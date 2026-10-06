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

package shared

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/klog/v2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/imageinspect"
	"github.com/captf-io/cluster-api-provider-terraform/internal/varschema"
)

// maxSchemaProblems is how many problems a VariablesInvalid message names;
// the rest are counted.
const maxSchemaProblems = 3

// SchemaProblems validates vars, the merged user variables of an object,
// against schema, the module image's variables schema, and returns one
// message per problem (never a value); nil when schema is nil or vars is
// acceptable. A value from a string-format variablesFrom source
// (contract.Variable.Lenient) is checked leniently.
func SchemaProblems(schema *varschema.Schema, vars contract.Variables) []string {
	if schema == nil {
		return nil
	}
	in := make(map[string]varschema.Value, len(vars))
	for name, v := range vars {
		in[name] = varschema.Value{JSON: json.RawMessage(v.Value), Lenient: v.Lenient}
	}
	return schema.Validate(in)
}

// VariablesSchemaGate checks vars, the object's merged user variables,
// against the variables schema of image, read through d's SchemaCache and
// Inspector with the pull Secrets named by pullSecrets in namespace, and
// returns a VariablesInvalid gate naming up to three problems, or nil when
// the variables are acceptable. It also returns nil, validating nothing,
// when d has no cache or inspector (unit tests), when the image declares
// no schema (the label is optional), or when the image cannot be read or
// its label is invalid: the Job's own pull then reports a bad image, and
// tfcapi-lint reports a bad label. ctx bounds the image read; d supplies
// the cache, the inspector and the reader of the pull Secrets in
// namespace; image is the spec image reference; vars are the merged
// variables.
func VariablesSchemaGate(ctx context.Context, d Deps, namespace, image string, pullSecrets []string, vars contract.Variables) *Gate {
	if d.Schemas == nil || d.Inspector == nil || image == "" {
		return nil
	}
	schema, err := schemaOf(ctx, d, namespace, image, pullSecrets)
	if err != nil {
		klog.FromContext(ctx).V(LogFlow).Info("Not validating variables: the image schema is unavailable", "image", image, "error", err.Error())
		return nil
	}
	problems := SchemaProblems(schema, vars)
	if len(problems) == 0 {
		return nil
	}
	msg := strings.Join(problems[:min(len(problems), maxSchemaProblems)], "; ")
	if extra := len(problems) - maxSchemaProblems; extra > 0 {
		msg += fmt.Sprintf(" (and %d more)", extra)
	}
	return variablesInvalid("The variables do not match the module image's schema (" + imageinspect.VariablesSchemaLabel + "): " + msg)
}

// schemaOf returns the variables schema of image from d's cache, reading
// the image config on a miss; nil for an image that declares none. ctx
// bounds the read, namespace and pullSecrets name the pull Secrets that
// authenticate it. It returns the schema, or an error from reading the
// image or its label.
func schemaOf(ctx context.Context, d Deps, namespace, image string, pullSecrets []string) (*varschema.Schema, error) {
	if s, ok := d.Schemas.Cached(image); ok {
		return s, nil
	}
	keychain, _, err := imageinspect.PullSecretsKeychain(ctx, d.APIReader, namespace, pullSecrets)
	if err != nil {
		return nil, err
	}
	return d.Schemas.Schema(ctx, d.Inspector, image, keychain)
}
