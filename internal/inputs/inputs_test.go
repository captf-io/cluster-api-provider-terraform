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

package inputs

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/contract"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// ns is the namespace these tests create objects in.
const ns = "team-a"

// ctx is the context used across these tests.
var ctx = context.Background()

// newClient returns a fake client, scheme registered, seeded with objs; t
// fails the test if a scheme registration errors.
func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, infrav1.AddToScheme} {
		if err := add(s); err != nil {
			t.Fatalf("scheme: %v", err)
		}
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// machine returns a TerraformMachine named name in ns with a fixed UID.
func machine(name string) *infrav1.TerraformMachine {
	return &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: "uid-m"}}
}

// machineFiles renders a real root, with bootstrap as the bootstrap data,
// and returns the rendered files, so render and inputs meet; t fails the
// test if rendering errors.
func machineFiles(t *testing.T, bootstrap string) render.Files {
	t.Helper()
	files, err := render.Root(contract.RoleMachine, contract.MachineInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "c", Namespace: ns},
			Object:   contract.Object{Kind: "TerraformMachine", Name: "m", Namespace: ns},
			Tags:     contract.Tags("c", ns, "TerraformMachine", "m", ""),
		},
		MachineName:     "m",
		BootstrapData:   base64.StdEncoding.EncodeToString([]byte(bootstrap)),
		BootstrapFormat: "cloud-config",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return files
}

// clusterFiles renders a real cluster root with ControlPlaneInitialized set
// to initialized and returns the rendered files; t fails the test if
// rendering errors.
func clusterFiles(t *testing.T, initialized bool) render.Files {
	t.Helper()
	files, err := render.Root(contract.RoleCluster, contract.ClusterInputs{
		CommonInputs: contract.CommonInputs{
			Contract: contract.Version,
			Cluster:  contract.Cluster{Name: "c", Namespace: ns},
			Object:   contract.Object{Kind: "TerraformCluster", Name: "c", Namespace: ns},
			Tags:     contract.Tags("c", ns, "TerraformCluster", "c", ""),
		},
		ControlPlaneInitialized: initialized,
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return files
}

// getSecret returns the Secret named name in ns, read through c; t fails
// the test if the get errors.
func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	s := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, s); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return s
}

