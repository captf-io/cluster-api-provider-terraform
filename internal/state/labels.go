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

package state

import (
	"crypto/sha256"
	"encoding/hex"

	"k8s.io/apimachinery/pkg/labels"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
)

// Labels CAPTF passes to the backend's labels map. They come only from
// immutable identity: Terraform lists state with a selector that includes
// this whole map, so a changed map means "no state". Never add or change a
// key without a re-labeling migration.
const (
	OwnerKindLabel = "captf.infrastructure.cluster.x-k8s.io/owner-kind"
	OwnerNameLabel = "captf.infrastructure.cluster.x-k8s.io/owner-name"
	// ManagedLabel selects everything CAPTF owns (cache and sweep selector).
	ManagedLabel = "captf.io/managed"
)

// RetainedFromUIDLabel marks a Secret that deletionPolicy Retain kept when
// its object was deleted: a state chunk, a state backup or the durable
// inputs. Its value is the uid of the object that retained it. Such a
// Secret has no owner reference to that object, so it outlives it; an
// object of the same kind, namespace and name finds it again by its
// deterministic names and selectors, and adopts it only with
// spec.adoptRetainedState, which removes the label. The backend's
// selector matches a superset of its labels, so the label does not hide a
// state chunk from Terraform or OpenTofu.
const RetainedFromUIDLabel = "captf.io/retained-from-uid"

// RetainedFrom returns the uid of the object that retained a Secret with
// labels (RetainedFromUIDLabel), or "" when it was not retained.
func RetainedFrom(labels map[string]string) string {
	return labels[RetainedFromUIDLabel]
}

// Labels the backend sets itself on every state Secret and on the Lease.
const (
	BackendStateLabel     = "tfstate"
	BackendSuffixLabel    = "tfstateSecretSuffix"
	BackendWorkspaceLabel = "tfstateWorkspace"
	Workspace             = "default"
)

// InputsHashAnnotation records, on the base state Secret, the inputs hash of
// the last successful apply. It is an annotation, not a label: the value is
// "h1:" plus 64 hex characters, which is longer than a label value and
// contains a colon, and nothing selects on it.
const InputsHashAnnotation = "captf.io/inputs-hash"

// maxLabelValue is the Kubernetes label value limit.
const maxLabelValue = 63

// LabelValue returns name when it fits a label value, else the first 16
// hex characters of its sha256.
func LabelValue(name string) string {
	if len(name) <= maxLabelValue {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:])[:16]
}

// BackendLabels returns the labels map passed to the kubernetes backend for
// an object of ownerKind named ownerName, belonging to the Cluster
// clusterName. The backend copies it onto every state Secret it creates and
// onto the lock Lease.
func BackendLabels(ownerKind, ownerName, clusterName string) map[string]string {
	return map[string]string{
		OwnerKindLabel:                   ownerKind,
		OwnerNameLabel:                   LabelValue(ownerName),
		clusterv1.ClusterNameLabel:       LabelValue(clusterName),
		ManagedLabel:                     "true",
		clusterctlv1.ClusterctlMoveLabel: "",
	}
}

// Selector returns a selector matching every state Secret (all chunks) of
// suffix.
func Selector(suffix string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{
		BackendStateLabel:     "true",
		BackendSuffixLabel:    suffix,
		BackendWorkspaceLabel: Workspace,
	})
}
