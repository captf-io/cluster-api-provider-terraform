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

package state

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	clusterctlv1 "sigs.k8s.io/cluster-api/cmd/clusterctl/api/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// backupT0 is a fixed time used as the "now" of a first backup in these
// tests, so later backups can be taken at deterministic offsets from it.
var backupT0 = time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)

// backupOpts returns BackupOptions for owner and suffix, backing up as of
// now, with a fixed OwnerKind, ClusterName and SourceJob.
func backupOpts(owner client.Object, suffix string, now time.Time) BackupOptions {
	return BackupOptions{Owner: owner, OwnerKind: KindTerraformMachine, ClusterName: "c1", Suffix: suffix, SourceJob: "captf-m-x-apply-a1-abcdef", Now: now}
}

// listSecrets returns every Secret in namespace, read through c; t fails
// the test on a list error.
func listSecrets(t *testing.T, c client.Client, namespace string) []corev1.Secret {
	t.Helper()
	var list corev1.SecretList
	if err := c.List(context.Background(), &list, client.InNamespace(namespace)); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// TestTakeBackupChunked copies a real chunked Terraform state verbatim, one
// backup Secret per chunk, and a second pass on the unchanged state takes
// nothing.
func TestTakeBackupChunked(t *testing.T) {
	t.Parallel()
	secrets, _ := loadFixture(t, "terraform-chunked")
	suffix, _ := Suffix(fixtureNamespace, KindTerraformMachine, "terraform-chunked")
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "terraform-chunked", Namespace: fixtureNamespace, UID: "uid-9"}}
	c := fixtureClient(t, secrets, owner)
	ctx := context.Background()
	st, err := NewReader(c).Read(ctx, fixtureNamespace, suffix)
	if err != nil {
		t.Fatal(err)
	}

	b, created, err := TakeBackup(ctx, c, backupOpts(owner, suffix, backupT0))
	if err != nil || !created {
		t.Fatalf("TakeBackup = %+v, %v, %v", b, created, err)
	}
	if b.Serial != st.Serial || b.Bytes != st.Bytes || len(b.Secrets) != 2 || b.Name != BackupName(suffix, st.Serial) ||
		b.Secrets[1] != b.Name+"-part-1" || b.ManagedResources != st.ManagedResources {
		t.Errorf("backup = %+v, state serial %d bytes %d", b, st.Serial, st.Bytes)
	}
	for i, name := range b.Secrets {
		var s corev1.Secret
		if err := c.Get(ctx, client.ObjectKey{Namespace: fixtureNamespace, Name: name}, &s); err != nil {
			t.Fatal(err)
		}
		src := secrets[0]
		for _, cand := range secrets {
			if cand.Name == chunkName(SecretName(suffix), i) {
				src = cand
			}
		}
		if !bytes.Equal(s.Data[DataKey], src.Data[DataKey]) {
			t.Errorf("%s: data differs from %s", name, src.Name)
		}
		for k, v := range map[string]string{
			BackupLabel: "true", BackupSuffixLabel: suffix, ManagedLabel: "true", OwnerKindLabel: KindTerraformMachine,
			OwnerNameLabel: "terraform-chunked", clusterv1.ClusterNameLabel: "c1", clusterctlv1.ClusterctlMoveLabel: "",
		} {
			if got, ok := s.Labels[k]; !ok || got != v {
				t.Errorf("%s: label %s = %q, want %q", name, k, got, v)
			}
		}
		for _, k := range []string{BackendStateLabel, BackendSuffixLabel, BackendWorkspaceLabel} {
			if _, ok := s.Labels[k]; ok {
				t.Errorf("%s carries the backend label %s: the backend would take it for state", name, k)
			}
		}
		if len(s.OwnerReferences) != 1 || s.OwnerReferences[0].UID != "uid-9" || s.OwnerReferences[0].Controller != nil ||
			s.OwnerReferences[0].BlockOwnerDeletion != nil {
			t.Errorf("%s: ownerRefs %+v", name, s.OwnerReferences)
		}
		if s.Annotations[BackupSourceJobAnnotation] != "captf-m-x-apply-a1-abcdef" || s.Annotations[BackupTakenAtAnnotation] != backupT0.Format(time.RFC3339) ||
			s.Annotations[BackupLineageAnnotation] != st.Lineage {
			t.Errorf("%s: annotations %v", name, s.Annotations)
		}
	}
	// The live state is still exactly what it was.
	if again, err := NewReader(c).Read(ctx, fixtureNamespace, suffix); err != nil || again.Serial != st.Serial || len(again.Secrets) != 2 {
		t.Errorf("state after the backup = %+v, %v", again, err)
	}

	_, created, err = TakeBackup(ctx, c, backupOpts(owner, suffix, backupT0.Add(time.Hour)))
	if err != nil || created {
		t.Errorf("second TakeBackup created %v, %v; want nothing new", created, err)
	}
	list, err := ListBackups(ctx, c, fixtureNamespace, suffix)
	if err != nil || len(list) != 1 || !list[0].Complete || list[0].Bytes != st.Bytes || !list[0].TakenAt.Equal(backupT0) {
		t.Errorf("ListBackups = %+v, %v", list, err)
	}
}