// dataKeys returns the sorted data keys of s.
func dataKeys(s *corev1.Secret) []string {
	keys := make([]string, 0, len(s.Data))
	for k := range s.Data {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// TestName proves Name is deterministic and produces a valid, unique Secret
// name even for names that would push it past 253 characters, that
// RunName is unchanged, and that AppliedName is bounded likewise.
func TestName(t *testing.T) {
	t.Parallel()
	if got := Name("m", "web-0"); got != "captf-inputs-m-web-0" {
		t.Errorf("Name = %s", got)
	}
	long := strings.Repeat("a", 250) + "-x"
	other := strings.Repeat("a", 250) + "-y"
	for _, n := range []string{long, strings.Repeat("b", 253), strings.Repeat("c", 236) + "." + strings.Repeat("d", 16)} {
		got := Name("mp", n)
		if errs := validation.IsDNS1123Subdomain(got); len(errs) != 0 || len(got) > 253 {
			t.Errorf("Name(%d chars) = %q (%d): %v", len(n), got, len(got), errs)
		}
		if Name("mp", n) != got {
			t.Error("Name is not deterministic")
		}
	}
	if Name("m", long) == Name("m", other) {
		t.Error("names differing past the cut collide")
	}
	if RunName("captf-m-web-0-apply-a1-abc123") != "captf-run-captf-m-web-0-apply-a1-abc123" {
		t.Error("RunName changed")
	}
	if got := AppliedName("m", "web-0"); got != "captf-applied-m-web-0" {
		t.Errorf("AppliedName = %s", got)
	}
	if got := AppliedName("mp", long); len(got) > 253 || len(validation.IsDNS1123Subdomain(got)) != 0 {
		t.Errorf("AppliedName(%d chars) = %q", len(long), got)
	}
}

// attempt returns an attempt record of files for Job job, with image
// r:v1 and identity id.
func attempt(files render.Files, job string) Record {
	return Record{Files: files, Image: "r:v1", Identity: "id", InputsHash: "h1:" + job, Job: job}
}

// TestWriteAttemptRead proves WriteAttempt round-trips the attempt record
// through Read, without a digest, and sets the Secret's type, owner
// reference and labels; a rewrite replaces the record and removes the
// may-have-applied mark of the one it replaces.
func TestWriteAttemptRead(t *testing.T) {
	t.Parallel()
	owner := machine("m")
	c := newClient(t, owner)
	files := machineFiles(t, "#cloud-config\n")
	rec := Record{Files: files, Image: "ghcr.io/x/m:v1", Digest: "ghcr.io/x/m@sha256:aaa", Identity: "id", IdentityKind: "Secret", InputsHash: "h1:a", Job: "j1"}
	if err := WriteAttempt(ctx, c, owner, rec); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	d, err := Read(ctx, c, ns, "m", "m")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := rec
	want.Digest = ""
	if d.Attempt == nil || !bytes.Equal(d.Attempt.Files.MainTF, files.MainTF) || !bytes.Equal(d.Attempt.Files.TFVars, files.TFVars) {
		t.Fatalf("files do not round-trip: %+v", d.Attempt)
	}
	got := *d.Attempt
	got.Files = files
	if got.Image != want.Image || got.Digest != "" || got.Identity != want.Identity || got.IdentityKind != want.IdentityKind ||
		got.InputsHash != want.InputsHash || got.Job != want.Job || got.MayHaveApplied {
		t.Errorf("attempt = %+v, want %+v", got, want)
	}
	if d.Applied != nil || d.AppliedOrAttempt() != d.Attempt || d.LastAttempt() != d.Attempt {
		t.Errorf("applied = %+v, want none", d.Applied)
	}
	if d.Secret.Name != "captf-inputs-m-m" || d.Secret.ResourceVersion == "" || len(d.Secret.OwnerReferences) != 1 || d.Secret.OwnerReferences[0].UID != owner.UID {
		t.Errorf("Secret metadata = %+v", d.Secret)
	}
	s := getSecret(t, c, "captf-inputs-m-m")
	if s.Type != corev1.SecretTypeOpaque || !slices.Equal(dataKeys(s), []string{MainTFKey, TFVarsKey}) {
		t.Errorf("type %s, data keys %v", s.Type, dataKeys(s))
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-m" || s.OwnerReferences[0].BlockOwnerDeletion != nil || s.OwnerReferences[0].Kind != "TerraformMachine" {
		t.Errorf("ownerRefs = %+v", s.OwnerReferences)
	}
	if s.Labels[state.ManagedLabel] != "true" || s.Labels[state.OwnerKindLabel] != "TerraformMachine" || s.Labels[state.OwnerNameLabel] != "m" {
		t.Errorf("labels = %v", s.Labels)
	}
	if _, ok := s.Annotations[ImageDigestAnnotation]; ok {
		t.Errorf("the attempt record carries a digest: %v", s.Annotations)
	}

	if err := SetMayHaveApplied(ctx, c, owner); err != nil {
		t.Fatal(err)
	}
	if d, _ = Read(ctx, c, ns, "m", "m"); !d.Attempt.MayHaveApplied {
		t.Error("SetMayHaveApplied did not mark the attempt record")
	}
	if err := WriteAttempt(ctx, c, owner, Record{Files: machineFiles(t, "#cloud-config\nnew\n"), Image: "ghcr.io/x/m:v2", Identity: "id2", InputsHash: "h1:b", Job: "j2"}); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	d, _ = Read(ctx, c, ns, "m", "m")
	if a := d.Attempt; a.Image != "ghcr.io/x/m:v2" || a.Identity != "id2" || a.IdentityKind != "" || a.InputsHash != "h1:b" || a.Job != "j2" || a.MayHaveApplied {
		t.Errorf("after rewrite attempt = %+v", a)
	}
	if err := SetMayHaveApplied(ctx, c, owner); err != nil {
		t.Fatal(err)
	}
	if err := ClearMayHaveApplied(ctx, c, owner); err != nil {
		t.Fatal(err)
	}
	if d, _ = Read(ctx, c, ns, "m", "m"); d.Attempt.MayHaveApplied {
		t.Error("ClearMayHaveApplied left the mark")
	}
}

// TestPromote proves Promote writes the applied record to a Secret of its
// own, owned and labeled like the durable one, with its digest, and
// replaces it whole on the next promotion (a digest included); it leaves
// the attempt record alone, and Read returns the applied record also
// without a durable Secret.
func TestPromote(t *testing.T) {
	t.Parallel()
	owner := machine("m")
	c := newClient(t, owner)
	if err := WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "b"), "j2")); err != nil {
		t.Fatal(err)
	}
	applied := Record{Files: machineFiles(t, "a"), Image: "r:v1", Digest: "r@sha256:1", Identity: "id", InputsHash: "h1:j1", Job: "j1"}
	if err := Promote(ctx, c, owner, applied); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	d, err := Read(ctx, c, ns, "m", "m")
	if err != nil {
		t.Fatal(err)
	}
	if d.Applied == nil || d.Applied.Job != "j1" || d.Applied.Digest != "r@sha256:1" || d.Applied.InputsHash != "h1:j1" ||
		!bytes.Equal(d.Applied.Files.TFVars, applied.Files.TFVars) {
		t.Errorf("applied = %+v", d.Applied)
	}
	if d.Attempt == nil || d.Attempt.Job != "j2" || d.AppliedOrAttempt() != d.Applied {
		t.Errorf("attempt = %+v; AppliedOrAttempt must prefer the applied record", d.Attempt)
	}
	if d.AppliedSecret.Name != "captf-applied-m-m" {
		t.Errorf("applied Secret metadata = %+v", d.AppliedSecret)
	}
	s := getSecret(t, c, AppliedName("m", "m"))
	if s.Type != corev1.SecretTypeOpaque || !slices.Equal(dataKeys(s), []string{MainTFKey, TFVarsKey}) {
		t.Errorf("type %s, data keys %v", s.Type, dataKeys(s))
	}
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-m" || s.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Errorf("ownerRefs = %+v", s.OwnerReferences)
	}
	if s.Labels[state.ManagedLabel] != "true" || s.Labels[state.OwnerKindLabel] != "TerraformMachine" || s.Labels[state.OwnerNameLabel] != "m" {
		t.Errorf("labels = %v", s.Labels)
	}

	next := attempt(machineFiles(t, "b"), "j2")
	if err := Promote(ctx, c, owner, next); err != nil {
		t.Fatalf("Promote: %v", err)
	}
	if d, _ = Read(ctx, c, ns, "m", "m"); d.Applied.Job != "j2" || d.Applied.Digest != "" {
		t.Errorf("after a second promotion applied = %+v, want j2 without a digest", d.Applied)
	}

	if err := state.DeleteSecretNamed(ctx, c, ns, Name("m", "m")); err != nil {
		t.Fatal(err)
	}
	d, err = Read(ctx, c, ns, "m", "m")
	if err != nil || d.Attempt != nil || d.Applied == nil || d.Secret.Name != "" || d.AppliedOrAttempt() != d.Applied {
		t.Errorf("Read without the durable Secret = %+v, %v; want the applied record alone", d, err)
	}
}

