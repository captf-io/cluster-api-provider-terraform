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

package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
)

// Kinds of a variablesFrom source, as messages and the source index name
// them.
const (
	kindConfigMap = "ConfigMap"
	kindSecret    = "Secret"
)

// VariablesSpec is the variables part of a Terraform* spec.
type VariablesSpec struct {
	// Inline is spec.variables.
	Inline runtime.RawExtension
	// From is spec.variablesFrom.
	From []infrav1.VariablesSource
}

// ResolveVariables merges an object's user variables: the variablesFrom
// sources in list order, a later one winning on the same key, then the
// inline variables over all of them. A variable is sensitive when its
// winning value came from a Secret. Sources are read through r, which
// must be uncached (Deps.APIReader): the manager's cache holds no
// ConfigMap data.
//
// A missing or unlabeled source that is not optional is a
// VariablesSourceNotFound gate; a bad key or value is VariablesInvalid.
// Messages name the source and the key, never a value. nil variables and a
// nil gate mean the object sets none.
//
// ctx bounds the source reads; namespace is the object's namespace, role
// its contract role (for name validation) and spec its variablesFrom and
// inline variables. It returns the merged variables, a gate when a source
// blocks resolution, and any read or parse error.
func ResolveVariables(ctx context.Context, r client.Reader, namespace string, role contract.Role, spec VariablesSpec) (contract.Variables, *Gate, error) {
	vars := contract.Variables{}
	for i, src := range spec.From {
		s, gate, err := loadSource(ctx, r, namespace, i, src)
		if err != nil || gate != nil {
			return nil, gate, err
		}
		if s == nil {
			continue
		}
		if gate := s.mergeInto(vars, role, src.Format); gate != nil {
			return nil, gate, nil
		}
	}
	if len(spec.Inline.Raw) > 0 {
		inline, err := contract.ParseVariables(spec.Inline.Raw)
		if err != nil {
			return nil, variablesInvalid("spec.variables: " + err.Error()), nil
		}
		for _, name := range slices.Sorted(maps.Keys(inline)) {
			if err := contract.ValidateVariableName(role, name); err != nil {
				return nil, variablesInvalid("spec.variables: " + nameProblem(err)), nil
			}
			vars[name] = contract.Variable{Value: inline[name]}
		}
	}
	if len(vars) == 0 {
		return nil, nil, nil
	}
	return vars, nil, nil
}

// sourceData is the data of one labeled variablesFrom source.
type sourceData struct {
	// what names the source in messages: "Secret db (spec.variablesFrom[1])".
	what      string
	data      map[string][]byte
	sensitive bool
}

// loadSource reads src, the i-th variablesFrom entry in namespace, using
// ctx and the reader r. It returns the source's data, nil source and nil
// gate for an optional source that is missing or unlabeled, a gate for a
// required one, or any read error.
func loadSource(ctx context.Context, r client.Reader, namespace string, i int, src infrav1.VariablesSource) (*sourceData, *Gate, error) {
	kind, name := kindConfigMap, src.ConfigMapRef.Name
	var obj client.Object = &corev1.ConfigMap{}
	if src.SecretRef.Name != "" {
		kind, name, obj = kindSecret, src.SecretRef.Name, &corev1.Secret{}
	}
	what := fmt.Sprintf("%s %s (spec.variablesFrom[%d])", kind, name, i)
	optional := src.Optional != nil && *src.Optional
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, obj)
	switch {
	case apierrors.IsNotFound(err):
		if optional {
			return nil, nil, nil
		}
		return nil, sourceNotFound(what + " not found"), nil
	case err != nil:
		return nil, nil, fmt.Errorf("get %s %s: %w", kind, name, err)
	}
	if obj.GetLabels()[infrav1.VariablesSourceLabel] != "true" {
		if optional {
			return nil, nil, nil
		}
		return nil, sourceNotFound(what + " does not carry the label " + infrav1.VariablesSourceLabel + "=true"), nil
	}
	s := &sourceData{what: what, data: map[string][]byte{}}
	switch o := obj.(type) {
	case *corev1.Secret:
		s.sensitive = true
		for k, v := range o.Data {
			s.data[k] = v
		}
	case *corev1.ConfigMap:
		for k, v := range o.Data {
			s.data[k] = []byte(v)
		}
		for k, v := range o.BinaryData {
			s.data[k] = v
		}
	}
	return s, nil, nil
}

// mergeInto adds the source's variables to vars, replacing earlier ones,
// validating each name against role and decoding each value per format
// (raw string, or JSON when format is VariablesFormatJSON). It returns a
// gate when a name or value is invalid, else nil.
func (s *sourceData) mergeInto(vars contract.Variables, role contract.Role, format infrav1.VariablesFormat) *Gate {
	// Sorted, so the first bad key reported is stable across reconciles.
	for _, key := range slices.Sorted(maps.Keys(s.data)) {
		value := s.data[key]
		if err := contract.ValidateVariableName(role, key); err != nil {
			return variablesInvalid(s.what + ": " + nameProblem(err))
		}
		if !utf8.Valid(value) {
			return variablesInvalid(fmt.Sprintf("%s: the value of key %q is not UTF-8", s.what, key))
		}
		var v json.RawMessage
		if format == infrav1.VariablesFormatJSON {
			if !json.Valid(value) {
				return variablesInvalid(fmt.Sprintf("%s: the value of key %q is not valid JSON (format JSON)", s.what, key))
			}
			v = json.RawMessage(value)
		} else {
			b, err := json.Marshal(string(value))
			if err != nil {
				return variablesInvalid(fmt.Sprintf("%s: the value of key %q cannot be encoded", s.what, key))
			}
			v = b
		}
		vars[key] = contract.Variable{Value: v, Sensitive: s.sensitive}
	}
	return nil
}

// nameProblem returns err's message without the package prefix.
func nameProblem(err error) string {
	return strings.TrimPrefix(err.Error(), "contract: ")
}

// sourceNotFound returns a VariablesSourceNotFound gate with msg.
func sourceNotFound(msg string) *Gate {
	return &Gate{Status: metav1.ConditionFalse, Reason: infrav1.VariablesSourceNotFoundReason, Message: msg}
}

// variablesInvalid returns a VariablesInvalid gate with msg.
func variablesInvalid(msg string) *Gate {
	return &Gate{Status: metav1.ConditionFalse, Reason: infrav1.VariablesInvalidReason, Message: msg}
}

// VariablesSourceWatches returns the ConfigMap and Secret watches on d's
// VariablesCache that enqueue what mapFn maps a labeled source to; none
// without the cache (unit tests).
func VariablesSourceWatches(d Deps, mapFn handler.MapFunc) []source.TypedSource[reconcile.Request] {
	if d.VariablesCache == nil {
		return nil
	}
	h := handler.EnqueueRequestsFromMapFunc(mapFn)
	return []source.TypedSource[reconcile.Request]{
		source.Kind[client.Object](d.VariablesCache, &corev1.ConfigMap{}, h),
		source.Kind[client.Object](d.VariablesCache, &corev1.Secret{}, h),
	}
}

// VariablesSourceKeys returns the VariablesSourceIndex values of from:
// "ConfigMap/<name>" and "Secret/<name>".
func VariablesSourceKeys(from []infrav1.VariablesSource) []string {
	var out []string
	for _, src := range from {
		switch {
		case src.SecretRef.Name != "":
			out = append(out, kindSecret+"/"+src.SecretRef.Name)
		case src.ConfigMapRef.Name != "":
			out = append(out, kindConfigMap+"/"+src.ConfigMapRef.Name)
		}
	}
	return out
}