// TestTakeBackupRefusesUnreadable: encrypted, corrupt and missing states
// are not backed up.
func TestTakeBackupRefusesUnreadable(t *testing.T) {
	t.Parallel()
	encrypted, _ := loadFixture(t, "opentofu-encrypted")
	encSuffix, _ := Suffix(fixtureNamespace, KindTerraformMachine, "opentofu-encrypted")
	corruptSuffix, _ := Suffix(ns, KindTerraformMachine, "m1")
	cases := []struct {
		name      string
		namespace string
		suffix    string
		objs      []client.Object
		want      error
	}{
		{"encrypted", fixtureNamespace, encSuffix, []client.Object{encrypted[0]}, ErrStateEncrypted},
		{"corrupt", ns, corruptSuffix, []client.Object{secret(corruptSuffix, SecretName(corruptSuffix), []byte("not gzip"))}, ErrStateCorrupt},
		{"inconsistent", ns, corruptSuffix, chunked(t, corruptSuffix, stateJSON, 3)[1:], ErrStateInconsistent},
		{"none", ns, corruptSuffix, nil, ErrNoState},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: tc.namespace, UID: "uid-1"}}
			c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(append(tc.objs, owner)...).Build()
			_, created, err := TakeBackup(context.Background(), c, backupOpts(owner, tc.suffix, backupT0))
			if !errors.Is(err, tc.want) || created {
				t.Errorf("TakeBackup = %v, %v; want %v", created, err, tc.want)
			}
			if list, _ := ListBackups(context.Background(), c, tc.namespace, tc.suffix); len(list) != 0 {
				t.Errorf("backups = %+v, want none", list)
			}
		})
	}
}

// TestTakeBackupSameSerialOtherContent: after a restore the backend
// continues an older serial, so a new state can repeat a backed-up serial;
// it is stored next to the old backup, and FindBackup picks the newer.
func TestTakeBackupSameSerialOtherContent(t *testing.T) {
	t.Parallel()
	suffix, _ := Suffix(ns, KindTerraformMachine, "m1")
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns, UID: "uid-1"}}
	base := secret(suffix, SecretName(suffix), gz(t, stateJSON))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(owner, base).Build()
	ctx := context.Background()
	first, _, err := TakeBackup(ctx, c, backupOpts(owner, suffix, backupT0))
	if err != nil {
		t.Fatal(err)
	}
	var live corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(base), &live); err != nil {
		t.Fatal(err)
	}
	live.Data[DataKey] = gz(t, strings.Replace(stateJSON, `"lineage":"abc"`, `"lineage":"abc","check_results":null`, 1))
	if err := c.Update(ctx, &live); err != nil {
		t.Fatal(err)
	}
	second, created, err := TakeBackup(ctx, c, backupOpts(owner, suffix, backupT0.Add(time.Minute)))
	if err != nil || !created || second.Serial != first.Serial || second.Name == first.Name ||
		!strings.HasPrefix(second.Name, first.Name+"-") || second.Digest == first.Digest {
		t.Fatalf("second = %+v (created %v, %v), first %+v", second, created, err, first)
	}
	list, err := ListBackups(ctx, c, ns, suffix)
	if err != nil || len(list) != 2 {
		t.Fatalf("ListBackups = %+v, %v", list, err)
	}
	if b, ok := FindBackup(list, 7); !ok || b.Name != second.Name {
		t.Errorf("FindBackup(7) = %+v, %v; want %s", b, ok, second.Name)
	}
	if _, ok := FindBackup(list, 8); ok {
		t.Error("FindBackup(8) found a backup")
	}
}