// TestMarkApplied: MarkApplied needs the Secret, sets the marker (again
// without harm) and leaves the rest alone, and Read reports it; a
// WriteAttempt keeps it.
func TestMarkApplied(t *testing.T) {
	t.Parallel()
	owner := machine("m")
	c := newClient(t, owner)
	if err := MarkApplied(ctx, c, owner); !errors.Is(err, ErrNotFound) {
		t.Errorf("MarkApplied without Secret: err = %v", err)
	}
	if err := WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "x"), "j1")); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	if d, _ := Read(ctx, c, ns, "m", "m"); d.AppliedMark {
		t.Error("a fresh Secret reads applied")
	}
	for i := range 2 {
		if err := MarkApplied(ctx, c, owner); err != nil {
			t.Fatalf("MarkApplied #%d: %v", i+1, err)
		}
		if d, _ := Read(ctx, c, ns, "m", "m"); !d.AppliedMark || d.Attempt.Job != "j1" {
			t.Errorf("after MarkApplied #%d: %+v", i+1, d)
		}
	}
	if err := WriteAttempt(ctx, c, owner, Record{Files: machineFiles(t, "y"), Image: "r:v2", Job: "j2"}); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	d, err := Read(ctx, c, ns, "m", "m")
	if err != nil || !d.AppliedMark {
		t.Errorf("after a rewrite: %+v, %v; want applied", d, err)
	}
}

