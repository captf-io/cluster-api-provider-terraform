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

package jobs

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	"github.com/captf-io/cluster-api-provider-terraform/internal/runner"
)

// Result is the runner's result document; the type lives in
// internal/runner so the runner and the controller share one schema.
type Result = runner.Result

// Result errors.
var (
	// ErrNoResult means the source container has no termination message:
	// it has not terminated, or the pod is gone.
	ErrNoResult = errors.New("jobs: no runner result")
	// ErrResultTruncated means the message hit the 4096-byte cap and is not
	// complete JSON.
	ErrResultTruncated = errors.New("jobs: runner result truncated")
	// ErrResultInvalid means the message is not a result document.
	ErrResultInvalid = errors.New("jobs: runner result invalid")
)

// sourceStatus returns pod's source container status, and whether the
// container was found at all.
func sourceStatus(pod *corev1.Pod) (corev1.ContainerStatus, bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name == SourceContainer {
			return cs, true
		}
	}
	return corev1.ContainerStatus{}, false
}

// ParseResult reads the result from pod's source container's termination
// message, and returns it. It returns a non-nil error when the container
// has not terminated or reported no message (ErrNoResult), the message hit
// the byte cap (ErrResultTruncated), or it is not a valid result document of
// the current version (ErrResultInvalid).
func ParseResult(pod *corev1.Pod) (*Result, error) {
	cs, ok := sourceStatus(pod)
	if !ok || cs.State.Terminated == nil || strings.TrimSpace(cs.State.Terminated.Message) == "" {
		return nil, ErrNoResult
	}
	msg := cs.State.Terminated.Message
	var r Result
	if err := json.Unmarshal([]byte(msg), &r); err != nil {
		if len(msg) >= runner.MaxResultBytes {
			return nil, fmt.Errorf("%w: %d bytes", ErrResultTruncated, len(msg))
		}
		return nil, fmt.Errorf("%w: %w", ErrResultInvalid, err)
	}
	if r.Version != runner.ResultVersion {
		return nil, fmt.Errorf("%w: version %d", ErrResultInvalid, r.Version)
	}
	return &r, nil
}

// SourceStartedAt is when the pod's source container started, from its
// terminated or running state; false when the pod reports neither (it never
// started, or the status is gone).
func SourceStartedAt(pod *corev1.Pod) (time.Time, bool) {
	cs, ok := sourceStatus(pod)
	switch {
	case !ok:
	case cs.State.Terminated != nil && !cs.State.Terminated.StartedAt.IsZero():
		return cs.State.Terminated.StartedAt.Time, true
	case cs.State.Running != nil && !cs.State.Running.StartedAt.IsZero():
		return cs.State.Running.StartedAt.Time, true
	}
	return time.Time{}, false
}

// pullableDigest matches a pullable image reference's trailing
// @sha256:<digest>, as ImageDigest reads from imageID.
var pullableDigest = regexp.MustCompile(`@(sha256:[0-9a-f]{64})$`)

// ImageDigest returns the source container's image pinned to a digest, as
// repo@sha256:…, for the destroy Job of an immutable machine. The repository
// is the spec's own image (pod.Spec.Containers, never re-resolved from
// status), not imageID's: CRI runtimes can report imageID under a different
// repository that mirrors the same content (a registry mirror, a pull
// through a different name), and pinning that would make destroy pull from
// the wrong place. Only the digest comes from imageID
// (docker-pullable://repo@sha256:…, repo@sha256:…); a
// bare sha256:… (a local image config ID, not a pullable manifest digest) or
// a tag-only imageID yields false, and so does a source container with no
// image in the pod spec.
func ImageDigest(pod *corev1.Pod) (string, bool) {
	cs, ok := sourceStatus(pod)
	if !ok {
		return "", false
	}
	id := cs.ImageID
	if i := strings.Index(id, "://"); i >= 0 {
		id = id[i+3:]
	}
	m := pullableDigest.FindStringSubmatch(id)
	if m == nil {
		return "", false
	}
	spec := specImage(pod)
	if spec == "" {
		return "", false
	}
	return repository(spec) + "@" + m[1], true
}

// specImage returns the source container's image as given in the pod spec
// (spec.source.image, or repo@digest when already pinned), not its status.
func specImage(pod *corev1.Pod) string {
	for _, c := range pod.Spec.Containers {
		if c.Name == SourceContainer {
			return c.Image
		}
	}
	return ""
}

// repository returns ref with any tag and/or digest stripped from its
// final path segment, leaving host[:port]/namespace/repo. It handles
// repo:tag, repo@digest and repo:tag@digest alike, and never touches a
// host:port earlier in ref.
func repository(ref string) string {
	dir, last := "", ref
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		dir, last = ref[:i+1], ref[i+1:]
	}
	if i := strings.Index(last, "@"); i >= 0 {
		last = last[:i]
	}
	if i := strings.Index(last, ":"); i >= 0 {
		last = last[:i]
	}
	return dir + last
}

// pullFailureReasons are the waiting reasons of an image that cannot be
// pulled. InvalidImageName is included: such a pod never starts either.
var pullFailureReasons = []string{"ErrImagePull", "ImagePullBackOff", "InvalidImageName"}

// PullFailed reports whether an init or app container of the pod is stuck
// pulling its image.
func PullFailed(pod *corev1.Pod) bool {
	for _, statuses := range [][]corev1.ContainerStatus{pod.Status.InitContainerStatuses, pod.Status.ContainerStatuses} {
		for _, cs := range statuses {
			if w := cs.State.Waiting; w != nil && slices.Contains(pullFailureReasons, w.Reason) {
				return true
			}
		}
	}
	return false
}

// SourcePullFailed reports whether the source container of pod, the
// module image, is waiting because its image cannot be pulled, and with
// which waiting reason (one of ErrImagePull, ImagePullBackOff,
// InvalidImageName). An init container stuck pulling the runner image
// does not count: no other module image would fix it. It returns the
// reason and true, or "" and false.
func SourcePullFailed(pod *corev1.Pod) (reason string, ok bool) {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.Name != SourceContainer {
			continue
		}
		if w := cs.State.Waiting; w != nil && slices.Contains(pullFailureReasons, w.Reason) {
			return w.Reason, true
		}
	}
	return "", false
}

// SourceImage returns the image job's source container runs, as its pod
// template gives it; "" when it has none.
func SourceImage(job *batchv1.Job) string {
	for _, c := range job.Spec.Template.Spec.Containers {
		if c.Name == SourceContainer {
			return c.Image
		}
	}
	return ""
}
