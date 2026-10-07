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

package conditions

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestClampMessages proves a message over MaxMessage is cut to fit, the
// others are left as they are, and nothing is reported when none is cut.
func TestClampMessages(t *testing.T) {
	t.Parallel()
	m := &infrav1.TerraformMachine{}
	m.SetConditions([]metav1.Condition{
		{Type: "A", Message: strings.Repeat("é", MaxMessage)},
		{Type: "B", Message: "short"},
	})
	if !ClampMessages(m) {
		t.Fatal("nothing cut")
	}
	got := m.GetConditions()
	if len(got[0].Message) > MaxMessage || len(got[0].Message) < MaxMessage-1 || got[1].Message != "short" {
		t.Errorf("messages = %d bytes, %q", len(got[0].Message), got[1].Message)
	}
	if ClampMessages(m) {
		t.Error("a second pass cut again")
	}
}
