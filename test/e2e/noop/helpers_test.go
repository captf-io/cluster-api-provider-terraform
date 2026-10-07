//go:build e2e

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

package noop

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kubewait"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/tfstate"
)

// Polling cadence of eventually.
const (
	// pollInterval is the time between checks.
	pollInterval = 2 * time.Second
	// progressEvery is the longest gap between progress lines.
	progressEvery = 30 * time.Second
	// pullGrace is how long a Job pod may report a failed image pull
	// before the suite gives up on it: a missing manifest never appears,
	// and waiting for the 600s Job deadline only delays the failure.
	pullGrace = 45 * time.Second
)

// Owner labels and other product names the suite reads (internal/state,
// internal/inputs, internal/identity, internal/rbac).
const (
	// stateLabel marks every Terraform state Secret.
	stateLabel = "tfstate"
	// imageDigestAnnotation is the applied inputs Secret's pinned
	// repo@digest.
	imageDigestAnnotation = "captf.io/image-digest"
	// tfvarsKey is an inputs Secret's rendered variables.
	tfvarsKey = "terraform.tfvars.json"
	// mainTFKey is an inputs Secret's rendered root module.
	mainTFKey = "main.tf.json"
	// runnerName names the runner ServiceAccount and RoleBinding.
	runnerName = "captf-runner"
	// mirrorPrefix starts the credential mirror's name.
	mirrorPrefix = "captf-creds-"
	// sourceContainer is the Job pod's container that runs the module
	// image.
	sourceContainer = "source"
)

// kindShort maps a CAPTF kind to the short name in its Secret and Job
// names.
var kindShort = map[string]string{
	objects.KindTerraformCluster:     "c",
	objects.KindTerraformMachine:     "m",
	objects.KindTerraformMachinePool: "mp",
}

// gvrOf maps a CAPTF kind to its resource.
var gvrOf = map[string]schema.GroupVersionResource{
	objects.KindTerraformCluster:     objects.TerraformClusterGVR,
	objects.KindTerraformMachine:     objects.TerraformMachineGVR,
	objects.KindTerraformMachinePool: objects.TerraformMachinePoolGVR,
}

// resourceOf maps a CAPTF kind to its kubectl resource name.
var resourceOf = map[string]string{
	objects.KindTerraformCluster:     "terraformcluster",
	objects.KindTerraformMachine:     "terraformmachine",
	objects.KindTerraformMachinePool: "terraformmachinepool",
}

// permanentError marks a check failure that waiting cannot fix (a failed
// apply, an image that cannot be pulled), so eventually stops at once.
type permanentError struct {
	// err is the failure.
	err error
}

// Error returns the wrapped error's message.
func (p permanentError) Error() string { return p.err.Error() }

// Unwrap returns the wrapped error.
func (p permanentError) Unwrap() error { return p.err }

// permanent wraps err so eventually gives up on it immediately. It
// returns nil for a nil err.
func permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err: err}
}

// eventually runs check under ctx every pollInterval until it returns
// nil, returns a permanent error, or timeout passes, logging progress
// through t at least every progressEvery. what names the condition in the
// log and the error. It returns nil once check passes, else an error
// carrying check's last failure.
func eventually(ctx context.Context, t testing.TB, what string, timeout time.Duration, check func(context.Context) error) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	lastLog := start
	var last error
	for {
		err := check(ctx)
		if err == nil {
			t.Logf("%s: ok after %s", what, time.Since(start).Round(time.Millisecond))
			return nil
		}
		if pe := (permanentError{}); errors.As(err, &pe) {
			return fmt.Errorf("%s: %w", what, pe.err)
		}
		if ctx.Err() == nil || last == nil {
			last = err
		}
		if time.Since(lastLog) >= progressEvery {
			t.Logf("%s: still waiting after %s: %v", what, time.Since(start).Round(time.Second), last)
			lastLog = time.Now()
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%s: not reached within %s; last observed: %w", what, timeout, last)
		case <-time.After(pollInterval):
		}
	}
}