// TestInterruptedApply: SetInterruptedApply records the unconfirmed apply
// Job on the durable Secret, Read reports it, a WriteAttempt keeps it,
// and ClearInterruptedApply removes it (twice without harm); both report a
// missing Secret as ErrNotFound, as SetMayHaveApplied does.
func TestInterruptedApply(t *testing.T) {
	t.Parallel()
	owner := machine("m")
	c := newClient(t, owner)
	if err := SetInterruptedApply(ctx, c, owner, "j1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetInterruptedApply without Secret: err = %v", err)
	}
	if err := ClearInterruptedApply(ctx, c, owner); !errors.Is(err, ErrNotFound) {
		t.Errorf("ClearInterruptedApply without Secret: err = %v", err)
	}
	if err := SetMayHaveApplied(ctx, c, owner); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetMayHaveApplied without Secret: err = %v", err)
	}
	if err := WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "x"), "j1")); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	if d, _ := Read(ctx, c, ns, "m", "m"); d.InterruptedApply != "" {
		t.Errorf("a fresh Secret names interrupted apply %q", d.InterruptedApply)
	}
	if err := SetInterruptedApply(ctx, c, owner, "j1"); err != nil {
		t.Fatal(err)
	}
	if err := WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "y"), "j2")); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	if d, _ := Read(ctx, c, ns, "m", "m"); d.InterruptedApply != "j1" {
		t.Errorf("interrupted apply after WriteAttempt = %q, want j1", d.InterruptedApply)
	}
	for i := range 2 {
		if err := ClearInterruptedApply(ctx, c, owner); err != nil {
			t.Fatalf("ClearInterruptedApply #%d: %v", i+1, err)
		}
		s := getSecret(t, c, Name("m", "m"))
		if _, ok := s.Annotations[InterruptedApplyAnnotation]; ok || s.Annotations[ImageAnnotation] != "r:v1" {
			t.Errorf("annotations after ClearInterruptedApply #%d: %v", i+1, s.Annotations)
		}
	}
}

// TestWriteOwnsTheWholeSecret: WriteAttempt leaves exactly one ownerRef
// and exactly two data keys even when the Secret had more; foreign labels
// survive.
func TestWriteOwnsTheWholeSecret(t *testing.T) {
	t.Parallel()
	owner := machine(strings.Repeat("n", 70))
	pre := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: Name("m", owner.Name),
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "x", UID: "foreign"}},
			Labels:          map[string]string{"keep": "me"},
		},
		Data: map[string][]byte{"stale": []byte("x"), MainTFKey: []byte("old")},
	}
	c := newClient(t, owner, pre)
	if err := WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "x"), "j1")); err != nil {
		t.Fatalf("WriteAttempt: %v", err)
	}
	s := getSecret(t, c, pre.Name)
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-m" {
		t.Errorf("ownerRefs = %+v, want only the owner", s.OwnerReferences)
	}
	if !slices.Equal(dataKeys(s), []string{MainTFKey, TFVarsKey}) {
		t.Errorf("data keys = %v", dataKeys(s))
	}
	if s.Labels["keep"] != "me" {
		t.Error("foreign label dropped")
	}
	for k, v := range s.Labels {
		if len(validation.IsValidLabelValue(v)) != 0 {
			t.Errorf("label %s has an invalid value (%d chars)", k, len(v))
		}
	}
}

