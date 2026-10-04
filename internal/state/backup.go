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
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Backup labels. They never include the backend's own labels (tfstate,
// tfstateSecretSuffix, tfstateWorkspace): the backend and Selector would
// then take a backup for a chunk of the live state.
const (
	// BackupLabel marks a state backup Secret.
	BackupLabel = "captf.io/state-backup"
	// BackupSuffixLabel is the state suffix a backup was taken from; it
	// selects one object's backups.
	BackupSuffixLabel = "captf.io/state-backup-suffix"
)

// Backup annotations. A backup is a set of Secrets (one per source chunk)
// sharing BackupSetAnnotation.
const (
	BackupSerialAnnotation    = "captf.io/state-backup-serial"
	BackupLineageAnnotation   = "captf.io/state-backup-lineage"
	BackupTakenAtAnnotation   = "captf.io/state-backup-taken-at"
	BackupSourceJobAnnotation = "captf.io/state-backup-source-job"
	// BackupDigestAnnotation is the hex sha256 of the concatenated
	// compressed payload: two states of one serial differ in it (a restore
	// continues an older serial, so the next serials repeat).
	BackupDigestAnnotation = "captf.io/state-backup-digest"
	// BackupResourcesAnnotation is the state's managed resource count, the
	// restore Job's sanity check.
	BackupResourcesAnnotation = "captf.io/state-backup-resources"
	BackupSetAnnotation       = "captf.io/state-backup-set"
	BackupChunkAnnotation     = "captf.io/state-backup-chunk"
	BackupChunksAnnotation    = "captf.io/state-backup-chunks"
)

// backupPrefix starts every backup Secret name.
const backupPrefix = "captf-state-backup-"

// BackupName returns the base Secret name of the backup of serial:
// captf-state-backup-<suffix>-<serial>. Chunks add "-part-N" like the
// backend's. With a 19-character suffix and a 19-digit serial the longest
// chunk name is under 90 characters, whatever the object's name.
func BackupName(suffix string, serial int64) string {
	return backupPrefix + suffix + "-" + strconv.FormatInt(serial, 10)
}

// chunkName returns the name of chunk i of the backup set base.
func chunkName(base string, i int) string {
	if i == 0 {
		return base
	}
	return base + "-part-" + strconv.Itoa(i)
}

// BackupSelector returns a selector matching every backup Secret of suffix.
func BackupSelector(suffix string) labels.Selector {
	return labels.SelectorFromSet(labels.Set{BackupLabel: "true", BackupSuffixLabel: suffix})
}

// Backup is one backup set.
type Backup struct {
	// Name is the base Secret's name (BackupSetAnnotation).
	Name       string
	Serial     int64
	Lineage    string
	TakenAt    time.Time
	Digest     string
	InputsHash string
	SourceJob  string
	// ManagedResources is the managed resource count of the state.
	ManagedResources int
	// Bytes is the compressed size summed over the chunks.
	Bytes int
	// Secrets are the chunk Secret names in chunk order.
	Secrets []string
	// Complete is false when a chunk is missing or the set is malformed: a
	// partial backup (a failed create, a hand-deleted chunk) is never
	// restored.
	Complete bool
}

// ErrBackupConflict means a backup Secret of the name about to be written
// exists with other content (a set left incomplete, then its source state
// changed); the Secret is left alone.
var ErrBackupConflict = errors.New("state: backup Secret exists with other content")

// BackupOptions describe the object a backup is taken for.
type BackupOptions struct {
	// Owner is the Terraform* object: the backups' controller-less owner,
	// so they are garbage collected with it, and not with the state. The
	// reference is set at creation; the reconciler re-owns a backup that
	// lost it or names Owner's earlier UID (after a management-cluster
	// restore) when it finds the object's other Secrets misowned.
	Owner       client.Object
	OwnerKind   string
	ClusterName string
	Suffix      string
	// SourceJob is the Job whose run the backup follows, if known.
	SourceJob string
	Now       time.Time
}