// TestPruneBackups keeps the newest sets and deletes every chunk of the
// others; an incomplete set is listed, never found, and pruned like any.
func TestPruneBackups(t *testing.T) {
	t.Parallel()
	suffix, _ := Suffix(ns, KindTerraformMachine, "m1")
	owner := &infrav1.TerraformMachine{ObjectMeta: metav1.ObjectMeta{Name: "m1", Namespace: ns, UID: "uid-1"}}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(owner).Build()
	ctx := context.Background()
	for serial := range 4 {
		// Two chunks each: pruning must take both.
		for _, o := range chunked(t, suffix, strings.Replace(stateJSON, `"serial":7`, `"serial":`+string(rune('1'+serial)), 1), 2) {
			if err := c.Create(ctx, o); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := TakeBackup(ctx, c, backupOpts(owner, suffix, backupT0.Add(time.Duration(serial)*time.Hour))); err != nil {
			t.Fatal(err)
		}
		if err := Cleanup(ctx, c, ns, suffix); err != nil {
			t.Fatal(err)
		}
	}
	// Serial 2 loses a chunk.
	if err := c.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: BackupName(suffix, 2) + "-part-1"}}); err != nil {
		t.Fatal(err)
	}
	list, err := ListBackups(ctx, c, ns, suffix)
	if err != nil || len(list) != 4 || list[0].Serial != 4 || list[3].Serial != 1 || list[2].Complete {
		t.Fatalf("ListBackups = %+v, %v", list, err)
	}
	if _, ok := FindBackup(list, 2); ok {
		t.Error("an incomplete backup was found for a restore")
	}
	pruned, err := PruneBackups(ctx, c, ns, list, 2)
	if err != nil || len(pruned) != 2 || pruned[0].Serial != 2 || pruned[1].Serial != 1 {
		t.Fatalf("PruneBackups = %+v, %v", pruned, err)
	}
	left, _ := ListBackups(ctx, c, ns, suffix)
	if len(left) != 2 || left[0].Serial != 4 || left[1].Serial != 3 {
		t.Errorf("left = %+v", left)
	}
	if n := len(listSecrets(t, c, ns)); n != 4 {
		t.Errorf("%d Secrets left, want the 4 chunks of serials 3 and 4", n)
	}
	if again, err := PruneBackups(ctx, c, ns, left, 2); err != nil || len(again) != 0 {
		t.Errorf("pruning within the limit = %+v, %v", again, err)
	}
}

// TestPruneCandidates: only complete sets take retention slots; incomplete
// sets behind a newer set are pruned and the newest one is kept only when it
// is the newest set overall.
func TestPruneCandidates(t *testing.T) {
	t.Parallel()
	set := func(name string, complete bool) Backup { return Backup{Name: name, Complete: complete} }
	tests := []struct {
		name    string
		backups []Backup
		keep    int
		want    []string
	}{
		{"partial then two complete keeps both complete", []Backup{set("p", false), set("a", true), set("b", true)}, 2, nil},
		{"partial between completes is pruned", []Backup{set("a", true), set("p", false), set("b", true)}, 2, []string{"p"}},
		{"old partials pruned", []Backup{set("a", true), set("b", true), set("p1", false), set("p2", false)}, 2, []string{"p1", "p2"}},
		{"third complete pruned", []Backup{set("a", true), set("b", true), set("c", true)}, 2, []string{"c"}},
		{"all incomplete keeps only the newest", []Backup{set("p1", false), set("p2", false), set("p3", false)}, 2, []string{"p2", "p3"}},
		{"newer partials behind the newest are pruned", []Backup{set("p1", false), set("p2", false), set("a", true)}, 1, []string{"p2"}},
		{"keep zero keeps only an in-flight set", []Backup{set("p", false), set("a", true)}, 0, []string{"a"}},
		{"empty", nil, 2, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var got []string
			for _, b := range pruneCandidates(tt.backups, tt.keep) {
				got = append(got, b.Name)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("pruneCandidates = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestBackupNameLength: the suffix is a hash, so even a 253-character
// object name gives a short, valid Secret name for the last chunk of the
// largest serial.
func TestBackupNameLength(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{KindTerraformCluster, KindTerraformMachine, KindTerraformMachinePool} {
		suffix, err := Suffix(ns, kind, strings.Repeat("a", 253))
		if err != nil {
			t.Fatal(err)
		}
		name := chunkName(BackupName(suffix, 1<<63-1)+"-0123abcd", MaxChunks-1)
		if len(name) > validation.DNS1123SubdomainMaxLength || len(validation.IsDNS1123Subdomain(name)) != 0 {
			t.Errorf("%s: %q (%d characters) is not a valid Secret name", kind, name, len(name))
		}
	}
}
