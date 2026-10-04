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

package webhooks

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/topology"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// Shared template rules: spec.template.spec is immutable, except for
// ClusterClass server-side-apply dry-runs, and spec.template.metadata
// must be valid labels and annotations.
//
// templateSpecPath and templateMetadataPath are the field.Path roots of
// spec.template.spec and spec.template.metadata, shared by every *Template
// kind's validation.
var (
	templateSpecPath     = field.NewPath("spec", "template", "spec")
	templateMetadataPath = field.NewPath("spec", "template", "metadata")
)

// validateTemplateMetadata validates the optional template metadata meta.
// It returns the field errors found, or nil when meta is nil or valid.
func validateTemplateMetadata(meta *clusterv1.ObjectMeta) field.ErrorList {
	if meta == nil {
		return nil
	}
	return meta.Validate(templateMetadataPath)
}

// skipImmutability reports whether the update to newObj, bounded by ctx, is
// a ClusterClass topology dry-run, for which the immutability check does
// not apply. It also returns an error when the admission request cannot be
// read from ctx.
func skipImmutability(ctx context.Context, newObj client.Object) (bool, error) {
	req, err := admission.RequestFromContext(ctx)
	if err != nil {
		return false, apierrors.NewBadRequest(fmt.Sprintf("expected an admission.Request inside context: %v", err))
	}
	return topology.IsDryRunRequest(req, newObj), nil
}