// TakeBackup copies the current state Secrets of o.Suffix verbatim into a
// backup set, using ctx and c to list, read and create the Secrets, and
// returns the backup and whether it created one. A state already backed up
// with the same serial and content creates nothing. A different state with
// the serial of an existing backup (after a restore) is stored under
// BackupName plus a digest, next to it.
//
// The state is parsed before copying, with the reader's caps: an encrypted,
// corrupt, inconsistent or oversized state is not backed up, and the error
// says why (the reader's sentinel errors).
func TakeBackup(ctx context.Context, c client.Client, o BackupOptions) (Backup, bool, error) {
	ns := o.Owner.GetNamespace()
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(ns), client.MatchingLabelsSelector{Selector: Selector(o.Suffix)}); err != nil {
		return Backup{}, false, fmt.Errorf("state: list Secrets: %w", err)
	}
	if len(list.Items) == 0 {
		return Backup{}, false, ErrNoState
	}
	chunks, err := orderChunks(list.Items, o.Suffix)
	if err != nil {
		return Backup{}, false, err
	}
	var payload []byte
	for _, ch := range chunks {
		data, ok := ch.secret.Data[DataKey]
		if !ok {
			return Backup{}, false, fmt.Errorf("%w: Secret %s has no %q key", ErrStateInconsistent, ch.secret.Name, DataKey)
		}
		payload = append(payload, data...)
	}
	st, err := parse(payload, MaxStateBytes)
	if err != nil {
		return Backup{}, false, err
	}
	if st.Serial < 1 {
		return Backup{}, false, fmt.Errorf("%w: serial %d", ErrStateInconsistent, st.Serial)
	}
	sum := sha256.Sum256(payload)
	digest := hex.EncodeToString(sum[:])

	existing, err := ListBackups(ctx, c, ns, o.Suffix)
	if err != nil {
		return Backup{}, false, err
	}
	name := BackupName(o.Suffix, st.Serial)
	for _, b := range existing {
		if b.Serial == st.Serial && b.Digest == digest && b.Complete {
			return b, false, nil
		}
	}
	// A set of this name with the same digest is an interrupted earlier
	// pass: completing it is what createBackupSecret tolerates.
	if slices.ContainsFunc(existing, func(b Backup) bool { return b.Name == name && b.Digest != digest }) {
		name += "-" + digest[:8]
	}

	b := Backup{
		Name: name, Serial: st.Serial, Lineage: st.Lineage, TakenAt: o.Now.UTC().Truncate(time.Second), Digest: digest,
		InputsHash: chunks[0].secret.Annotations[InputsHashAnnotation], SourceJob: o.SourceJob,
		ManagedResources: st.ManagedResources, Bytes: len(payload), Complete: true,
	}
	for i, ch := range chunks {
		s := backupSecret(o, b, i, len(chunks), ch.secret)
		if err := controllerutil.SetOwnerReference(o.Owner, s, c.Scheme(), noBlockOwnerDeletion); err != nil {
			return Backup{}, false, fmt.Errorf("state: owner reference on %s: %w", s.Name, err)
		}
		if err := createBackupSecret(ctx, c, s, digest); err != nil {
			return Backup{}, false, err
		}
		b.Secrets = append(b.Secrets, s.Name)
	}
	return b, true, nil
}

// backupSecret returns chunk i of n of b, a verbatim copy of src's data,
// labeled for o's owner and object identity.
func backupSecret(o BackupOptions, b Backup, i, n int, src *corev1.Secret) *corev1.Secret {
	lbls := BackendLabels(o.OwnerKind, o.Owner.GetName(), o.ClusterName)
	lbls[BackupLabel] = "true"
	lbls[BackupSuffixLabel] = o.Suffix
	ann := map[string]string{
		BackupSerialAnnotation:    strconv.FormatInt(b.Serial, 10),
		BackupLineageAnnotation:   b.Lineage,
		BackupTakenAtAnnotation:   b.TakenAt.Format(time.RFC3339),
		BackupDigestAnnotation:    b.Digest,
		BackupResourcesAnnotation: strconv.Itoa(b.ManagedResources),
		BackupSetAnnotation:       b.Name,
		BackupChunkAnnotation:     strconv.Itoa(i),
		BackupChunksAnnotation:    strconv.Itoa(n),
	}
	if b.SourceJob != "" {
		ann[BackupSourceJobAnnotation] = b.SourceJob
	}
	if i == 0 && b.InputsHash != "" {
		ann[InputsHashAnnotation] = b.InputsHash
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: o.Owner.GetNamespace(), Name: chunkName(b.Name, i), Labels: lbls, Annotations: ann,
		},
		Type: corev1.SecretTypeOpaque,
		Data: maps.Clone(src.Data),
	}
}

// createBackupSecret creates s through c, using ctx for the call, and
// returns any error. One that exists already with the same digest is a
// leftover of an interrupted earlier pass and is kept; one that exists with
// a different digest is ErrBackupConflict.
func createBackupSecret(ctx context.Context, c client.Client, s *corev1.Secret, digest string) error {
	err := c.Create(ctx, s)
	if err == nil {
		return nil
	}
	if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("state: create backup %s: %w", s.Name, err)
	}
	var got corev1.Secret
	if err := c.Get(ctx, client.ObjectKeyFromObject(s), &got); err != nil {
		return fmt.Errorf("state: get backup %s: %w", s.Name, err)
	}
	if got.Annotations[BackupDigestAnnotation] != digest {
		return fmt.Errorf("%w: %s", ErrBackupConflict, s.Name)
	}
	return nil
}

