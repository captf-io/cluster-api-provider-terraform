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
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	serjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
	utiljson "k8s.io/apimachinery/pkg/util/json"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// conditionsAccessor is the shape sigs.k8s.io/cluster-api/util/conditions
// needs (Getter/Setter); the api module cannot import that package.
type conditionsAccessor interface {
	// GetConditions returns the object's condition list.
	GetConditions() []metav1.Condition
	// SetConditions replaces the object's condition list.
	SetConditions([]metav1.Condition)
}

var (
	_ conditionsAccessor = &TerraformCluster{}
	_ conditionsAccessor = &TerraformMachine{}
	_ conditionsAccessor = &TerraformMachineTemplate{}
	_ conditionsAccessor = &TerraformMachinePool{}
	_ conditionsAccessor = &TerraformClusterIdentity{}
	_ conditionsAccessor = &TerraformPlan{}
)

// TestConditionsAccessors checks that every kind's SetConditions stores
// the list GetConditions returns, which CAPI's condition helpers rely on,
// and that a new object has none.
func TestConditionsAccessors(t *testing.T) {
	t.Parallel()
	want := []metav1.Condition{{Type: ReadyCondition, Status: metav1.ConditionTrue, Reason: "Ready"}}
	for name, obj := range map[string]conditionsAccessor{
		"TerraformCluster":         &TerraformCluster{},
		"TerraformMachine":         &TerraformMachine{},
		"TerraformMachineTemplate": &TerraformMachineTemplate{},
		"TerraformMachinePool":     &TerraformMachinePool{},
		"TerraformClusterIdentity": &TerraformClusterIdentity{},
		"TerraformPlan":            &TerraformPlan{},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := obj.GetConditions(); got != nil {
				t.Fatalf("new object has conditions %v", got)
			}
			obj.SetConditions(want)
			if got := obj.GetConditions(); !reflect.DeepEqual(got, want) {
				t.Errorf("GetConditions() = %v, want %v", got, want)
			}
		})
	}
}

// fullSource returns a Source fixture with every field populated.
func fullSource() Source {
	return Source{
		Image:           "ghcr.io/acme/captf-aws:1.4.2",
		ImagePullPolicy: corev1.PullIfNotPresent,
	}
}

// fullJobs returns a JobPolicy fixture with every field populated.
func fullJobs() *JobPolicy {
	return &JobPolicy{
		SuccessfulJobsHistoryLimit: ptr(int32(3)),
		FailedJobsHistoryLimit:     ptr(int32(3)),
		ActiveDeadlineSeconds:      3600,
		ServiceAccountName:         "runner",
		LockTimeoutSeconds:         ptr(int32(300)),
		ImagePullSecrets:           []corev1.LocalObjectReference{{Name: "regcred"}},
		Resources: &corev1.ResourceRequirements{
			Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("512Mi")},
		},
		Env:                []corev1.EnvVar{{Name: "TF_LOG", Value: "INFO"}},
		SecurityContext:    &corev1.SecurityContext{RunAsNonRoot: ptr(true)},
		PodSecurityContext: &corev1.PodSecurityContext{FSGroup: ptr(int64(65532))},
	}
}

// fullVariables returns a spec.variables fixture as compact JSON, since the
// round trip re-marshals it; it returns that raw extension.
func fullVariables() runtime.RawExtension {
	return runtime.RawExtension{Raw: []byte(`{"disk_gib":40,"instance_type":"t3.large","subnet_ids":["a","b"]}`)}
}

// fullVariablesFrom returns a spec.variablesFrom fixture covering a
// ConfigMap and an optional, format-tagged Secret source.
func fullVariablesFrom() []VariablesSource {
	return []VariablesSource{
		{ConfigMapRef: VariablesSourceReference{Name: "sizes"}},
		{SecretRef: VariablesSourceReference{Name: "db"}, Optional: ptr(true), Format: VariablesFormatJSON},
	}
}

// fullDrift returns a DriftPolicy fixture with every field populated.
func fullDrift() *DriftPolicy {
	return &DriftPolicy{IntervalSeconds: ptr(int32(1800)), Action: DriftActionRemediate}
}

// fullMachineDrift returns a MachineDriftPolicy fixture with every field
// populated.
func fullMachineDrift() *MachineDriftPolicy {
	return &MachineDriftPolicy{IntervalSeconds: ptr(int32(1800))}
}

