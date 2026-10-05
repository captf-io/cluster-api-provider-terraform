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
	"bufio"
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/health"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/objects"
)

// maxLogLines caps the manager log lines a health check reads.
const maxLogLines = 200000

// health is stage 7: the manager pod is the one setup saw, with no new
// restart; the manager's log since the suite started holds no panic,
// fatal or klog E/F line (health.DefaultFatal, no allowlist); and every
// CAPTF Job pod the tracker saw ended Succeeded, except the failing
// cluster's apply pods from before its variable was removed.
// It runs under ctx and fails t on any problem.
func (s *suite) health(ctx context.Context, t *testing.T) {
	mhint := s.kubectl("-n " + env.ManagerNamespace + " get pods -l " + managerSelector + " -o wide")
	m, err := s.currentManager(ctx)
	switch {
	case err != nil:
		t.Errorf("%v; inspect: %s", err, mhint)
	case m.uid != s.manager.uid:
		t.Errorf("expected the manager pod %s (uid %s) to run throughout, observed %s (uid %s): it was replaced during the run; inspect: %s",
			s.manager.name, s.manager.uid, m.name, m.uid, mhint)
	case m.restarts != s.manager.restarts:
		t.Errorf("expected manager pod %s to keep %d restarts, observed %d; inspect: %s", m.name, s.manager.restarts, m.restarts, mhint)
	}
	if err == nil {
		s.scanManagerLog(ctx, t, m.name)
	}

	s.tracker.stop()
	booked := s.succeededJobs(ctx, t)
	var bad []string
	succeeded := 0
	for _, r := range s.tracker.records() {
		switch {
		case r.phase == corev1.PodSucceeded:
			succeeded++
		case r.phase != corev1.PodFailed && booked[r.job]:
			// Deleted with its object before the tracker saw it finish;
			// the manager's JobSucceeded Event says it succeeded.
			succeeded++
		case r.kind == objects.KindTerraformCluster && r.owner == failName && r.op == "apply" && r.created.Before(s.failFixed) && r.phase == corev1.PodFailed:
			t.Logf("expected failure: pod %s of Job %s (the undeclared variable) ended %s", r.name, r.job, r.phase)
		default:
			bad = append(bad, fmt.Sprintf("%s (Job %s, %s %s %s) last seen %s %s", r.name, r.job, r.kind, r.owner, r.op, r.phase, r.detail))
		}
	}
	if len(bad) > 0 {
		t.Errorf("expected every CAPTF Job pod in %s to end Succeeded (but the deliberate failure's), observed:\n  %s\ninspect: the Job objects in the diagnostics bundle's objects/jobs",
			s.ns, strings.Join(bad, "\n  "))
	}
	t.Logf("%d CAPTF Job pods ended Succeeded", succeeded)
	if succeeded == 0 {
		t.Errorf("expected the tracker to have seen CAPTF Job pods in %s, observed none", s.ns)
	}
}

// scanManagerLog fails t when the log of every container of the manager
// pod name since the suite started has a line health.DefaultFatal
// matches.
// It reads the log under ctx.
func (s *suite) scanManagerLog(ctx context.Context, t *testing.T, name string) {
	t.Helper()
	pod, err := s.c.Kube.CoreV1().Pods(env.ManagerNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		t.Errorf("get manager pod %s: %v", name, err)
		return
	}
	since := metav1.NewTime(s.start)
	fatal := health.DefaultFatal()
	for _, c := range pod.Spec.Containers {
		rc, err := s.c.Kube.CoreV1().Pods(env.ManagerNamespace).GetLogs(name, &corev1.PodLogOptions{Container: c.Name, SinceTime: &since}).Stream(ctx)
		if err != nil {
			t.Errorf("read the log of manager container %s/%s: %v", name, c.Name, err)
			continue
		}
		var hits []string
		lines := 0
		sc := bufio.NewScanner(rc)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() && lines < maxLogLines {
			lines++
			line := sc.Text()
			if s.earlierRunEvent(line) {
				continue
			}
			for _, re := range fatal {
				if re.MatchString(line) {
					hits = append(hits, truncate(line, 300))
					break
				}
			}
		}
		_ = rc.Close()
		if len(hits) > 0 {
			t.Errorf("expected no panic, fatal or E/F line in manager container %s/%s since %s, observed %d:\n  %s\ninspect: %s",
				name, c.Name, s.start.Format("15:04:05"), len(hits), strings.Join(hits, "\n  "),
				s.kubectl(fmt.Sprintf("-n %s logs %s -c %s --since-time=%s", env.ManagerNamespace, name, c.Name, since.UTC().Format("2006-01-02T15:04:05Z"))))
		}
		t.Logf("manager container %s: %d log lines since %s scanned", c.Name, lines, s.start.Format("15:04:05"))
	}
}

// succeededJobs returns the names of the Jobs the manager reported
// successful in the run's namespace: the Job named by each JobSucceeded
// Event ("<op> Job <name> succeeded ..."), listed under ctx from both
// Event APIs. A list error is reported through t.
func (s *suite) succeededJobs(ctx context.Context, t *testing.T) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	add := func(reason, msg string) {
		if reason != "JobSucceeded" {
			return
		}
		if f := strings.Fields(msg); len(f) >= 3 && f[1] == "Job" {
			out[f[2]] = true
		}
	}
	if list, err := s.c.Kube.EventsV1().Events(s.ns).List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("list events.k8s.io Events in %s: %v", s.ns, err)
	} else {
		for i := range list.Items {
			add(list.Items[i].Reason, list.Items[i].Note)
		}
	}
	if list, err := s.c.Kube.CoreV1().Events(s.ns).List(ctx, metav1.ListOptions{}); err != nil {
		t.Errorf("list core Events in %s: %v", s.ns, err)
	} else {
		for i := range list.Items {
			add(list.Items[i].Reason, list.Items[i].Message)
		}
	}
	return out
}

// rejectedEvent matches client-go's events broadcaster refusing to write
// an Event into a namespace that no longer exists, and captures the
// namespace.
var rejectedEvent = regexp.MustCompile(`"Server rejected event \(will not retry!\)" err="namespaces \\"(e2e-noop-[0-9a-f]{6})\\" not found"`)

// earlierRunEvent reports whether line is the manager's only expected E
// line: client-go's events broadcaster (event_broadcaster.go) failing to
// update an Event series in the namespace of an earlier noop run, which
// that run's cleanup deleted. The broadcaster flushes a series minutes
// after its last Event, so the line can land in the next run's window.
// It is never accepted for this run's own namespace.
func (s *suite) earlierRunEvent(line string) bool {
	m := rejectedEvent.FindStringSubmatch(line)
	return len(m) == 2 && m[1] != s.ns
}

// truncate returns s cut to n bytes with an ellipsis when longer.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