// ListBackups lists namespace with c using ctx and returns the backup sets
// of suffix, newest first (by takenAt, then serial, then name).
func ListBackups(ctx context.Context, c client.Reader, namespace, suffix string) ([]Backup, error) {
	var list corev1.SecretList
	if err := c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: BackupSelector(suffix)}); err != nil {
		return nil, fmt.Errorf("state: list backups: %w", err)
	}
	sets := map[string][]*corev1.Secret{}
	for i := range list.Items {
		s := &list.Items[i]
		set := cmp.Or(s.Annotations[BackupSetAnnotation], s.Name)
		sets[set] = append(sets[set], s)
	}
	out := make([]Backup, 0, len(sets))
	for name, secrets := range sets {
		out = append(out, backupOf(name, secrets))
	}
	slices.SortFunc(out, func(a, b Backup) int {
		return cmp.Or(b.TakenAt.Compare(a.TakenAt), cmp.Compare(b.Serial, a.Serial), cmp.Compare(a.Name, b.Name))
	})
	return out, nil
}

// backupOf returns the backup set name assembled from its chunk secrets.
func backupOf(name string, secrets []*corev1.Secret) Backup {
	b := Backup{Name: name}
	byIndex := map[int]*corev1.Secret{}
	want := -1
	for _, s := range secrets {
		i, err := strconv.Atoi(s.Annotations[BackupChunkAnnotation])
		if err != nil || i < 0 || i >= MaxChunks || s.Name != chunkName(name, i) {
			continue
		}
		byIndex[i] = s
		if n, err := strconv.Atoi(s.Annotations[BackupChunksAnnotation]); err == nil {
			want = n
		}
	}
	base, ok := byIndex[0]
	if !ok {
		// Metadata from any chunk, so the incomplete set is still listed
		// (and pruned).
		base = secrets[0]
	}
	a := base.Annotations
	b.Serial, _ = strconv.ParseInt(a[BackupSerialAnnotation], 10, 64)
	b.Lineage = a[BackupLineageAnnotation]
	b.Digest = a[BackupDigestAnnotation]
	b.InputsHash = a[InputsHashAnnotation]
	b.SourceJob = a[BackupSourceJobAnnotation]
	b.ManagedResources, _ = strconv.Atoi(a[BackupResourcesAnnotation])
	if t, err := time.Parse(time.RFC3339, a[BackupTakenAtAnnotation]); err == nil {
		b.TakenAt = t
	} else {
		b.TakenAt = base.CreationTimestamp.Time
	}
	b.Complete = want >= 1 && want <= MaxChunks && len(byIndex) == want && len(secrets) == want && b.Serial >= 1
	for i := range max(want, 0) {
		s, ok := byIndex[i]
		if !ok {
			b.Complete = false
			break
		}
		b.Secrets = append(b.Secrets, s.Name)
		b.Bytes += len(s.Data[DataKey])
	}
	if !b.Complete {
		b.Secrets = nil
		for _, s := range secrets {
			b.Secrets = append(b.Secrets, s.Name)
		}
		slices.Sort(b.Secrets)
	}
	return b
}

// FindBackup returns the newest complete backup of serial in backups
// (newest first), if any.
func FindBackup(backups []Backup, serial int64) (Backup, bool) {
	for _, b := range backups {
		if b.Serial == serial && b.Complete {
			return b, true
		}
	}
	return Backup{}, false
}

// pruneCandidates returns the sets of backups (newest first) PruneBackups
// deletes: complete sets beyond the newest keep, and every incomplete set
// except the newest one when that one is newer than every complete set. A
// set is written chunk by chunk, so the newest set, if incomplete, may still
// be in flight; any incomplete set behind a newer set is a failed write.
func pruneCandidates(backups []Backup, keep int) []Backup {
	var out []Backup
	complete := 0
	for i, b := range backups {
		switch {
		case b.Complete:
			complete++
			if complete > keep {
				out = append(out, b)
			}
		case i == 0:
			// Possibly still being written: left alone.
		default:
			out = append(out, b)
		}
	}
	return out
}

// PruneBackups deletes, in namespace through c using ctx, the backup sets of
// backups (newest first) that no longer earn a slot and returns the ones it
// deleted. Only complete sets count toward keep, so a partial set never
// displaces a good one: complete sets beyond the newest keep are deleted, and
// so is every incomplete set except the newest set overall when it is
// incomplete (it may still be being written). Secrets already gone are fine.
func PruneBackups(ctx context.Context, c client.Client, namespace string, backups []Backup, keep int) ([]Backup, error) {
	if keep < 0 {
		return nil, nil
	}
	var pruned []Backup
	for _, b := range pruneCandidates(backups, keep) {
		for _, name := range b.Secrets {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
			if err := client.IgnoreNotFound(c.Delete(ctx, s)); err != nil {
				return pruned, fmt.Errorf("state: delete backup %s: %w", name, err)
			}
		}
		pruned = append(pruned, b)
	}
	return pruned, nil
}