// TestReadAndDeleteErrors proves Read reports ErrNotFound when both
// Secrets are missing, Delete tolerates them missing and removes both,
// that WriteAttempt on an owner kind unknown to
// state reports state.ErrUnknownKind, that Delete on an object the scheme
// does not resolve errors, and that a non-NotFound Get failure from Read is
// wrapped rather than reported as ErrNotFound.
func TestReadAndDeleteErrors(t *testing.T) {
	t.Parallel()
	owner := machine("m")
	c := newClient(t, owner)
	if _, err := Read(ctx, c, ns, "m", "m"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read missing: err = %v", err)
	}
	for range 2 {
		if err := Delete(ctx, c, owner); err != nil {
			t.Errorf("Delete: %v", err)
		}
	}
	_ = WriteAttempt(ctx, c, owner, attempt(machineFiles(t, "x"), "j1"))
	_ = Promote(ctx, c, owner, attempt(machineFiles(t, "x"), "j1"))
	if err := Delete(ctx, c, owner); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := Read(ctx, c, ns, "m", "m"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Read after Delete: err = %v", err)
	}
	// Owners without an inputs Secret, or unknown to the scheme.
	tmpl := &infrav1.TerraformMachineTemplate{ObjectMeta: metav1.ObjectMeta{Name: "t", Namespace: ns}}
	if err := WriteAttempt(ctx, c, tmpl, Record{}); !errors.Is(err, state.ErrUnknownKind) {
		t.Errorf("template owner: err = %v", err)
	}
	if err := Delete(ctx, c, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "x", Namespace: ns}}); err == nil {
		t.Error("ConfigMap owner accepted")
	}
	// API errors other than NotFound are wrapped, not ErrNotFound.
	failing := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("boom")
		},
	}).Build()
	if _, err := Read(ctx, failing, ns, "m", "m"); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get failure: err = %v", err)
	}
}

// TestRunSecret proves CreateRun is idempotent and sets the Job owner
// reference, labels, data and the record's annotations, that ReadRun
// reads the record back under the Job's name, and that DeleteRun removes
// the Secret and reports true only on the call that deleted it.
func TestRunSecret(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "captf-m-m-apply-a1-abc123", Namespace: ns, UID: "uid-job"}}
	c := newClient(t, job)
	files := machineFiles(t, "x")
	if _, err := ReadRun(ctx, c, job); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadRun without Secret: err = %v", err)
	}
	rec := Record{Files: files, Image: "r:v1", Digest: "r@sha256:1", Identity: "id", InputsHash: "h1:a", Job: "ignored", MayHaveApplied: true}
	for range 2 { // AlreadyExists is fine
		if err := CreateRun(ctx, c, job, rec); err != nil {
			t.Fatalf("CreateRun: %v", err)
		}
	}
	got, err := ReadRun(ctx, c, job)
	if err != nil || got.Job != job.Name || got.Image != "r:v1" || got.Identity != "id" || got.InputsHash != "h1:a" ||
		got.Digest != "" || got.MayHaveApplied || !bytes.Equal(got.Files.TFVars, files.TFVars) {
		t.Errorf("ReadRun = %+v, %v", got, err)
	}
	other := job.DeepCopy()
	other.UID = "uid-other"
	if _, err := ReadRun(ctx, c, other); !errors.Is(err, ErrNotFound) {
		t.Errorf("ReadRun of another Job of that name: err = %v, want ErrNotFound", err)
	}
	s := getSecret(t, c, RunName(job.Name))
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].Kind != "Job" || s.OwnerReferences[0].UID != "uid-job" || s.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Errorf("ownerRefs = %+v", s.OwnerReferences)
	}
	if !slices.Equal(dataKeys(s), []string{MainTFKey, TFVarsKey}) || !bytes.Equal(s.Data[TFVarsKey], files.TFVars) || s.Labels[state.ManagedLabel] != "true" {
		t.Errorf("per-run Secret data keys %v, labels %v", dataKeys(s), s.Labels)
	}
	for i := range 2 {
		deleted, err := DeleteRun(ctx, c, job)
		if err != nil {
			t.Fatalf("DeleteRun: %v", err)
		}
		if deleted != (i == 0) {
			t.Errorf("call %d: deleted = %v, want true only the first time", i, deleted)
		}
	}
	if err := c.Get(ctx, client.ObjectKeyFromObject(s), &corev1.Secret{}); err == nil {
		t.Error("per-run Secret survived DeleteRun")
	}
}

