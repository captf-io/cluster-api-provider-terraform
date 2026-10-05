/*
Copyright 2026 The CAPTF Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// This file emits the runner's real-time progress as events.k8s.io/v1
// Events on the Terraform* object that owns the Job. The runner must stay
// a small static binary (depguard: standard library, plus k8s.io/klog/v2
// for logging), so instead of client-go it POSTs the Event to the API
// server itself, with the pod's ServiceAccount token: the same in-cluster
// credentials the kubernetes state backend uses. Emission is best effort: a
// failure is logged once and never fails, or noticeably slows, the run.

package runner

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/strutil"
)

// Runner event reasons. Every one is listed in DocumentedEvents, and a test
// checks that each is emitted somewhere.
const (
	// EventRunStarted: the runtime is ready and the first step is about to
	// run (op, image reference, runtime version).
	EventRunStarted = "RunStarted"
	// EventStepStarted: a runtime step started.
	EventStepStarted = "StepStarted"
	// EventStepSucceeded: a runtime step finished successfully.
	EventStepSucceeded = "StepSucceeded"
	// EventStepFailed: a runtime step failed; the note carries the runner's
	// curated failure summary, never raw stderr.
	EventStepFailed = "StepFailed"
	// EventPlanSummary: a plan the runner parsed (drift, or a guarded
	// cluster apply): counts only.
	EventPlanSummary = "PlanSummary"
	// EventResourcesChanged: what an apply or destroy step changed, from the
	// runtime's summary line: counts only.
	EventResourcesChanged = "ResourcesChanged"
	// EventRunFinished: the run ended (result, total duration).
	EventRunFinished = "RunFinished"
)

// DocumentedEvents returns the runner's event reasons.
func DocumentedEvents() []string {
	return []string{
		EventRunStarted, EventStepStarted, EventStepSucceeded, EventStepFailed,
		EventPlanSummary, EventResourcesChanged, EventRunFinished,
	}
}

// Event types.
const (
	EventTypeNormal  = "Normal"
	EventTypeWarning = "Warning"
)

// ReportingController is the reportingController of every runner event.
const ReportingController = "captf.io/runner"

// Limits of the events.k8s.io/v1 API (k8s.io/kubernetes pkg/apis/core
// validation): the note and the reporting instance.
const (
	maxNote     = 1024
	maxInstance = 128
	// maxNamePrefix keeps "<name>.<hex>" within a DNS subdomain.
	maxNamePrefix = 200
)

// Emission tuning: each POST gets EventTimeout, and after MaxEventFailures
// consecutive failures the runner stops trying for the rest of the run.
const (
	EventTimeout     = 2 * time.Second
	MaxEventFailures = 3
)

// Recorder emits one event about the run. Implementations never block for
// long and never fail the run.
type Recorder interface {
	// Event emits one event using ctx, of eventType (EventTypeNormal or
	// EventTypeWarning), with reason, action and note as its fields.
	Event(ctx context.Context, eventType, reason, action, note string)
}

// ObjectRef names the object an event is about (regarding) or related to.
type ObjectRef struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Namespace  string `json:"namespace,omitempty"`
	Name       string `json:"name,omitempty"`
	UID        string `json:"uid,omitempty"`
}

// String returns r in the --event-object form:
// <apiVersion>/<kind>/<namespace>/<name>/<uid>.
func (r ObjectRef) String() string {
	return strings.Join([]string{r.APIVersion, r.Kind, r.Namespace, r.Name, r.UID}, "/")
}

// ParseObjectRef parses s, the --event-object form, and returns the
// resulting ObjectRef. The apiVersion may itself contain a slash
// (group/version), so the last four fields are taken from the right. It
// returns a non-nil error when s has too few fields or any field is empty.
func ParseObjectRef(s string) (ObjectRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) < 5 {
		return ObjectRef{}, fmt.Errorf("event object %q: want <apiVersion>/<kind>/<namespace>/<name>/<uid>", s)
	}
	n := len(parts)
	r := ObjectRef{
		APIVersion: strings.Join(parts[:n-4], "/"),
		Kind:       parts[n-4], Namespace: parts[n-3], Name: parts[n-2], UID: parts[n-1],
	}
	if r.APIVersion == "" || r.Kind == "" || r.Namespace == "" || r.Name == "" || r.UID == "" {
		return ObjectRef{}, fmt.Errorf("event object %q: every field must be set", s)
	}
	return r, nil
}

// InCluster locates the API server and the pod's ServiceAccount
// credentials, as client-go's rest.InClusterConfig does.
type InCluster struct {
	Host, Port        string
	TokenFile, CAFile string
}

// Default in-cluster credential paths.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	// DefaultTokenFile is the projected ServiceAccount token.
	DefaultTokenFile = serviceAccountDir + "/token"
	// DefaultCAFile is the cluster CA bundle.
	DefaultCAFile = serviceAccountDir + "/ca.crt"
)

// InClusterFromEnv returns the InCluster config read through getenv, from
// KUBERNETES_SERVICE_HOST and _PORT and the default credential paths.
func InClusterFromEnv(getenv func(string) string) InCluster {
	return InCluster{
		Host: getenv("KUBERNETES_SERVICE_HOST"), Port: getenv("KUBERNETES_SERVICE_PORT"),
		TokenFile: DefaultTokenFile, CAFile: DefaultCAFile,
	}
}

// NewEventRecorder returns the recorder for o, built from the in-cluster
// config ic, using ctx's logger: nil (no events) without --event-object,
// and nil after logging why the object reference or ic is unusable.
// instance is the reporting instance, the pod name.
func NewEventRecorder(ctx context.Context, o Options, ic InCluster, instance string) Recorder {
	if o.EventObject == "" {
		return nil
	}
	regarding, err := ParseObjectRef(o.EventObject)
	if err == nil {
		var rec *APIRecorder
		rec, err = newInClusterRecorder(ic, regarding, o.JobName, instance)
		if err == nil {
			return rec
		}
	}
	klog.FromContext(ctx).Info("Events disabled", "err", err)
	return nil
}

// newInClusterRecorder builds the APIRecorder for events about regarding,
// using ic's in-cluster credentials, jobName as the related Job (when set)
// and instance as the reporting instance. It returns a non-nil error when
// ic is not usable in a cluster or its CA file cannot be read.
func newInClusterRecorder(ic InCluster, regarding ObjectRef, jobName, instance string) (*APIRecorder, error) {
	if ic.Host == "" || ic.Port == "" {
		return nil, errors.New("not running in a cluster (KUBERNETES_SERVICE_HOST/PORT unset)")
	}
	ca, err := os.ReadFile(ic.CAFile)
	if err != nil {
		return nil, fmt.Errorf("read CA: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("no certificate in %s", ic.CAFile)
	}
	tokenFile := ic.TokenFile
	rec := &APIRecorder{
		BaseURL: "https://" + net.JoinHostPort(ic.Host, ic.Port),
		Client: &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			Proxy:           http.ProxyFromEnvironment,
		}},
		// Read on every event: the kubelet rotates the projected token.
		Token:     func() (string, error) { b, err := os.ReadFile(tokenFile); return strings.TrimSpace(string(b)), err }, // #nosec G304 -- the projected service account token path
		Regarding: regarding,
		Instance:  instance,
	}
	if jobName != "" {
		rec.Related = &ObjectRef{APIVersion: "batch/v1", Kind: "Job", Namespace: regarding.Namespace, Name: jobName}
	}
	return rec, nil
}

// APIRecorder creates events.k8s.io/v1 Events through the API server's REST
// API. It is safe for concurrent use.
type APIRecorder struct {
	// BaseURL is the API server, e.g. https://10.0.0.1:443.
	BaseURL string
	Client  *http.Client
	// Token returns the bearer token; nil sends none.
	Token func() (string, error)
	// Regarding is the Terraform* object; Related is its Job, if known.
	Regarding ObjectRef
	Related   *ObjectRef
	// Instance is the reporting instance (the pod name).
	Instance string
	// Timeout bounds each POST (EventTimeout when zero); MaxFailures is the
	// consecutive failures after which emission stops (MaxEventFailures
	// when zero).
	Timeout     time.Duration
	MaxFailures int

	mu       sync.Mutex
	failures int
	warned   bool
	disabled bool
	last     int64
}

var _ Recorder = &APIRecorder{}

// eventBody is the events.k8s.io/v1 Event the runner creates.
type eventBody struct {
	APIVersion          string     `json:"apiVersion"`
	Kind                string     `json:"kind"`
	Metadata            eventMeta  `json:"metadata"`
	EventTime           string     `json:"eventTime"`
	ReportingController string     `json:"reportingController"`
	ReportingInstance   string     `json:"reportingInstance"`
	Action              string     `json:"action"`
	Reason              string     `json:"reason"`
	Regarding           ObjectRef  `json:"regarding"`
	Related             *ObjectRef `json:"related,omitempty"`
	Note                string     `json:"note,omitempty"`
	Type                string     `json:"type"`
}

// eventMeta is the JSON metadata of an events.k8s.io/v1 Event.
type eventMeta struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// microTime is metav1.MicroTime's wire format.
const microTime = "2006-01-02T15:04:05.000000Z07:00"

// Event creates one event of eventType, with reason, action and note as
// its fields, using ctx's logger. Failures are counted, logged once, and
// stop emission after MaxFailures in a row; they never reach the run. The
// request outlives a canceled ctx (a SIGTERM is exactly when the
// interruption must still be reported), bounded by Timeout.
func (r *APIRecorder) Event(ctx context.Context, eventType, reason, action, note string) {
	logger := klog.FromContext(ctx)
	now, seq, ok := r.next()
	if !ok {
		return
	}
	err := r.post(ctx, now, seq, eventType, reason, action, note)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		r.failures = 0
		return
	}
	r.failures++
	if !r.warned {
		r.warned = true
		logger.Info("Emitting event failed; the run continues", "reason", reason, "err", err)
	}
	if !r.disabled && r.failures >= r.maxFailures() {
		r.disabled = true
		logger.Info("Events failed repeatedly; not emitting events for the rest of the run", "failures", r.failures)
	}
}

// maxFailures returns r.MaxFailures, or MaxEventFailures when it is unset.
func (r *APIRecorder) maxFailures() int {
	if r.MaxFailures > 0 {
		return r.MaxFailures
	}
	return MaxEventFailures
}

// next returns the event time and a strictly increasing sequence for the
// event's name (client-go names events <object>.<hex unix nanos>), or false
// once emission is disabled.
func (r *APIRecorder) next() (time.Time, int64, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disabled {
		return time.Time{}, 0, false
	}
	now := time.Now()
	n := now.UnixNano()
	if n <= r.last {
		n = r.last + 1
	}
	r.last = n
	return now, n, true
}

// post builds and sends, using ctx, the Event of eventType, reason, action
// and note, named from seq and timestamped now. It returns a non-nil error
// when marshaling, building the request or the POST itself fails, or the
// API server does not answer 2xx.
func (r *APIRecorder) post(ctx context.Context, now time.Time, seq int64, eventType, reason, action, note string) error {
	prefix := r.Regarding.Name
	if len(prefix) > maxNamePrefix {
		// A cut can end on a separator, which is invalid before the ".".
		prefix = strings.TrimRight(prefix[:maxNamePrefix], "-.")
	}
	ev := eventBody{
		APIVersion: "events.k8s.io/v1", Kind: "Event",
		Metadata:            eventMeta{Name: fmt.Sprintf("%s.%x", prefix, seq), Namespace: r.Regarding.Namespace},
		EventTime:           now.UTC().Format(microTime),
		ReportingController: ReportingController,
		ReportingInstance:   strutil.Truncate(r.Instance, maxInstance),
		Action:              strutil.Truncate(action, maxInstance),
		Reason:              reason,
		Regarding:           r.Regarding,
		Related:             r.Related,
		Note:                strutil.Truncate(note, maxNote),
		Type:                eventType,
	}
	body, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = EventTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	u := r.BaseURL + "/apis/events.k8s.io/v1/namespaces/" + url.PathEscape(r.Regarding.Namespace) + "/events"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if r.Token != nil {
		tok, err := r.Token()
		if err != nil {
			return fmt.Errorf("read token: %w", err)
		}
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
	}
	client := r.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode/100 != 2 {
		// The status only: the body could echo the request.
		return fmt.Errorf("API server answered %s", resp.Status)
	}
	return nil
}

// emit sends, using ctx, one event of eventType, reason and action, with
// note formatted from format and args, when o has a recorder.
func (o Options) emit(ctx context.Context, eventType, reason, action, format string, args ...any) {
	if o.Events == nil {
		return
	}
	o.Events.Event(ctx, eventType, reason, action, o.red.Redact(fmt.Sprintf(format, args...)))
}