// logWriter adapts a test's log to io.Writer, one Log call per write, for
// the framework helpers that print progress.
type logWriter struct {
	// t receives the lines.
	t testing.TB
}

// Write logs p without its trailing newline and returns len(p) and nil.
func (w logWriter) Write(p []byte) (int, error) {
	w.t.Helper()
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

// waitOpts returns kubewait.Options with timeout that log through t.
func waitOpts(t testing.TB, timeout time.Duration) kubewait.Options {
	return kubewait.Options{Interval: pollInterval, ReportEvery: progressEvery, Timeout: timeout, Out: logWriter{t: t}}
}

// randomSuffix returns six random lowercase hex digits for the run's
// namespace and identity.
func randomSuffix() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%06x", b)
}

// resource returns the client of gvr in ns from dyn, cluster-scoped when
// ns is empty.
func resource(dyn dynamic.Interface, gvr schema.GroupVersionResource, ns string) dynamic.ResourceInterface {
	if ns == "" {
		return dyn.Resource(gvr)
	}
	return dyn.Resource(gvr).Namespace(ns)
}

// create creates u through the dynamic client of gvr in its namespace
// under ctx, failing t with hint when the API server refuses it.
func (s *suite) create(ctx context.Context, t *testing.T, gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	t.Helper()
	if _, err := resource(s.c.Dynamic, gvr, u.GetNamespace()).Create(ctx, u, metav1.CreateOptions{}); err != nil {
		t.Fatalf("create %s %s: expected the API server (and the CAPTF or CAPI webhook) to admit it, observed: %v", u.GetKind(), qualified(u.GetNamespace(), u.GetName()), err)
	}
}

// get returns the object name of gvr in the run's namespace, read under
// ctx, failing t when it cannot be read.
func (s *suite) get(ctx context.Context, t *testing.T, gvr schema.GroupVersionResource, name string) *unstructured.Unstructured {
	t.Helper()
	u, err := resource(s.c.Dynamic, gvr, s.ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get %s %s/%s: %v", gvr.Resource, s.ns, name, err)
	}
	return u
}

// deleteObject deletes the object name of gvr in the run's namespace
// under ctx with foreground propagation off (the default), failing t on
// any error but NotFound.
func (s *suite) deleteObject(ctx context.Context, t *testing.T, gvr schema.GroupVersionResource, name string) {
	t.Helper()
	if err := ignoreNotFound(resource(s.c.Dynamic, gvr, s.ns).Delete(ctx, name, metav1.DeleteOptions{})); err != nil {
		t.Fatalf("delete %s %s/%s: expected the API server (and the webhooks) to allow it, observed: %v", gvr.Resource, s.ns, name, err)
	}
}

// qualified returns "ns/name", or name alone when ns is empty.
func qualified(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}

