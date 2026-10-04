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
	"strings"
	"testing"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// TestValidateEffectiveJobPolicy covers the lock timeout against the
// deadline across inherited, defaulted and explicit values.
func TestValidateEffectiveJobPolicy(t *testing.T) {
	t.Parallel()
	lock := func(v int32) *int32 { return &v }
	tests := []struct {
		name    string
		own     *infrav1.JobPolicy
		defs    *infrav1.JobPolicy
		wantErr []string
	}{
		{name: "all defaults"},
		{name: "explicit valid pair", own: &infrav1.JobPolicy{ActiveDeadlineSeconds: 600, LockTimeoutSeconds: lock(60)}},
		{
			name:    "inherited lock exceeds own deadline",
			own:     &infrav1.JobPolicy{ActiveDeadlineSeconds: 60},
			defs:    &infrav1.JobPolicy{LockTimeoutSeconds: lock(120)},
			wantErr: []string{"lockTimeoutSeconds 120 (configured)", "activeDeadlineSeconds 60 (configured)"},
		},
		{
			name:    "default lock exceeds short deadline",
			own:     &infrav1.JobPolicy{ActiveDeadlineSeconds: 60},
			wantErr: []string{"lockTimeoutSeconds 300 (built-in default)", "activeDeadlineSeconds 60 (configured)"},
		},
		{
			name:    "equal values",
			own:     &infrav1.JobPolicy{ActiveDeadlineSeconds: 300, LockTimeoutSeconds: lock(300)},
			wantErr: []string{"lockTimeoutSeconds 300"},
		},
		{
			name:    "lock exceeds default deadline",
			own:     &infrav1.JobPolicy{LockTimeoutSeconds: lock(7200)},
			wantErr: []string{"activeDeadlineSeconds 3600 (built-in default)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateEffectiveJobPolicy(MergeJobPolicy(tt.own, tt.defs))
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatal("error = nil, want one")
			}
			for _, w := range tt.wantErr {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("error %q lacks %q", err, w)
				}
			}
		})
	}
}
