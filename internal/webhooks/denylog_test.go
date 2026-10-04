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
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/klog/v2"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// TestInvalidLogsDenial: a denial is logged at V(1) with the requesting
// user, the kind, name and the violated field, and never the rejected
// value; no violation logs nothing.
func TestInvalidLogsDenial(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	log := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1), textlogger.Output(&buf)))
	ctx := klog.NewContext(context.Background(), log)
	ctx = admission.NewContextWithRequest(ctx, admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		UserInfo: authenticationv1.UserInfo{Username: "alice"},
	}})

	if err := invalid(ctx, "TerraformMachine", "m", nil); err != nil || buf.Len() != 0 {
		t.Fatalf("empty errs: err = %v, log = %q; want none", err, buf.String())
	}
	errs := field.ErrorList{field.Invalid(field.NewPath("spec", "source", "image"), "s3cret-value", "bad")}
	if err := invalid(ctx, "TerraformMachine", "m", errs); err == nil {
		t.Fatal("want an Invalid error")
	}
	out := buf.String()
	for _, want := range []string{"alice", "TerraformMachine", `"m"`, "spec.source.image"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q lacks %q", out, want)
		}
	}
	if strings.Contains(out, "s3cret-value") {
		t.Errorf("log %q leaks the rejected value", out)
	}
}