// ignoreNotFound returns err unless it is a NotFound API error.
func ignoreNotFound(err error) error {
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// field returns the value at the dotted path in u (status.source.image),
// and whether it is there.
func field(u *unstructured.Unstructured, path string) (any, bool) {
	v, found, err := unstructured.NestedFieldNoCopy(u.Object, strings.Split(path, ".")...)
	return v, found && err == nil
}

// isTrue reports whether v is the boolean true.
func isTrue(v any) bool {
	b, ok := v.(bool)
	return ok && b
}

// str returns the string at the dotted path in u, "" when absent.
func str(u *unstructured.Unstructured, path string) string {
	v, _ := field(u, path)
	s, _ := v.(string)
	return s
}

// canonical returns v rendered as JSON for comparison and messages, so
// int and int64 and float64, and map key order, do not matter. An
// unencodable value renders as its %v form.
func canonical(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// sameJSON reports whether got and want encode to the same JSON.
func sameJSON(got, want any) bool {
	return canonical(got) == canonical(want)
}

// expectField fails t unless the value at path in u, the object what,
// equals want as JSON. hint says where to look.
func expectField(t *testing.T, what string, u *unstructured.Unstructured, path string, want any, hint string) {
	t.Helper()
	got, found := field(u, path)
	if !found {
		t.Errorf("%s: expected %s = %s, observed the field absent; inspect: %s", what, path, canonical(want), hint)
		return
	}
	if !sameJSON(got, want) {
		t.Errorf("%s: expected %s = %s, observed %s; inspect: %s", what, path, canonical(want), canonical(got), hint)
	}
}

// expectEqual fails t unless got equals want as JSON. what names the
// value and hint says where to look.
func expectEqual(t *testing.T, what string, got, want any, hint string) {
	t.Helper()
	if !sameJSON(got, want) {
		t.Errorf("%s: expected %s, observed %s; inspect: %s", what, canonical(want), canonical(got), hint)
	}
}

// fieldCheck is one expected value at a dotted path.
type fieldCheck struct {
	// path is the dotted field path.
	path string
	// want is the expected value, compared as JSON.
	want any
}

// fieldsMatch returns nil when every one of checks holds on u, else an
// error listing each mismatch.
func fieldsMatch(u *unstructured.Unstructured, checks []fieldCheck) error {
	var bad []string
	for _, c := range checks {
		got, found := field(u, c.path)
		switch {
		case !found:
			bad = append(bad, fmt.Sprintf("%s absent (want %s)", c.path, canonical(c.want)))
		case !sameJSON(got, c.want):
			bad = append(bad, fmt.Sprintf("%s = %s (want %s)", c.path, canonical(got), canonical(c.want)))
		}
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return nil
}

// waitFields waits under ctx, up to timeout, until every one of checks
// holds on the object name of gvr in the run's namespace (CAPI copies
// values on its own schedule), failing t with what it last observed and
// hint otherwise.
func (s *suite) waitFields(ctx context.Context, t *testing.T, gvr schema.GroupVersionResource, name string, checks []fieldCheck, timeout time.Duration, hint string) {
	t.Helper()
	err := eventually(ctx, t, fmt.Sprintf("%s %s fields", gvr.Resource, name), timeout, func(ctx context.Context) error {
		u, err := resource(s.c.Dynamic, gvr, s.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		return fieldsMatch(u, checks)
	})
	if err != nil {
		t.Fatalf("%s %s/%s: expected the values below, observed: %v; inspect: %s", gvr.Resource, s.ns, name, err, hint)
	}
}

// cond is a condition expectation: type, status and, when not empty,
// reason.
type cond struct {
	// typ is the condition type.
	typ string
	// status is the expected status.
	status string
	// reason is the expected reason; empty accepts any.
	reason string
}

// condition returns the status, reason and message of the condition typ
// in u's status.conditions, and whether it exists.
func condition(u *unstructured.Unstructured, typ string) (status, reason, message string, found bool) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != typ {
			continue
		}
		status, _ = m["status"].(string)
		reason, _ = m["reason"].(string)
		message, _ = m["message"].(string)
		return status, reason, message, true
	}
	return "", "", "", false
}

// conditionsSummary renders every condition of u as "Type=Status
// (Reason: message)", for failure messages. It returns "no conditions"
// when there are none.
func conditionsSummary(u *unstructured.Unstructured) string {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	var parts []string
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok {
			parts = append(parts, fmt.Sprintf("%v=%v (%v: %v)", m["type"], m["status"], m["reason"], m["message"]))
		}
	}
	if len(parts) == 0 {
		return "no conditions"
	}
	return strings.Join(parts, "; ")
}

// conditionsMatch returns nil when u holds every condition in want, else
// an error naming the first mismatch and every condition observed.
func conditionsMatch(u *unstructured.Unstructured, want []cond) error {
	for _, w := range want {
		status, reason, _, found := condition(u, w.typ)
		if !found || status != w.status || (w.reason != "" && reason != w.reason) {
			exp := w.typ + "=" + w.status
			if w.reason != "" {
				exp += " (" + w.reason + ")"
			}
			return fmt.Errorf("expected %s, observed %s", exp, conditionsSummary(u))
		}
	}
	return nil
}

// waitConditions waits under ctx, up to timeout, until the object name of
// gvr in the run's namespace holds every condition in want, failing t
// with what it observed and hint otherwise. It returns the object.
func (s *suite) waitConditions(ctx context.Context, t *testing.T, gvr schema.GroupVersionResource, name string, want []cond, timeout time.Duration, hint string) *unstructured.Unstructured {
	t.Helper()
	var last *unstructured.Unstructured
	err := eventually(ctx, t, fmt.Sprintf("%s %s conditions", gvr.Resource, name), timeout, func(ctx context.Context) error {
		u, err := resource(s.c.Dynamic, gvr, s.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		last = u
		return conditionsMatch(u, want)
	})
	if err != nil {
		t.Fatalf("%s %s/%s: %v; inspect: %s", gvr.Resource, s.ns, name, err, hint)
	}
	return last
}

// failedApplyReasons are the ApplyJobSucceeded reasons of an apply that
// failed for good: waiting for provisioned is pointless after them.
var failedApplyReasons = []string{
	"ApplyFailed", "ImagePullFailed", "JobDeadlineExceeded", "ImageInvalid",
	"InputsTooLarge", "JobPolicyInvalid", "IdentityNotAllowed",
}

// hint returns the kubectl command that shows the CAPTF object kind name
// and its Jobs, for failure messages.
func (s *suite) hint(kind, name string) string {
	return s.kubectlNS(fmt.Sprintf("get %s %s -o yaml; kubectl -n %s get jobs,pods -l %s=%s,%s=%s",
		resourceOf[kind], name, s.ns, kubewait.OwnerKindLabel, kind, kubewait.OwnerNameLabel, name))
}

// waitApplied waits under ctx, up to timeout, until the CAPTF object kind
// name is
// provisioned with ApplyJobSucceeded=True. It fails t at once when the
// apply failed for good (failedApplyReasons) or when one of the object's
// apply Job pods has failed to pull its image for longer than pullGrace,
// with the pull error, so a broken image fails in under a minute rather
// than at the Job deadline. It returns the object.
func (s *suite) waitApplied(ctx context.Context, t *testing.T, kind, name string, timeout time.Duration) *unstructured.Unstructured {
	t.Helper()
	gvr := gvrOf[kind]
	var last *unstructured.Unstructured
	pullSince := map[string]time.Time{}
	err := eventually(ctx, t, fmt.Sprintf("%s %s provisioned", kind, name), timeout, func(ctx context.Context) error {
		u, err := resource(s.c.Dynamic, gvr, s.ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		last = u
		status, reason, msg, _ := condition(u, "ApplyJobSucceeded")
		if status == "False" && slices.Contains(failedApplyReasons, reason) {
			return permanent(fmt.Errorf("expected the apply to succeed, observed ApplyJobSucceeded=False (%s: %s); lastRun.error.summary %q",
				reason, msg, str(u, "status.lastRun.error.summary")))
		}
		if err := s.pullStuck(ctx, kind, name, "apply", pullSince); err != nil {
			return permanent(err)
		}
		provisioned, _ := field(u, "status.initialization.provisioned")
		if !isTrue(provisioned) || status != "True" {
			return fmt.Errorf("provisioned=%v, ApplyJobSucceeded=%s (%s: %s)", provisioned, orNone(status), reason, msg)
		}
		return nil
	})
	if err != nil {
		cs := "object never read"
		if last != nil {
			cs = conditionsSummary(last)
		}
		t.Fatalf("%v\nconditions: %s\ninspect: %s", err, cs, s.hint(kind, name))
	}
	return last
}

// orNone returns v, or "<none>" when v is empty.
func orNone(v string) string {
	if v == "" {
		return "<none>"
	}
	return v
}

// pullStuck returns an error when a pod of an op Job of the CAPTF object
// kind name has reported a failed image pull (ErrImagePull,
// ImagePullBackOff, InvalidImageName, ErrImageNeverPull) for longer than
// pullGrace; since remembers when each pod was first seen failing. It
// lists under ctx, and a list error is not a failure.
func (s *suite) pullStuck(ctx context.Context, kind, name, op string, since map[string]time.Time) error {
	pods, err := s.c.Kube.CoreV1().Pods(s.ns).List(ctx, metav1.ListOptions{LabelSelector: kubewait.JobsFor(kind, name, op).String()})
	if err != nil {
		return nil
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		for _, cs := range append(slices.Clone(p.Status.InitContainerStatuses), p.Status.ContainerStatuses...) {
			w := cs.State.Waiting
			if w == nil || !slices.Contains([]string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull"}, w.Reason) {
				continue
			}
			first, ok := since[p.Name]
			if !ok {
				since[p.Name] = time.Now()
				continue
			}
			if time.Since(first) >= pullGrace {
				return fmt.Errorf("expected %s Job pod %s to run, observed container %q unable to pull image %s for %s: %s: %s (the Job's %ds deadline would end it as ImagePullFailed)",
					op, p.Name, cs.Name, cs.Image, time.Since(first).Round(time.Second), w.Reason, w.Message, jobDeadline)
			}
		}
	}
	return nil
}

// jobComplete waits under ctx, up to timeout, for the newest Job of the
// CAPTF object
// kind name running op, created at or after after, to finish, and fails t
// unless it completed. It returns the Job. It suits Jobs that outlive
// their wait; a destroy Job goes with its object within seconds, so
// checkDestroyed reads the pod tracker and the Events instead.
func (s *suite) jobComplete(ctx context.Context, t *testing.T, kind, name, op string, after time.Time, timeout time.Duration) *batchv1.Job {
	t.Helper()
	sel := kubewait.JobsFor(kind, name, op).String()
	var job *batchv1.Job
	pullSince := map[string]time.Time{}
	err := eventually(ctx, t, fmt.Sprintf("%s Job of %s %s finished", op, kind, name), timeout, func(ctx context.Context) error {
		list, err := s.c.Kube.BatchV1().Jobs(s.ns).List(ctx, metav1.ListOptions{LabelSelector: sel})
		if err != nil {
			return err
		}
		var newest *batchv1.Job
		for i := range list.Items {
			j := &list.Items[i]
			if j.CreationTimestamp.Time.Before(after.Truncate(time.Second)) {
				continue
			}
			if newest == nil || j.CreationTimestamp.After(newest.CreationTimestamp.Time) ||
				(j.CreationTimestamp.Equal(&newest.CreationTimestamp) && j.Name > newest.Name) {
				newest = j
			}
		}
		if newest == nil {
			return fmt.Errorf("no %s Job created since %s (%d older)", op, after.Format(time.RFC3339), len(list.Items))
		}
		if err := s.pullStuck(ctx, kind, name, op, pullSince); err != nil {
			return permanent(err)
		}
		for _, c := range newest.Status.Conditions {
			if c.Status != corev1.ConditionTrue {
				continue
			}
			switch c.Type {
			case batchv1.JobComplete:
				job = newest.DeepCopy()
				return nil
			case batchv1.JobFailed:
				return permanent(fmt.Errorf("expected Job %s to complete, observed it Failed (%s: %s)", newest.Name, c.Reason, c.Message))
			}
		}
		return fmt.Errorf("job %s running (active %d)", newest.Name, newest.Status.Active)
	})
	if err != nil {
		t.Fatalf("%v; inspect: %s", err, s.hint(kind, name))
	}
	return job
}

// jobImage returns the image of job's source container, "" when it has
// none.
func jobImage(job *batchv1.Job) string {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == sourceContainer {
			return c.Image
		}
	}
	return ""
}

// expectEvents fails t unless, within timeout under ctx, an Event about
// the object name in the run's namespace carries each of reasons. hint
// says where to look.
func (s *suite) expectEvents(ctx context.Context, t *testing.T, name string, timeout time.Duration, hint string, reasons ...string) {
	t.Helper()
	for _, r := range reasons {
		if err := kubewait.EventSeen(ctx, s.c.Kube, s.ns, name, r, waitOpts(t, timeout)); err != nil {
			t.Errorf("expected an Event with reason %s about %s: %v; inspect: %s", r, name, err, hint)
		}
	}
}

// expectWarningEvent fails t, with hint, unless within timeout under ctx
// a Warning Event about the object name in the run's namespace has
// reason. It reads core and events.k8s.io Events.
func (s *suite) expectWarningEvent(ctx context.Context, t *testing.T, name, reason string, timeout time.Duration, hint string) {
	t.Helper()
	err := eventually(ctx, t, fmt.Sprintf("Warning event %s about %s", reason, name), timeout, func(ctx context.Context) error {
		var seen []string
		if list, err := s.c.Kube.EventsV1().Events(s.ns).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range list.Items {
				e := &list.Items[i]
				if e.Regarding.Name == name && e.Reason == reason {
					if e.Type == corev1.EventTypeWarning {
						return nil
					}
					seen = append(seen, e.Type)
				}
			}
		}
		if list, err := s.c.Kube.CoreV1().Events(s.ns).List(ctx, metav1.ListOptions{}); err == nil {
			for i := range list.Items {
				e := &list.Items[i]
				if e.InvolvedObject.Name == name && e.Reason == reason {
					if e.Type == corev1.EventTypeWarning {
						return nil
					}
					seen = append(seen, e.Type)
				}
			}
		}
		return fmt.Errorf("no Warning %s event about %s (types seen for that reason: %v)", reason, name, seen)
	})
	if err != nil {
		t.Errorf("%v; inspect: %s", err, hint)
	}
}

// applied returns the applied inputs Secret captf-applied-<short>-<name>
// of the CAPTF object kind name (the inputs of its last successful
// apply, with the digest it ran), and its decoded terraform.tfvars.json,
// read under ctx. It fails t when the Secret or the key is missing or the
// JSON is invalid.
func (s *suite) applied(ctx context.Context, t *testing.T, kind, name string) (*corev1.Secret, map[string]any) {
	t.Helper()
	secretName := "captf-applied-" + kindShort[kind] + "-" + name
	sec, err := s.c.Kube.CoreV1().Secrets(s.ns).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("expected the applied inputs Secret %s/%s of %s %s: %v; inspect: %s", s.ns, secretName, kind, name, err, s.kubectlNS("get secrets"))
	}
	for _, k := range []string{tfvarsKey, mainTFKey} {
		if len(sec.Data[k]) == 0 {
			t.Errorf("applied inputs Secret %s/%s: expected a non-empty %s key, observed keys %v", s.ns, secretName, k, keys(sec.Data))
		}
	}
	var vars map[string]any
	if err := json.Unmarshal(sec.Data[tfvarsKey], &vars); err != nil {
		t.Fatalf("applied inputs Secret %s/%s: %s is not a JSON object: %v", s.ns, secretName, tfvarsKey, err)
	}
	return sec, vars
}

// keys returns the sorted keys of m.
func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// readState returns the Terraform state of the CAPTF object kind name,
// read under ctx, failing t when it cannot be read.
func (s *suite) readState(ctx context.Context, t *testing.T, kind, name string) *tfstate.State {
	t.Helper()
	suffix := tfstate.SuffixFor(s.ns, kind, name)
	st, err := tfstate.Read(ctx, s.c.Kube, s.ns, suffix)
	if err != nil {
		t.Fatalf("expected the state of %s %s in Secret %s/%s: %v; inspect: %s", kind, name, s.ns, tfstate.SecretName(suffix), err, s.kubectlNS("get secrets -l "+stateLabel+"=true"))
	}
	return st
}

// secretsLeft returns the sorted names of the Secrets of the run's
// namespace, listed under ctx, that match selector (may be empty) and
// whose names start with one of prefixes (none means any), or the list
// error.
func (s *suite) secretsLeft(ctx context.Context, selector string, prefixes ...string) ([]string, error) {
	list, err := s.c.Kube.CoreV1().Secrets(s.ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, err
	}
	var out []string
	for i := range list.Items {
		n := list.Items[i].Name
		if len(prefixes) == 0 || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(n, p) }) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}