// TestRunSecretReplacesStale shows that a per-run Secret left by an earlier
// Job of the same name (a different UID) is replaced, not adopted.
func TestRunSecretReplacesStale(t *testing.T) {
	t.Parallel()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "captf-m-m-apply-a1-abc123", Namespace: ns, UID: "uid-new"}}
	stale := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: RunName(job.Name), Namespace: ns, UID: "uid-stale-secret",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: "uid-old"}},
	}, Data: map[string][]byte{"old": []byte("x")}}
	c := newClient(t, job, stale)
	files := machineFiles(t, "x")
	if err := CreateRun(ctx, c, job, Record{Files: files}); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	s := getSecret(t, c, RunName(job.Name))
	if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-new" || !bytes.Equal(s.Data[TFVarsKey], files.TFVars) || s.Data["old"] != nil {
		t.Errorf("per-run Secret not replaced: owners %+v, keys %v", s.OwnerReferences, dataKeys(s))
	}
}

// TestLastControlPlaneInitialized proves LastControlPlaneInitialized reads
// control_plane_initialized from a Record's tfvars and is false for a nil
// Record, tfvars without the key or unparsable tfvars.
func TestLastControlPlaneInitialized(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		d    *Record
		want bool
	}{
		{"nil", nil, false},
		{"true", &Record{Files: clusterFiles(t, true)}, true},
		{"false", &Record{Files: clusterFiles(t, false)}, false},
		{"machine tfvars have no key", &Record{Files: machineFiles(t, "x")}, false},
		{"unparsable", &Record{Files: render.Files{TFVars: []byte("{")}}, false},
	}
	for _, c := range cases {
		if got := LastControlPlaneInitialized(c.d); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// TestLastControlPlaneEndpointNull proves LastControlPlaneEndpointNull is
// true for a null or absent control_plane_endpoint, false once it is set,
// false for a nil Record or unparsable tfvars, and true for freshly
// rendered cluster tfvars before the first apply.
func TestLastControlPlaneEndpointNull(t *testing.T) {
	t.Parallel()
	tfvars := func(s string) *Record { return &Record{Files: render.Files{TFVars: []byte(s)}} }
	cases := []struct {
		name string
		d    *Record
		want bool
	}{
		{"nil: no apply yet", nil, false},
		{"null", tfvars(`{"control_plane_endpoint":null}`), true},
		{"absent", tfvars(`{}`), true},
		{"set", tfvars(`{"control_plane_endpoint":{"host":"h","port":6443}}`), false},
		{"unparsable", tfvars(`{`), false},
		{"rendered cluster files", &Record{Files: clusterFiles(t, false)}, true},
	}
	for _, c := range cases {
		if got := LastControlPlaneEndpointNull(c.d); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
