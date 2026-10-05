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
	"strings"
	"testing"

	coordinationv1 "k8s.io/api/coordination/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/captf-io/cluster-api-provider-terraform/internal/jobs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/locks"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// lockLease returns the state lock Lease of the test machine, held as
// lockID by who; t fails the test on error. It also returns the state
// suffix.
func lockLease(t *testing.T, lockID, who string) (*coordinationv1.Lease, string) {
	t.Helper()
	suffix, err := state.Suffix(testNS, state.KindTerraformMachine, testName)
	if err != nil {
		t.Fatal(err)
	}
	holder := lockID
	return &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: testNS, Name: state.LeaseName(suffix),
			Annotations: map[string]string{locks.LockInfoAnnotation: `{"ID":"` + lockID + `","Who":"` + who + `","Operation":"OperationTypeApply"}`},
		},
		Spec: coordinationv1.LeaseSpec{HolderIdentity: &holder},
	}, suffix
}

// TestCheckLockLogs: a stale lock of the object's own runner is logged at
// LogFlow with its lockID and holder pod; a foreign lock at V0 with its
// holder.
func TestCheckLockLogs(t *testing.T) {
	t.Parallel()
	deadPod := jobs.Name("m", testName, jobs.OpApply, 1, "abc123") + "-x7k2p"
	for _, tt := range []struct {
		name      string
		who       string
		verbosity int
		want      []string
	}{
		{name: "stale at LogFlow", who: "runner@" + deadPod, verbosity: LogFlow, want: []string{"force-unlocks it", `lockID="lock-1"`, `holderPod="` + deadPod + `"`}},
		{name: "stale is quiet at V0", who: "runner@" + deadPod, verbosity: 0},
		{name: "foreign at V0", who: "steven@laptop", verbosity: 0, want: []string{"something other than this object's runner", `lockID="lock-1"`, `holder="steven@laptop"`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			lease, suffix := lockLease(t, "lock-1", tt.who)
			e := newEnv(t, world(machine(withFinalizer, notPaused), lease)...)
			logger, u := capture(t, tt.verbosity)
			bk := &Bookkeeping{}
			if err := bk.checkLock(klog.NewContext(t.Context(), logger), e.d, e.kindFor(t, readyOwner), suffix); err != nil {
				t.Fatal(err)
			}
			got := u.GetBuffer().String()
			if tt.want == nil && got != "" {
				t.Errorf("logged %s, want nothing", got)
			}
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("log %s lacks %s", got, w)
				}
			}
		})
	}
}

// TestForeignLockLeaseError: an error reading the Lease again is logged,
// and the lock is still described without its holder.
func TestForeignLockLeaseError(t *testing.T) {
	t.Parallel()
	c := fake.NewClientBuilder().WithInterceptorFuncs(interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return errors.New("apiserver unavailable")
		},
	}).Build()
	logger, u := capture(t, 0)
	msg := foreignLock(klog.NewContext(t.Context(), logger), c, testNS, "s", "lock-1")
	if msg != "The state lock lock-1 is held by something other than this object's runner" {
		t.Errorf("description = %q", msg)
	}
	if got := u.GetBuffer().String(); !strings.Contains(got, "apiserver unavailable") || !strings.Contains(got, `lockID="lock-1"`) {
		t.Errorf("log %s lacks the Lease error", got)
	}
}

// TestMarkBookkeptGoneJob: a Job deleted before its bookkept mark is no
// error, and is logged at LogFlow.
func TestMarkBookkeptGoneJob(t *testing.T) {
	t.Parallel()
	gone := job("a1", jobs.OpApply, jobs.Succeeded, t0)
	bk := &Bookkeeping{unmarked: []finished{{job: &gone, ok: true}}}
	logger, u := capture(t, LogFlow)
	if err := bk.MarkBookkept(klog.NewContext(t.Context(), logger), fake.NewClientBuilder().Build()); err != nil {
		t.Fatal(err)
	}
	if got := u.GetBuffer().String(); !strings.Contains(got, "already gone") || !strings.Contains(got, `Job="team-a/a1"`) {
		t.Errorf("log = %s", got)
	}
}
