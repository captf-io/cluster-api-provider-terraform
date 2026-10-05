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

package kubewait

import (
	"context"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Options tunes a wait. The zero value is usable: zero durations take the
// defaults and a nil Out discards the progress output.
type Options struct {
	// Interval is the time between checks. Default 2s.
	Interval time.Duration
	// Timeout bounds the whole wait. Default 3m.
	Timeout time.Duration
	// ReportEvery is the time between progress lines. Default 30s.
	ReportEvery time.Duration
	// Out receives the progress lines. Nil discards them.
	Out io.Writer
}

// withDefaults returns o with zero fields replaced by the defaults.
func (o Options) withDefaults() Options {
	if o.Interval <= 0 {
		o.Interval = 2 * time.Second
	}
	if o.Timeout <= 0 {
		o.Timeout = 3 * time.Minute
	}
	if o.ReportEvery <= 0 {
		o.ReportEvery = 30 * time.Second
	}
	if o.Out == nil {
		o.Out = io.Discard
	}
	return o
}

// check is one poll step: it returns whether the wait is over and a
// one-line observation.
type check func(ctx context.Context) (done bool, observed string)

// poll runs step at once and then every o.Interval until it is done, the
// timeout passes or ctx is done, printing the observation every
// o.ReportEvery. desc names the wait. It returns nil when done and, else, an
// error carrying desc and the last observation.
func poll(ctx context.Context, desc string, o Options, step check) error {
	o = o.withDefaults()
	ctx, cancel := context.WithTimeout(ctx, o.Timeout)
	defer cancel()
	start := time.Now()
	lastReport := start
	ticker := time.NewTicker(o.Interval)
	defer ticker.Stop()
	for {
		done, observed := step(ctx)
		if done {
			fmt.Fprintf(o.Out, "kubewait: %s: done after %s\n", desc, time.Since(start).Round(time.Millisecond))
			return nil
		}
		if ctx.Err() == nil && time.Since(lastReport) >= o.ReportEvery {
			lastReport = time.Now()
			fmt.Fprintf(o.Out, "kubewait: %s: %s (waiting %s)\n", desc, observed, time.Since(start).Round(time.Second))
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("kubewait: %s: not met after %s, last observed: %s: %w", desc, time.Since(start).Round(time.Millisecond), observed, ctx.Err())
		case <-ticker.C:
		}
	}
}

// resource returns the resource client of dyn for gvr, namespaced when ns
// is not empty.
func resource(dyn dynamic.Interface, gvr schema.GroupVersionResource, ns string) dynamic.ResourceInterface {
	if ns == "" {
		return dyn.Resource(gvr)
	}
	return dyn.Resource(gvr).Namespace(ns)
}

// Get returns the object name of gvr in ns (empty for a cluster-scoped
// kind), read through dyn using ctx. A missing object is an error wrapping
// the NotFound API error.
func Get(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	u, err := resource(dyn, gvr, ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("kubewait: get %s %s/%s: %w", gvr.Resource, ns, name, err)
	}
	return u, nil
}

// Until polls the object name of gvr in ns through dyn until pred accepts
// it, and returns the accepted object. pred returns whether the object
// satisfies the wait and an observation, which is printed in progress lines
// and carried by the timeout error together with desc. A missing object or
// a read error is the observation and is retried. o sets the timing and the
// progress output; ctx cancels the wait.
func Until(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name, desc string,
	pred func(*unstructured.Unstructured) (bool, string), o Options) (*unstructured.Unstructured, error) {
	var last *unstructured.Unstructured
	err := poll(ctx, fmt.Sprintf("%s %s/%s %s", gvr.Resource, ns, name, desc), o, func(ctx context.Context) (bool, string) {
		u, err := resource(dyn, gvr, ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, "not found"
		}
		if err != nil {
			return false, "get failed: " + err.Error()
		}
		ok, observed := pred(u)
		if ok {
			last = u
		}
		return ok, observed
	})
	if err != nil {
		return nil, err
	}
	return last, nil
}

// ConditionStatus waits until status.conditions of the object name of gvr in
// ns, read through dyn, holds a condition of type condType whose status is wantStatus and,
// when wantReason is not empty, whose reason is wantReason. It returns the
// object; o and ctx are as for Until.
func ConditionStatus(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name,
	condType, wantStatus, wantReason string, o Options) (*unstructured.Unstructured, error) {
	desc := fmt.Sprintf("condition %s=%s", condType, wantStatus)
	if wantReason != "" {
		desc += " reason " + wantReason
	}
	return Until(ctx, dyn, gvr, ns, name, desc, func(u *unstructured.Unstructured) (bool, string) {
		conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
		for _, c := range conds {
			m, _ := c.(map[string]any)
			if m["type"] != condType {
				continue
			}
			status, _ := m["status"].(string)
			reason, _ := m["reason"].(string)
			msg, _ := m["message"].(string)
			observed := fmt.Sprintf("%s=%s reason=%q message=%q", condType, status, reason, msg)
			return status == wantStatus && (wantReason == "" || reason == wantReason), observed
		}
		return false, fmt.Sprintf("condition %s absent (%d conditions)", condType, len(conds))
	}, o)
}

// FieldEquals waits until the field at path of the object name of gvr in ns,
// read through dyn, deep-equals want, comparing the unstructured value
// (unstructured.NestedFieldNoCopy), so integers must be int64 and
// lists []any. It returns the object; o and ctx are as for Until.
func FieldEquals(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string,
	path []string, want any, o Options) (*unstructured.Unstructured, error) {
	desc := fmt.Sprintf("field %s == %v", strings.Join(path, "."), want)
	return Until(ctx, dyn, gvr, ns, name, desc, func(u *unstructured.Unstructured) (bool, string) {
		got, found, err := unstructured.NestedFieldNoCopy(u.Object, path...)
		if err != nil {
			return false, "field unreadable: " + err.Error()
		}
		if !found {
			return false, "field absent"
		}
		return reflect.DeepEqual(got, want), fmt.Sprintf("field = %v", got)
	}, o)
}

// Gone waits until the object name of gvr in ns no longer exists, read
// through dyn. It returns nil once a read reports NotFound; o and ctx are as
// for Until, and other read errors are retried.
func Gone(ctx context.Context, dyn dynamic.Interface, gvr schema.GroupVersionResource, ns, name string, o Options) error {
	return poll(ctx, fmt.Sprintf("%s %s/%s gone", gvr.Resource, ns, name), o, func(ctx context.Context) (bool, string) {
		u, err := resource(dyn, gvr, ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return true, "gone"
		}
		if err != nil {
			return false, "get failed: " + err.Error()
		}
		if ts := u.GetDeletionTimestamp(); ts != nil {
			return false, fmt.Sprintf("terminating since %s, finalizers %v", ts.Format(time.RFC3339), u.GetFinalizers())
		}
		return false, "still exists"
	})
}
