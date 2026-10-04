/*
Copyright 2026 The cluster-api-provider-terraform Authors.

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

package health

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// WarningEvents checks that no Warning event of namespaces (all
// namespaces when empty) happened after since, other than those allow
// excuses. An event's time is the latest of its lastTimestamp, eventTime
// and series.lastObservedTime; an allow pattern is matched against
// "reason: message". It lists events with c under ctx. It returns nil when there are none, else an error
// listing every such event.
func WarningEvents(ctx context.Context, c wait.Clients, namespaces []string, since time.Time, allow []*regexp.Regexp) error {
	var events []corev1.Event
	for _, ns := range nsOrAll(namespaces) {
		list, err := c.Kube.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "type=" + corev1.EventTypeWarning})
		if err != nil {
			return fmt.Errorf("health: list events in %q: %w", ns, err)
		}
		events = append(events, list.Items...)
	}
	if bad := evalEvents(events, since, allow); len(bad) > 0 {
		return fmt.Errorf("health: warning events since %s:\n  %s", since.UTC().Format(time.RFC3339), strings.Join(bad, "\n  "))
	}
	return nil
}

// eventTime returns the latest time recorded on e, or the zero time.
func eventTime(e *corev1.Event) time.Time {
	t := e.LastTimestamp.Time
	if e.EventTime.Time.After(t) {
		t = e.EventTime.Time
	}
	if e.Series != nil && e.Series.LastObservedTime.Time.After(t) {
		t = e.Series.LastObservedTime.Time
	}
	return t
}

// evalEvents is the pure event check. It returns one sorted message per
// Warning event of events newer than since that no allow pattern excuses.
func evalEvents(events []corev1.Event, since time.Time, allow []*regexp.Regexp) []string {
	var out []string
	for i := range events {
		e := &events[i]
		if e.Type != corev1.EventTypeWarning {
			continue
		}
		t := eventTime(e)
		if !t.After(since) {
			continue
		}
		if anyMatch(allow, e.Reason+": "+e.Message) {
			continue
		}
		out = append(out, fmt.Sprintf("%s %s/%s %s %s: %s (x%d)", t.UTC().Format(time.RFC3339), e.Namespace, e.InvolvedObject.Name, e.InvolvedObject.Kind, e.Reason, e.Message, e.Count))
	}
	sort.Strings(out)
	return out
}