// fullPoolDrift returns a MachinePoolDriftPolicy fixture with every field
// populated.
func fullPoolDrift() *MachinePoolDriftPolicy {
	return &MachinePoolDriftPolicy{IntervalSeconds: 1800, Action: DriftActionRemediate}
}

// fullRunStatus returns fixtures for the status.activeJob, status.lastRun
// and status.source run-tracking fields, and the timestamp they share, all
// with every field populated: an ActiveJob, a LastRun with a failed step, a
// SourceStatus, and the shared *metav1.Time.
func fullRunStatus() (ActiveJob, LastRun, SourceStatus, *metav1.Time) {
	now := metav1.NewTime(metav1.Now().Rfc3339Copy().Time)
	return ActiveJob{Name: "captf-c-demo-apply-a1-0f3a2b", Operation: OperationApply, Attempt: 1, StartTime: &now},
		LastRun{
			Job:       "captf-c-demo-apply-a1-0f3a2b",
			Operation: OperationApply,
			Steps: []RunStep{
				{Name: "init", ExitCode: ptr(int32(0)), DurationMilliseconds: ptr(int64(4100))},
				{Name: "apply", ExitCode: ptr(int32(1)), DurationMilliseconds: ptr(int64(9800))},
			},
			Error: RunError{Kind: RunErrorKindStep, Step: "apply", Summary: "Error: timeout"},
		},
		SourceStatus{Image: "ghcr.io/acme/captf-aws:1.4.2", ImageDigest: "sha256:abc", RuntimeVersion: "1.16.4"},
		&now
}

