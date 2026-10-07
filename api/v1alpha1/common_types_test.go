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

package v1alpha1

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ptr returns a pointer to a copy of v.
func ptr[T any](v T) *T { return &v }

// TestCommonTypesJSONRoundTrip proves each shared status and spec type in
// common_types.go marshals to the expected JSON and unmarshals back to an
// equal value.
func TestCommonTypesJSONRoundTrip(t *testing.T) {
	t.Parallel()
	// metav1.Time unmarshals into time.Local; Rfc3339Copy parses into UTC
	// when the host zone is UTC (as on CI runners), and reflect.DeepEqual
	// compares the *time.Location, so pin the expected value to Local too.
	start := metav1.NewTime(metav1.Now().Rfc3339Copy().Local())
	tests := []struct {
		name string
		in   any
		out  any
		want string
	}{
		{
			name: "source with every field",
			in: &Source{
				Image:           "ghcr.io/acme/captf-aws-machine:1.4.2",
				ImagePullPolicy: corev1.PullAlways,
			},
			out:  &Source{},
			want: `{"image":"ghcr.io/acme/captf-aws-machine:1.4.2","imagePullPolicy":"Always"}`,
		},
		{
			name: "empty job policy serializes to {}",
			in:   &JobPolicy{},
			out:  &JobPolicy{},
			want: `{}`,
		},
		{
			name: "explicit zero pointers survive",
			in:   &JobPolicy{FailedJobsHistoryLimit: ptr(int32(0)), LockTimeoutSeconds: ptr(int32(0))},
			out:  &JobPolicy{},
			want: `{"failedJobsHistoryLimit":0,"lockTimeoutSeconds":0}`,
		},
		{
			name: "activeDeadlineSeconds is a pointer",
			in:   &JobPolicy{ActiveDeadlineSeconds: 600},
			out:  &JobPolicy{},
			want: `{"activeDeadlineSeconds":600}`,
		},
		{
			name: "drift policy with 0 interval (disabled)",
			in:   &DriftPolicy{IntervalSeconds: ptr(int32(0)), Action: DriftActionReport},
			out:  &DriftPolicy{},
			want: `{"intervalSeconds":0,"action":"Report"}`,
		},
		{
			name: "machine drift policy has no action",
			in:   &MachineDriftPolicy{IntervalSeconds: ptr(int32(0))},
			out:  &MachineDriftPolicy{},
			want: `{"intervalSeconds":0}`,
		},
		{
			name: "machine remediation",
			in:   &MachineRemediation{AnnotateMachine: ptr(true), UnhealthyThreshold: 3, HealthCheckIntervalSeconds: 600},
			out:  &MachineRemediation{},
			want: `{"annotateMachine":true,"unhealthyThreshold":3,"healthCheckIntervalSeconds":600}`,
		},
		{
			name: "run error summary",
			in:   &RunError{Kind: RunErrorKindStep, Step: "apply", Summary: "apply failed: 1 error"},
			out:  &RunError{},
			want: `{"kind":"step","step":"apply","summary":"apply failed: 1 error"}`,
		},
		{
			name: "initialization unset omits provisioned",
			in:   &Initialization{},
			out:  &Initialization{},
			want: `{}`,
		},
		{
			name: "last run without error omits it",
			in: &LastRun{
				Job:       "captf-m-demo-apply-a1-0f3a2b",
				Operation: OperationApply,
				Steps:     []RunStep{{Name: "init", ExitCode: ptr(int32(0)), DurationMilliseconds: ptr(int64(4100))}},
			},
			out:  &LastRun{},
			want: `{"job":"captf-m-demo-apply-a1-0f3a2b","operation":"apply","steps":[{"name":"init","exitCode":0,"durationMilliseconds":4100}]}`,
		},
		{
			name: "last run with error",
			in: &LastRun{
				Job:       "captf-m-demo-apply-a2-0f3a2b",
				Operation: OperationApply,
				Error:     RunError{Kind: RunErrorKindImageLayout},
			},
			out:  &LastRun{},
			want: `{"job":"captf-m-demo-apply-a2-0f3a2b","operation":"apply","error":{"kind":"image-layout"}}`,
		},
		{
			name: "active job",
			in:   &ActiveJob{Name: "captf-c-demo-drift-a1-0f3a2b", Operation: OperationDrift, Attempt: 1, StartTime: &start},
			out:  &ActiveJob{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			data, err := json.Marshal(tt.in)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if tt.want != "" && string(data) != tt.want {
				t.Errorf("marshal = %s, want %s", data, tt.want)
			}
			if err := json.Unmarshal(data, tt.out); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if !reflect.DeepEqual(tt.in, tt.out) {
				t.Errorf("round trip = %#v, want %#v", tt.out, tt.in)
			}
		})
	}
}

