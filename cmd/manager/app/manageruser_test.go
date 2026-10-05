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

package app

import (
	"strings"
	"testing"

	"k8s.io/klog/v2/ktesting"

	"github.com/captf-io/cluster-api-provider-terraform/cmd/manager/app/options"
)

// TestWarnManagerUserUnset proves an empty manager identity logs a warning
// that names both environment variables, and a set one logs nothing.
func TestWarnManagerUserUnset(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		user string
		want bool
	}{
		{"unset", "", true},
		{"set", "system:serviceaccount:captf-system:captf-manager", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			logger := ktesting.NewLogger(t, ktesting.NewConfig(ktesting.BufferLogs(true)))
			warnManagerUserUnset(logger, tt.user)
			got := logger.GetSink().(ktesting.Underlier).GetBuffer().String()
			if (got != "") != tt.want {
				t.Fatalf("log = %q, want a warning: %v", got, tt.want)
			}
			if tt.want {
				for _, s := range []string{options.PodNamespaceEnv, options.ServiceAccountEnv, "providerID"} {
					if !strings.Contains(got, s) {
						t.Errorf("warning %q lacks %q", got, s)
					}
				}
			}
		})
	}
}