// TestKindsJSONRoundTrip proves each of the eight kinds' spec and status,
// fully populated, marshals to the expected JSON and unmarshals back to an
// equal value.
func TestKindsJSONRoundTrip(t *testing.T) {
	t.Parallel()
	active, lastRun, source, now := fullRunStatus()
	tests := []struct {
		name string
		in   any
		out  func() any
	}{
		{
			name: "TerraformCluster",
			in: &TerraformCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
				Spec: TerraformClusterSpec{
					ControlPlaneEndpoint: &clusterv1.APIEndpoint{Host: "api.demo.example.com", Port: 6443},
					WorkspaceSpec: WorkspaceSpec{
						Source:        fullSource(),
						IdentityRef:   IdentityReference{Name: "aws"},
						Jobs:          fullJobs(),
						Variables:     fullVariables(),
						VariablesFrom: fullVariablesFrom(),
					},
					Drift: fullDrift(),
					Defaults: &TerraformClusterDefaults{
						IdentityRef:                      IdentityReference{Name: "aws"},
						Jobs:                             fullJobs(),
						Drift:                            fullDrift(),
						Remediation:                      &MachineRemediation{AnnotateMachine: ptr(true), UnhealthyThreshold: 3, HealthCheckIntervalSeconds: 600},
						MembershipRefreshIntervalSeconds: 30,
					},
				},
				Status: TerraformClusterStatus{
					Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: *now}},
					WorkspaceStatus: WorkspaceStatus{
						Initialization:      Initialization{Provisioned: ptr(true)},
						ObservedGeneration:  2,
						ActiveJob:           active,
						LastDriftCheck:      now,
						LastRefresh:         now,
						LastRun:             lastRun,
						ObservedStateSerial: 7,
						StateSecretSuffix:   "0123456789abcdef-c",
						Source:              source,
					},
					FailureDomains: []clusterv1.FailureDomain{
						{Name: "zone-a", ControlPlane: ptr(true), Attributes: map[string]string{"region": "r1"}},
					},
					Exports: runtime.RawExtension{Raw: []byte(`{"kubeconfig_secret":"demo-kubeconfig","zones":["a","b"]}`)},
				},
			},
			out: func() any { return &TerraformCluster{} },
		},
		{
			name: "TerraformMachine",
			in: &TerraformMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-cp-abcde", Namespace: "default"},
				Spec: TerraformMachineSpec{
					ProviderID: "aws:///us-east-1a/i-0123",
					WorkspaceSpec: WorkspaceSpec{
						Source:        fullSource(),
						IdentityRef:   IdentityReference{Name: "aws"},
						Jobs:          fullJobs(),
						Variables:     fullVariables(),
						VariablesFrom: fullVariablesFrom(),
					},
					Drift:       fullMachineDrift(),
					Remediation: &MachineRemediation{AnnotateMachine: ptr(true), UnhealthyThreshold: 3},
				},
				Status: TerraformMachineStatus{
					Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionFalse, Reason: "NotReady", LastTransitionTime: *now}},
					WorkspaceStatus: WorkspaceStatus{
						Initialization:      Initialization{Provisioned: ptr(true)},
						ObservedGeneration:  1,
						ActiveJob:           active,
						LastDriftCheck:      now,
						LastRefresh:         now,
						LastRun:             lastRun,
						ObservedStateSerial: 3,
						StateSecretSuffix:   "0123456789abcdef-m",
						Source:              source,
					},
					Addresses: []clusterv1.MachineAddress{
						{Type: clusterv1.MachineInternalIP, Address: "10.0.0.1"},
						{Type: clusterv1.MachineHostName, Address: "demo-cp-abcde"},
					},
					FailureDomain:    "zone-a",
					Interruptible:    ptr(true),
					UnhealthySamples: 2,
				},
			},
			out: func() any { return &TerraformMachine{} },
		},
		{
			name: "TerraformClusterTemplate",
			in: &TerraformClusterTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"},
				Spec: TerraformClusterTemplateSpec{Template: TerraformClusterTemplateResource{
					TemplateMeta: TemplateMeta{ObjectMeta: &clusterv1.ObjectMeta{Labels: map[string]string{"a": "b"}}},
					Spec:         TerraformClusterSpec{WorkspaceSpec: WorkspaceSpec{Source: fullSource()}, Defaults: &TerraformClusterDefaults{IdentityRef: IdentityReference{Name: "aws"}}},
				}},
			},
			out: func() any { return &TerraformClusterTemplate{} },
		},
		{
			name: "TerraformMachineTemplate",
			in: &TerraformMachineTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-md", Namespace: "default"},
				Spec: TerraformMachineTemplateSpec{Template: TerraformMachineTemplateResource{
					TemplateMeta: TemplateMeta{ObjectMeta: &clusterv1.ObjectMeta{Annotations: map[string]string{"x": "y"}}},
					Spec:         TerraformMachineSpec{WorkspaceSpec: WorkspaceSpec{Source: fullSource()}, Remediation: &MachineRemediation{AnnotateMachine: ptr(false)}},
				}},
				Status: TerraformMachineTemplateStatus{
					Conditions: []metav1.Condition{{Type: "CapacityResolved", Status: metav1.ConditionTrue, Reason: "CapacityResolved", LastTransitionTime: *now}},
					Capacity: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("4"),
						corev1.ResourceMemory: resource.MustParse("16Gi"),
					},
					NodeInfo:       NodeInfo{Architecture: ArchitectureArm64, OperatingSystem: "linux"},
					CapacitySource: CapacitySource{Image: "ghcr.io/acme/captf-aws:1.4.2"},
				},
			},
			out: func() any { return &TerraformMachineTemplate{} },
		},
		{
			name: "TerraformMachinePool",
			in: &TerraformMachinePool{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-mp0", Namespace: "default"},
				Spec: TerraformMachinePoolSpec{
					ProviderID:     "aws-asg:///demo-mp0",
					ProviderIDList: []string{"aws:///us-east-1a/i-0123", "aws:///us-east-1a/i-0456"},
					WorkspaceSpec: WorkspaceSpec{
						Source:        fullSource(),
						IdentityRef:   IdentityReference{Name: "aws"},
						Jobs:          fullJobs(),
						Variables:     fullVariables(),
						VariablesFrom: fullVariablesFrom(),
					},
					Drift:                            fullPoolDrift(),
					MembershipRefreshIntervalSeconds: 60,
				},
				Status: TerraformMachinePoolStatus{
					Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: *now}},
					WorkspaceStatus: WorkspaceStatus{
						Initialization:      Initialization{Provisioned: ptr(true)},
						ObservedGeneration:  1,
						ActiveJob:           active,
						LastDriftCheck:      now,
						LastRefresh:         now,
						LastRun:             lastRun,
						ObservedStateSerial: 5,
						StateSecretSuffix:   "0000000000000000-mp",
						Source:              source,
					},
					Ready:    ptr(true),
					Replicas: ptr(int32(2)),
					Instances: []MachinePoolInstance{
						{ProviderID: "aws:///us-east-1a/i-0123", InstanceID: "i-0123", Addresses: []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: "10.0.0.1"}}, FailureDomain: "us-east-1a", State: HealthStateRunning},
						{ProviderID: "aws:///us-east-1a/i-0456", State: HealthStateRunning},
					},
				},
			},
			out: func() any { return &TerraformMachinePool{} },
		},
		{
			name: "TerraformMachinePoolTemplate",
			in: &TerraformMachinePoolTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-mp0", Namespace: "default"},
				Spec: TerraformMachinePoolTemplateSpec{Template: TerraformMachinePoolTemplateResource{
					TemplateMeta: TemplateMeta{ObjectMeta: &clusterv1.ObjectMeta{Labels: map[string]string{"a": "b"}}},
					Spec:         TerraformMachinePoolSpec{WorkspaceSpec: WorkspaceSpec{Source: fullSource()}, Drift: fullPoolDrift()},
				}},
			},
			out: func() any { return &TerraformMachinePoolTemplate{} },
		},
		{
			name: "TerraformClusterIdentity",
			in: &TerraformClusterIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: "aws"},
				Spec: TerraformClusterIdentitySpec{
					SecretRef: SecretReference{Name: "aws-creds", Namespace: "captf-system"},
					AllowedNamespaces: &AllowedNamespaces{
						List:     []string{"team-a", "team-b"},
						Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"captf.io/tenant": "true"}},
					},
				},
				Status: TerraformClusterIdentityStatus{
					Conditions: []metav1.Condition{{Type: ReadyCondition, Status: metav1.ConditionTrue, Reason: SecretFoundReason, LastTransitionTime: *now}},
					Namespaces: []string{"team-a"},
				},
			},
			out: func() any { return &TerraformClusterIdentity{} },
		},
		{
			name: "TerraformClusterIdentity allowing all namespaces keeps selector {}",
			in: &TerraformClusterIdentity{
				ObjectMeta: metav1.ObjectMeta{Name: "open"},
				Spec: TerraformClusterIdentitySpec{
					SecretRef:         SecretReference{Name: "aws-creds", Namespace: "captf-system"},
					AllowedNamespaces: &AllowedNamespaces{Selector: &metav1.LabelSelector{}},
				},
			},
			out: func() any { return &TerraformClusterIdentity{} },
		},
		{
			name: "TerraformPlan",
			in: &TerraformPlan{
				ObjectMeta: metav1.ObjectMeta{Name: "demo-0123456789", Namespace: "default"},
				Spec: TerraformPlanSpec{
					TargetRef:  PlanTargetRef{Kind: PlanTargetCluster, Name: "demo"},
					PlanHash:   "p2:abc",
					InputsHash: "h2:def",
					Reason:     PlanReasonDestructive,
					Summary: PlanSummary{
						Create: ptr(int32(1)), Update: ptr(int32(2)), Replace: ptr(int32(3)), Delete: ptr(int32(4)),
						Import: ptr(int32(5)), Move: ptr(int32(6)), Forget: ptr(int32(7)), OutputChanges: ptr(int32(8)),
						Resources: []string{"aws_instance.a (replace)", "aws_instance.b (update, move)"}, Truncated: ptr(true),
					},
					Approved:   ptr(true),
					ApprovedBy: "alice@example.com",
				},
				Status: TerraformPlanStatus{
					Phase:              PlanPhaseApproved,
					Conditions:         []metav1.Condition{{Type: PlanApprovedCondition, Status: metav1.ConditionTrue, Reason: "Approved", LastTransitionTime: *now}},
					ObservedGeneration: 2,
				},
			},
			out: func() any { return &TerraformPlan{} },
		},
	}
	decoders := map[string]func([]byte, any) error{
		"encoding/json":                     json.Unmarshal,
		"k8s.io/apimachinery/pkg/util/json": utiljson.Unmarshal,
	}
	for _, tt := range tests {
		for decName, decode := range decoders {
			t.Run(tt.name+"/"+decName, func(t *testing.T) {
				t.Parallel()
				data, err := json.Marshal(tt.in)
				if err != nil {
					t.Fatalf("marshal: %v", err)
				}
				got := tt.out()
				if err := decode(data, got); err != nil {
					t.Fatalf("unmarshal: %v", err)
				}
				// Quantities re-marshal canonically; compare through JSON.
				again, err := json.Marshal(got)
				if err != nil {
					t.Fatalf("re-marshal: %v", err)
				}
				if !bytes.Equal(again, data) {
					t.Errorf("round trip lost data:\n got %s\nwant %s", again, data)
				}
			})
		}
	}
}

