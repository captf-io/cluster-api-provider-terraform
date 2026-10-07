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
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
)

// TestSourcePullFailed proves SourcePullFailed reports the source
// container waiting on a pull, with its reason, and nothing else: not
// the runner init container, not another waiting reason, not a running
// source container.
func TestSourcePullFailed(t *testing.T) {
	t.Parallel()
	waiting := func(name, reason string) corev1.ContainerStatus {
		return corev1.ContainerStatus{Name: name, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: reason}}}
	}
	for _, tc := range []struct {
		name       string
		init, main []corev1.ContainerStatus
		want       string
	}{
		{"back-off", nil, []corev1.ContainerStatus{waiting(SourceContainer, "ImagePullBackOff")}, "ImagePullBackOff"},
		{"pull error", nil, []corev1.ContainerStatus{waiting(SourceContainer, "ErrImagePull")}, "ErrImagePull"},
		{"invalid name", nil, []corev1.ContainerStatus{waiting(SourceContainer, "InvalidImageName")}, "InvalidImageName"},
		{"init container", []corev1.ContainerStatus{waiting(RunnerContainer, "ImagePullBackOff")}, []corev1.ContainerStatus{waiting(SourceContainer, "PodInitializing")}, ""},
		{"other container", nil, []corev1.ContainerStatus{waiting("sidecar", "ImagePullBackOff")}, ""},
		{"creating", nil, []corev1.ContainerStatus{waiting(SourceContainer, "ContainerCreating")}, ""},
		{"running", nil, []corev1.ContainerStatus{{Name: SourceContainer, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}, ""},
	} {
		pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: tc.init, ContainerStatuses: tc.main}}
		if got, ok := SourcePullFailed(pod); got != tc.want || ok != (tc.want != "") {
			t.Errorf("%s: SourcePullFailed = %q, %v; want %q", tc.name, got, ok, tc.want)
		}
	}
}

// TestSourceImage proves SourceImage reads the source container's image
// from the pod template, and "" without one.
func TestSourceImage(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{}
	if got := SourceImage(job); got != "" {
		t.Errorf("no containers: %q", got)
	}
	job.Spec.Template.Spec.Containers = []corev1.Container{{Name: "other", Image: "x"}, {Name: SourceContainer, Image: "r@sha256:a"}}
	if got := SourceImage(job); got != "r@sha256:a" {
		t.Errorf("SourceImage = %q", got)
	}
}
