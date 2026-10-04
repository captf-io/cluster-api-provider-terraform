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

package imageinspect

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect/labels"
)

// Capacity labels. The parsers live in internal/imageinspect/labels, which
// tfcapi-lint shares.
const (
	CapacityLabel = labels.CapacityLabel
	NodeInfoLabel = labels.NodeInfoLabel
)

// ErrInvalidLabel reports a capacity label that is present but invalid.
var ErrInvalidLabel = labels.ErrInvalidLabel

// ParseCapacity is labels.ParseCapacity: it parses label, the
// io.captf.capacity value, and returns the decoded resource list or an
// error wrapping ErrInvalidLabel.
func ParseCapacity(label string) (corev1.ResourceList, error) { return labels.ParseCapacity(label) }

// ParseNodeInfo is labels.ParseNodeInfo: it parses label, the
// io.captf.node-info value, and returns the decoded NodeInfo or an error
// wrapping ErrInvalidLabel.
func ParseNodeInfo(label string) (infrav1.NodeInfo, error) { return labels.ParseNodeInfo(label) }

// Resolution is what the labels resolve to.
type Resolution struct {
	Capacity  corev1.ResourceList
	NodeInfo  infrav1.NodeInfo
	Condition metav1.Condition
}

// Resolve maps imageLabels, an image's config labels, to the
// TerraformMachineTemplate CapacityResolved condition: neither label →
// True/CapacityNotDeclared with both fields unset; every present label
// valid → True/CapacityResolved; a present but invalid label →
// False/CapacityLabelInvalid with that field unset and a valid other
// label still set. It returns the resulting Resolution.
func Resolve(imageLabels map[string]string) Resolution {
	var r Resolution
	capLabel, hasCap := imageLabels[CapacityLabel]
	niLabel, hasNI := imageLabels[NodeInfoLabel]
	if !hasCap && !hasNI {
		r.Condition = metav1.Condition{
			Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionTrue, Reason: infrav1.CapacityNotDeclaredReason,
			Message: "The image declares neither " + CapacityLabel + " nor " + NodeInfoLabel,
		}
		return r
	}
	var errs []error
	if hasCap {
		c, err := ParseCapacity(capLabel)
		r.Capacity = c
		errs = append(errs, err)
	}
	if hasNI {
		ni, err := ParseNodeInfo(niLabel)
		r.NodeInfo = ni
		errs = append(errs, err)
	}
	if err := kerrors.NewAggregate(errs); err != nil {
		r.Condition = metav1.Condition{
			Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionFalse, Reason: infrav1.CapacityLabelInvalidReason, Message: err.Error(),
		}
		return r
	}
	r.Condition = metav1.Condition{Type: infrav1.CapacityResolvedCondition, Status: metav1.ConditionTrue, Reason: infrav1.CapacityResolvedReason}
	return r
}
