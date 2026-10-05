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
	"errors"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/metrics"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// backupState copies the state just read, a serial this object has not
// observed before, into a backup and prunes the backups beyond
// Deps.StateBackups. It runs once per new serial: an unchanged state, or
// a bookkept Job, has the observed serial.
// A backup never fails the reconcile: a state that cannot be backed up
// (encrypted, corrupt, inconsistent, oversized) is logged and counted as
// skipped, and the reconcile goes on. It runs using ctx and bk's newest
// Job as the backup's source. It returns false only after a transient
// failure (an API error), which the caller retries by not recording the
// serial as observed.
func (r *reconciler) backupState(ctx context.Context, bk *Bookkeeping) bool {
	keep := r.d.StateBackups
	if keep <= 0 || r.deleting {
		return true
	}
	logger := klog.FromContext(ctx)
	kind, ns := r.k.Kind(), r.obj.GetNamespace()
	b, created, err := state.TakeBackup(ctx, r.d.Client, state.BackupOptions{
		Owner: r.obj, OwnerKind: kind, ClusterName: ClusterName(r.obj, r.owner), Suffix: r.suffix,
		SourceJob: bk.newestJob, Now: r.d.Clock.Now(),
	})
	switch {
	case errors.Is(err, state.ErrNoState):
		// Gone since it was read: nothing to copy.
		return true
	case errors.Is(err, state.ErrStateEncrypted), errors.Is(err, state.ErrStateCorrupt),
		errors.Is(err, state.ErrStateInconsistent), errors.Is(err, state.ErrUnsupportedStateVersion):
		logger.Info("Not backing up the state", "reason", err.Error())
		r.d.Metrics.StateBackup(kind, metrics.BackupSkipped, 1)
		return true
	case err != nil:
		logger.Error(err, "Backing up the state failed; the next reconcile retries")
		r.d.Metrics.StateBackup(kind, metrics.BackupSkipped, 1)
		return false
	}
	if created {
		logger.Info("Backed up the state", "backup", b.Name, "stateSerial", b.Serial, "chunks", len(b.Secrets), "bytes", b.Bytes)
		r.d.Metrics.StateBackup(kind, metrics.BackupTaken, 1)
		r.d.Emit(r.obj, corev1.EventTypeNormal, EventStateBackedUp, "Backup",
			"Backed up state serial %d (%d bytes in %d Secrets) as %s; restore it with %s=%d",
			b.Serial, b.Bytes, len(b.Secrets), b.Name, infrav1.RestoreStateAnnotation, b.Serial)
	}
	backups, err := state.ListBackups(ctx, r.d.Client, ns, r.suffix)
	if err != nil {
		logger.Error(err, "Listing the state backups failed; pruning waits for the next backup")
		return true
	}
	// The backup a pending restore names is never pruned: a bad state that
	// arrives as a new serial must not push out the one being restored.
	candidates := slices.DeleteFunc(slices.Clone(backups), func(b state.Backup) bool {
		return r.requestedRestore() == b.Serial
	})
	pruned, err := state.PruneBackups(ctx, r.d.Client, ns, candidates, keep)
	r.d.Metrics.StateBackup(kind, metrics.BackupPruned, len(pruned))
	for _, p := range pruned {
		logger.V(LogFlow).Info("Pruned a state backup", "backup", p.Name, "stateSerial", p.Serial)
	}
	if err != nil {
		logger.Error(err, "Pruning the state backups failed; the next backup prunes again")
		return true
	}
	r.recordBackups(slices.DeleteFunc(backups, func(b state.Backup) bool {
		return slices.ContainsFunc(pruned, func(p state.Backup) bool { return p.Name == b.Name })
	}))
	return true
}

// requestedRestore returns the serial the restore annotation names, 0
// when none.
func (r *reconciler) requestedRestore() int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(r.annotation(infrav1.RestoreStateAnnotation)), 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// recordBackups sets status.stateBackups from backups (newest first): the
// complete ones, at most MaxStateBackups.
func (r *reconciler) recordBackups(backups []state.Backup) {
	var out []infrav1.StateBackup
	for _, b := range backups {
		if !b.Complete {
			continue
		}
		if len(out) == infrav1.MaxStateBackups {
			break
		}
		at := metav1.NewTime(b.TakenAt)
		out = append(out, infrav1.StateBackup{Serial: b.Serial, TakenAt: &at, Bytes: int64(b.Bytes)})
	}
	r.st.StateBackups = out
}
