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
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/wait"
)

const (
	// MaxLogBytes caps the log bytes ScanLogs keeps per container log
	// (current or previous): the last 5 MiB. Earlier output is dropped
	// unscanned.
	MaxLogBytes = 5 << 20
	// maxLogTailLines is the TailLines ScanLogs asks the kubelet for, so a
	// chatty container does not stream its whole history.
	maxLogTailLines = 50000
	// maxLineLen truncates a reported log line.
	maxLineLen = 300
)

// Rules says which log lines fail ScanLogs.
type Rules struct {
	// Fatal are the patterns that make a line a finding.
	Fatal []*regexp.Regexp
	// Allow are the patterns that excuse a line a Fatal pattern matched.
	Allow []*regexp.Regexp
}

// DefaultFatal returns the patterns every component is held to: Go
// "panic:" and "fatal error:" lines, and klog error or fatal lines
// (^E0102 15:04:05 style).
func DefaultFatal() []*regexp.Regexp {
	return []*regexp.Regexp{
		regexp.MustCompile(`panic:`),
		regexp.MustCompile(`fatal error:`),
		regexp.MustCompile(`^[EF]\d{4} `),
	}
}

// logOpener opens the log of one container of a pod. previous selects the
// previous instance. It returns a stream to read, or an error.
type logOpener func(ctx context.Context, namespace, pod, container string, previous bool) (io.ReadCloser, error)

// ScanLogs fetches, with the clients c and the context ctx, the logs of
// every container of each pod of namespace
// matching labelSelector (an empty selector matches all): the current log,
// plus the previous one when the container's restartCount is above zero.
// Each line r.Fatal matches and no r.Allow pattern matches is a finding,
// truncated to 300 bytes. At most MaxLogBytes (the last 5 MiB) of each log
// is scanned. It returns nil when there are no findings, else an error
// listing all of them. A selector that matches no pods is an error, so a wrong
// selector cannot pass silently.
func ScanLogs(ctx context.Context, c wait.Clients, namespace, labelSelector string, r Rules) error {
	list, err := c.Kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return fmt.Errorf("health: list pods in %s for logs: %w", namespace, err)
	}
	open := func(ctx context.Context, ns, pod, container string, previous bool) (io.ReadCloser, error) {
		tail := int64(maxLogTailLines)
		return c.Kube.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{Container: container, Previous: previous, TailLines: &tail}).Stream(ctx)
	}
	return scanLogs(ctx, list.Items, labelSelector, open, r)
}

// scanLogs is ScanLogs over pods already listed (selector is used only in
// messages), reading logs through open with ctx and judging lines by r. It
// returns nil when clean, else an error listing the findings.
func scanLogs(ctx context.Context, pods []corev1.Pod, selector string, open logOpener, r Rules) error {
	if len(pods) == 0 {
		return fmt.Errorf("health: scan logs: no pods match selector %q", selector)
	}
	var findings []string
	for i := range pods {
		p := &pods[i]
		restarts := map[string]int32{}
		var names []string
		for _, cs := range p.Status.InitContainerStatuses {
			restarts[cs.Name] = cs.RestartCount
		}
		for _, cs := range p.Status.ContainerStatuses {
			restarts[cs.Name] = cs.RestartCount
		}
		for _, ct := range p.Spec.InitContainers {
			names = append(names, ct.Name)
		}
		for _, ct := range p.Spec.Containers {
			names = append(names, ct.Name)
		}
		for _, name := range names {
			prev := []bool{false}
			if restarts[name] > 0 {
				prev = append(prev, true)
			}
			for _, previous := range prev {
				label := fmt.Sprintf("pod %s/%s container %s", p.Namespace, p.Name, name)
				if previous {
					label += " (previous)"
				}
				rc, err := open(ctx, p.Namespace, p.Name, name, previous)
				if err != nil {
					findings = append(findings, fmt.Sprintf("%s: cannot read logs: %v", label, err))
					continue
				}
				lines, err := tailLines(rc, MaxLogBytes)
				_ = rc.Close()
				if err != nil {
					findings = append(findings, fmt.Sprintf("%s: reading logs: %v", label, err))
				}
				for _, l := range matchLines(lines, r) {
					findings = append(findings, fmt.Sprintf("%s: %s", label, l))
				}
			}
		}
	}
	if len(findings) > 0 {
		return fmt.Errorf("health: fatal log lines:\n  %s", strings.Join(findings, "\n  "))
	}
	return nil
}

// tailLines reads rd to its end keeping only the last limit bytes, and
// returns them split into lines. When bytes were dropped, the first line
// (probably partial) is dropped too. A read error returns the lines read
// so far and the error.
func tailLines(rd io.Reader, limit int) ([]string, error) {
	var buf bytes.Buffer
	truncated := false
	chunk := make([]byte, 32*1024)
	var rerr error
	for {
		n, err := rd.Read(chunk)
		if n > 0 {
			buf.Write(chunk[:n])
			if buf.Len() > limit {
				truncated = true
				b := buf.Bytes()
				keep := append([]byte(nil), b[len(b)-limit:]...)
				buf.Reset()
				buf.Write(keep)
			}
		}
		if err != nil {
			if err != io.EOF {
				rerr = err
			}
			break
		}
	}
	var lines []string
	sc := bufio.NewScanner(&buf)
	sc.Buffer(make([]byte, 0, 64*1024), limit+1)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if truncated && len(lines) > 0 {
		lines = lines[1:]
	}
	return lines, rerr
}

// matchLines returns the lines of lines that some r.Fatal pattern matches
// and no r.Allow pattern matches, each truncated.
func matchLines(lines []string, r Rules) []string {
	var out []string
	for _, l := range lines {
		if !anyMatch(r.Fatal, l) || anyMatch(r.Allow, l) {
			continue
		}
		if len(l) > maxLineLen {
			l = l[:maxLineLen] + "..."
		}
		out = append(out, l)
	}
	return out
}

// anyMatch reports whether any of res matches s.
func anyMatch(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