// TestEnumsMatchMarkers checks that the Go constants of each enum type are
// exactly the values of its +kubebuilder:validation:Enum marker.
func TestEnumsMatchMarkers(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("common_types.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	tests := []struct {
		typeName string
		values   []string
	}{
		{"DriftAction", []string{string(DriftActionReport), string(DriftActionRemediate)}},
		{"Operation", []string{string(OperationApply), string(OperationDestroy), string(OperationDrift), string(OperationRefresh), string(OperationRestore), string(OperationPlan)}},
		{"RunErrorKind", []string{string(RunErrorKindImageLayout), string(RunErrorKindStep), string(RunErrorKindInterrupted), string(RunErrorKindBlocked), string(RunErrorKindPlanChanged)}},
		{"VariablesFormat", []string{string(VariablesFormatString), string(VariablesFormatJSON)}},
		{"ApplyPolicy", []string{string(ApplyPolicyAutomatic), string(ApplyPolicyManual)}},
		{"PlanTargetKind", []string{string(PlanTargetCluster), string(PlanTargetMachinePool)}},
		{"PlanReason", []string{string(PlanReasonManual), string(PlanReasonDestructive), string(PlanReasonExportsChange)}},
		{"PlanPhase", []string{string(PlanPhasePending), string(PlanPhaseApproved), string(PlanPhaseApplied), string(PlanPhaseSuperseded), string(PlanPhaseFailed)}},
	}
	cluster, err := os.ReadFile("terraformcluster_types.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src = append(src, cluster...)
	plan, err := os.ReadFile("terraformplan_types.go")
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	src = append(src, plan...)
	for _, tt := range tests {
		re := regexp.MustCompile(`\+kubebuilder:validation:Enum=([^\n]+)\ntype ` + tt.typeName + ` string`)
		m := re.FindSubmatch(src)
		if m == nil {
			t.Errorf("%s: no Enum marker directly above the type", tt.typeName)
			continue
		}
		if got, want := string(m[1]), strings.Join(tt.values, ";"); got != want {
			t.Errorf("%s: marker Enum=%s, constants %s", tt.typeName, got, want)
		}
	}
	if got := []string{string(DriftActionReport), string(DriftActionRemediate)}; !reflect.DeepEqual(got, []string{"Report", "Remediate"}) {
		t.Errorf("DriftAction values = %v, want [Report Remediate]", got)
	}
}

// TestEveryFieldHasJSONTag guards the CRD shape: every exported field of the
// shared types carries a lower-camel json tag.
func TestEveryFieldHasJSONTag(t *testing.T) {
	t.Parallel()
	types := []any{
		Source{}, JobPolicy{}, DriftPolicy{}, MachineDriftPolicy{}, MachineRemediation{}, IdentityReference{},
		SecretReference{}, Initialization{}, ActiveJob{}, RunStep{}, RunError{}, LastRun{}, SourceStatus{},
	}
	lowerCamel := regexp.MustCompile(`^[a-z][A-Za-z0-9]*$`)
	for _, v := range types {
		rt := reflect.TypeOf(v)
		for i := range rt.NumField() {
			f := rt.Field(i)
			name := strings.Split(f.Tag.Get("json"), ",")[0]
			if !lowerCamel.MatchString(name) {
				t.Errorf("%s.%s: json tag %q is not lower camel case", rt.Name(), f.Name, f.Tag.Get("json"))
			}
		}
	}
}
