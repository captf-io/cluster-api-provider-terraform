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

package wait

import (
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// webhookName is the name of the CAPTF TerraformCluster validating
// webhook, as the API server quotes it in a denial.
const webhookName = "validation.terraformcluster.infrastructure.cluster.x-k8s.io"

// webhookValidation is a message only the webhook's own validation
// produces: the CRD schema cannot check an image reference's syntax.
const webhookValidation = "not a valid image reference"

// deploymentReady reports whether d is Available and fully rolled out: the
// controller has seen the latest generation, every replica is updated and
// available, and no old replica remains. It returns true when ready, else
// false and a short reason.
func deploymentReady(d *appsv1.Deployment) (bool, string) {
	want := int32(1)
	if d.Spec.Replicas != nil {
		want = *d.Spec.Replicas
	}
	if d.Status.ObservedGeneration < d.Generation {
		return false, "generation not yet observed"
	}
	available := false
	for _, c := range d.Status.Conditions {
		if c.Type == appsv1.DeploymentAvailable {
			available = c.Status == corev1.ConditionTrue
		}
	}
	switch {
	case !available:
		return false, "not Available"
	case d.Status.UpdatedReplicas < want:
		return false, fmt.Sprintf("%d/%d replicas updated", d.Status.UpdatedReplicas, want)
	case d.Status.Replicas > d.Status.UpdatedReplicas:
		return false, fmt.Sprintf("%d old replicas pending termination", d.Status.Replicas-d.Status.UpdatedReplicas)
	case d.Status.AvailableReplicas < want:
		return false, fmt.Sprintf("%d/%d replicas available", d.Status.AvailableReplicas, want)
	}
	return true, ""
}

// crdEstablished reports whether the CustomResourceDefinition u has the
// condition Established=True. It returns true when established, else false
// and a short reason.
func crdEstablished(u *unstructured.Unstructured) (bool, string) {
	conds, _, _ := unstructured.NestedSlice(u.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if !ok || m["type"] != "Established" {
			continue
		}
		if m["status"] == "True" {
			return true, ""
		}
		return false, "Established=" + fmt.Sprint(m["status"])
	}
	return false, "no Established condition yet"
}

// classifyProbe interprets the result of the server-side dry-run Create of
// the invalid TerraformCluster. err is that Create's error (nil when the
// object was accepted). It returns true only when the rejection came from
// the CAPTF webhook; otherwise false and a short reason. An accepted
// object, a connection or "failed calling webhook" error, and a rejection
// by anything other than the webhook (such as the CRD schema, or a missing
// CRD) all mean the webhook is not serving yet.
func classifyProbe(err error) (bool, string) {
	if err == nil {
		return false, "invalid TerraformCluster was accepted: webhook not enforcing yet"
	}
	msg := err.Error()
	if strings.Contains(msg, "failed calling webhook") {
		return false, "webhook unreachable: " + msg
	}
	if !apierrors.IsInvalid(err) && !apierrors.IsForbidden(err) && !apierrors.IsBadRequest(err) {
		return false, "probe failed: " + msg
	}
	if strings.Contains(msg, webhookName) || strings.Contains(msg, webhookValidation) {
		return true, ""
	}
	return false, "rejected, but not by the CAPTF webhook: " + msg
}
