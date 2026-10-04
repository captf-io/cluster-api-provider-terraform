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

package labels

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// Capacity labels.
const (
	CapacityLabel = "io.captf.capacity"
	NodeInfoLabel = "io.captf.node-info"
)

// ErrInvalidLabel reports a capacity label that is present but invalid.
var ErrInvalidLabel = errors.New("imageinspect: invalid label")

// architectures returns the node architectures ParseNodeInfo accepts:
// amd64, arm64, s390x and ppc64le.
func architectures() []infrav1.Architecture {
	return []infrav1.Architecture{infrav1.ArchitectureAmd64, infrav1.ArchitectureArm64, infrav1.ArchitectureS390x, infrav1.ArchitecturePpc64le}
}

// ParseCapacity parses label, the io.captf.capacity value: a JSON object of
// resource name to quantity string, each parsed with
// resource.ParseQuantity. An empty object declares nothing and is invalid.
// It returns the decoded resource list, or an error wrapping
// ErrInvalidLabel when label is not valid JSON, has an invalid resource
// name or quantity, or is empty.
func ParseCapacity(label string) (corev1.ResourceList, error) {
	var m map[string]string
	if err := strictJSON(label, &m); err != nil {
		return nil, fmt.Errorf("%w: %s is not a JSON object of quantity strings", ErrInvalidLabel, CapacityLabel)
	}
	if len(m) == 0 {
		return nil, fmt.Errorf("%w: %s is empty", ErrInvalidLabel, CapacityLabel)
	}
	out := corev1.ResourceList{}
	for k, v := range m {
		if errs := validation.IsQualifiedName(k); len(errs) > 0 {
			return nil, fmt.Errorf("%w: %s: %q is not a valid resource name: %s", ErrInvalidLabel, CapacityLabel, k, strings.Join(errs, "; "))
		}
		q, err := resource.ParseQuantity(v)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %q is not a quantity for %q", ErrInvalidLabel, CapacityLabel, v, k)
		}
		out[corev1.ResourceName(k)] = q
	}
	return out, nil
}

// ParseNodeInfo parses label, the io.captf.node-info value:
// {"architecture","operatingSystem"} with at least one key set; when
// present, architecture must be amd64, arm64, s390x or ppc64le and
// operatingSystem at most 64 characters. It returns the decoded NodeInfo,
// or an error wrapping ErrInvalidLabel when label is not valid JSON, sets
// neither field, or sets either to an invalid value.
func ParseNodeInfo(label string) (infrav1.NodeInfo, error) {
	var ni infrav1.NodeInfo
	if err := strictJSON(label, &ni); err != nil {
		return infrav1.NodeInfo{}, fmt.Errorf("%w: %s is not {\"architecture\",\"operatingSystem\"}", ErrInvalidLabel, NodeInfoLabel)
	}
	if ni == (infrav1.NodeInfo{}) {
		return infrav1.NodeInfo{}, fmt.Errorf("%w: %s sets neither architecture nor operatingSystem", ErrInvalidLabel, NodeInfoLabel)
	}
	if ni.Architecture != "" && !slices.Contains(architectures(), ni.Architecture) {
		return infrav1.NodeInfo{}, fmt.Errorf("%w: %s: unknown architecture %q", ErrInvalidLabel, NodeInfoLabel, ni.Architecture)
	}
	if len(ni.OperatingSystem) > 64 {
		return infrav1.NodeInfo{}, fmt.Errorf("%w: %s: operatingSystem longer than 64 characters", ErrInvalidLabel, NodeInfoLabel)
	}
	return ni, nil
}

// strictJSON decodes the JSON document s into v, using
// json.Decoder.DisallowUnknownFields so an unrecognized field is an error,
// and returns an error when s is malformed, has an unknown field, or has
// trailing data after the first value.
func strictJSON(s string, v any) error {
	dec := json.NewDecoder(bytes.NewReader([]byte(s)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("trailing data")
	}
	return nil
}
