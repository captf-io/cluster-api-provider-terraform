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

package health

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

// progressEvery is how often Stable prints a progress line at the least.
const progressEvery = 30 * time.Second

// podSnap is what Stable remembers of one pod.
type podSnap struct {
	// uid is the pod's UID.
	uid types.UID
	// phase is the pod's phase.
	phase corev1.PodPhase
	// ready is true when the pod is Running with condition Ready=True.
	ready bool
	// restarts maps each container (init containers too) to its restart
	// count.
	restarts map[string]int32
	// job is true when a Job owns the pod.
	job bool
}

// snapshot maps "namespace/name" to the pod's snap.
type snapshot map[string]podSnap

// takeSnapshot builds and returns a snapshot from pods.
func takeSnapshot(pods []corev1.Pod) snapshot {
	s := make(snapshot, len(pods))
	for i := range pods {
		p := &pods[i]
		sp := podSnap{uid: p.UID, phase: p.Status.Phase, restarts: map[string]int32{}}
		for _, c := range p.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue && p.Status.Phase == corev1.PodRunning {
				sp.ready = true
			}
		}
		for _, cs := range p.Status.InitContainerStatuses {
			sp.restarts[cs.Name] = cs.RestartCount
		}
		for _, cs := range p.Status.ContainerStatuses {
			sp.restarts[cs.Name] = cs.RestartCount
		}
		for _, o := range p.OwnerReferences {
			if o.Kind == "Job" {
				sp.job = true
			}
		}
		s[p.Namespace+"/"+p.Name] = sp
	}
	return s
}

// diffSnapshots compares a later snapshot cur against the baseline base.
// It returns the violations (restarts, recreated or deleted pods, pods
// gone not-ready) and the additions (new pods other than completed ones),
// each sorted. Both empty means stable.
func diffSnapshots(base, cur snapshot) (violations, additions []string) {
	for key, b := range base {
		c, ok := cur[key]
		switch {
		case !ok:
			if b.phase != corev1.PodSucceeded {
				violations = append(violations, fmt.Sprintf("pod %s: deleted (uid %s)", key, b.uid))
			}
			continue
		case c.uid != b.uid:
			violations = append(violations, fmt.Sprintf("pod %s: recreated (uid %s -> %s)", key, b.uid, c.uid))
			continue
		}
		for name, was := range b.restarts {
			if now := c.restarts[name]; now > was {
				violations = append(violations, fmt.Sprintf("pod %s: container %s restarted (restarts %d -> %d)", key, name, was, now))
			}
		}
		if b.ready && !c.ready && c.phase != corev1.PodSucceeded {
			violations = append(violations, fmt.Sprintf("pod %s: went not ready (phase %s)", key, c.phase))
		}
	}
	for key, c := range cur {
		if _, ok := base[key]; ok {
			continue
		}
		if c.phase == corev1.PodSucceeded {
			continue
		}
		additions = append(additions, fmt.Sprintf("pod %s: added (uid %s, phase %s)", key, c.uid, c.phase))
	}
	sort.Strings(violations)
	sort.Strings(additions)
	return violations, additions
}

// Stable checks, with the clients c, that the pods of namespaces (all
// namespaces when empty) hold still for window. It takes a baseline snapshot, then re-samples
// every interval until window ends, and fails on any restart-count
// increase, any pod recreated or deleted, any pod going not-ready, and any
// added pod other than a Succeeded one (additions are reported
// separately). It prints a progress line to out at the start, at least
// every 30 seconds and at the end. It returns nil when the window passes
// clean, else an error listing every violation of the first bad sample,
// or the context error when ctx ends first.
func Stable(ctx context.Context, c wait.Clients, namespaces []string, window, interval time.Duration, out io.Writer) error {
	return stable(ctx, c, namespaces, window, interval, out, progressEvery)
}

// stable is Stable with the progress period progress made explicit, so
// tests can shorten it. Its arguments ctx, c, namespaces, window, interval
// and out are Stable's, and it returns Stable's result.
func stable(ctx context.Context, c wait.Clients, namespaces []string, window, interval time.Duration, out io.Writer, progress time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("health: stable: interval must be positive, got %s", interval)
	}
	pods, err := listPods(ctx, c, namespaces)
	if err != nil {
		return fmt.Errorf("health: stable baseline: %w", err)
	}
	base := takeSnapshot(pods)
	start := time.Now()
	deadline := start.Add(window)
	lastPrint := start
	fmt.Fprintf(out, "stable: baseline %d pods, watching for %s (every %s)\n", len(base), window, interval)
	for {
		wake := time.Now().Add(interval)
		if wake.After(deadline) {
			wake = deadline
		}
		timer := time.NewTimer(time.Until(wake))
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("health: stable: %w", ctx.Err())
		case <-timer.C:
		}
		pods, err := listPods(ctx, c, namespaces)
		if err != nil {
			return fmt.Errorf("health: stable sample: %w", err)
		}
		violations, additions := diffSnapshots(base, takeSnapshot(pods))
		if len(violations) > 0 || len(additions) > 0 {
			var b strings.Builder
			fmt.Fprintf(&b, "health: cluster not stable after %s:", time.Since(start).Round(time.Second))
			for _, v := range violations {
				b.WriteString("\n  " + v)
			}
			for _, a := range additions {
				b.WriteString("\n  " + a)
			}
			return fmt.Errorf("%s", b.String())
		}
		now := time.Now()
		if !now.Before(deadline) {
			fmt.Fprintf(out, "stable: %s elapsed, %d pods unchanged: stable\n", window, len(base))
			return nil
		}
		if now.Sub(lastPrint) >= progress {
			fmt.Fprintf(out, "stable: %s/%s elapsed, %d pods unchanged\n", now.Sub(start).Round(time.Second), window, len(base))
			lastPrint = now
		}
	}
}