// TestZeroStatusOmitted checks the omitzero convention: an object without a
// status serializes without a status key.
func TestZeroStatusOmitted(t *testing.T) {
	t.Parallel()
	data, err := json.Marshal(&TerraformMachine{Spec: TerraformMachineSpec{WorkspaceSpec: WorkspaceSpec{Source: fullSource()}}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := m["status"]; ok {
		t.Errorf("zero status serialized: %s", data)
	}
	spec, ok := m["spec"].(map[string]any)
	if !ok {
		t.Fatalf("spec missing: %s", data)
	}
	for _, k := range []string{"identityRef", "jobs", "drift", "remediation", "providerID"} {
		if _, ok := spec[k]; ok {
			t.Errorf("unset spec.%s serialized: %s", k, data)
		}
	}
	if !reflect.DeepEqual(spec["source"].(map[string]any)["image"], "ghcr.io/acme/captf-aws:1.4.2") {
		t.Errorf("spec.source.image missing: %s", data)
	}
}

// TestAddToSchemeRegistersEveryKind guards the explicit registration in
// groupversion_info.go: no *_types.go registers itself with init().
func TestAddToSchemeRegistersEveryKind(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	for _, kind := range []string{
		"TerraformCluster", "TerraformClusterList",
		"TerraformClusterTemplate", "TerraformClusterTemplateList",
		"TerraformMachine", "TerraformMachineList",
		"TerraformMachineTemplate", "TerraformMachineTemplateList",
		"TerraformMachinePool", "TerraformMachinePoolList",
		"TerraformMachinePoolTemplate", "TerraformMachinePoolTemplateList",
		"TerraformClusterIdentity", "TerraformClusterIdentityList",
		"TerraformPlan", "TerraformPlanList",
	} {
		if !scheme.Recognizes(SchemeGroupVersion.WithKind(kind)) {
			t.Errorf("%s is not registered", kind)
		}
	}
	if !scheme.Recognizes(SchemeGroupVersion.WithKind("ListOptions")) {
		t.Errorf("metav1 options are not registered for the group-version")
	}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		if strings.Contains(string(src), "\nfunc init() {") {
			t.Errorf("%s declares func init(); register kinds in groupversion_info.go", f)
		}
	}
}

// TestSerializerOmitsZeroStatus encodes through apimachinery's JSON
// serializer, the path the API server and clients use: a zero status must not
// be persisted as `status: {}`, which MinProperties=1 would reject.
func TestSerializerOmitsZeroStatus(t *testing.T) {
	t.Parallel()
	scheme := runtime.NewScheme()
	if err := AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	s := serjson.NewSerializerWithOptions(serjson.DefaultMetaFactory, scheme, scheme, serjson.SerializerOptions{})
	objs := []runtime.Object{
		&TerraformMachine{
			TypeMeta: metav1.TypeMeta{APIVersion: SchemeGroupVersion.String(), Kind: "TerraformMachine"},
			Spec:     TerraformMachineSpec{WorkspaceSpec: WorkspaceSpec{Source: fullSource()}},
		},
		&TerraformCluster{
			TypeMeta: metav1.TypeMeta{APIVersion: SchemeGroupVersion.String(), Kind: "TerraformCluster"},
			Spec:     TerraformClusterSpec{WorkspaceSpec: WorkspaceSpec{Source: fullSource()}},
		},
	}
	for _, obj := range objs {
		var buf bytes.Buffer
		if err := s.Encode(obj, &buf); err != nil {
			t.Fatalf("encode: %v", err)
		}
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, ok := m["status"]; ok {
			t.Errorf("%T: zero status serialized by the apimachinery serializer: %s", obj, buf.String())
		}
	}
}

// TestPlanPhaseTerminal: Applied, Superseded and Failed are terminal;
// Pending, Approved and the unset phase are live.
func TestPlanPhaseTerminal(t *testing.T) {
	t.Parallel()
	for phase, want := range map[PlanPhase]bool{
		"": false, PlanPhasePending: false, PlanPhaseApproved: false,
		PlanPhaseApplied: true, PlanPhaseSuperseded: true, PlanPhaseFailed: true,
	} {
		if got := phase.Terminal(); got != want {
			t.Errorf("%q.Terminal() = %v, want %v", phase, got, want)
		}
	}
}
