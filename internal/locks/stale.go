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

package locks

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// LockInfoAnnotation holds the backend's JSON lock info on the Lease.
const LockInfoAnnotation = "app.terraform.io/lock-info"

// Info is the part of the lock info the controller uses.
type Info struct {
	ID        string    `json:"ID"`
	Operation string    `json:"Operation"`
	Who       string    `json:"Who"`
	Version   string    `json:"Version"`
	Created   time.Time `json:"Created"`
}

// Status describes the state lock of one object.
type Status struct {
	// Held is true while the Lease has a holder.
	Held bool
	// LockID is the lock to force-unlock.
	LockID string
	// Holder is the pod name from the lock info's Who. It is empty when
	// unknown, and when the holder is not one of the object's own runner
	// pods: a lock held from a workstation or another tool is never stale.
	Holder string
	// HolderPodExists reports whether that pod still exists and has not
	// reached a terminal phase.
	HolderPodExists bool
}

// Stale reports whether the lock is held by a pod that no longer exists.
func (s Status) Stale() bool {
	return s.Held && s.Holder != "" && !s.HolderPodExists
}

// Check reads the lock of suffix in namespace using ctx and c, and returns
// its Status. ours reports whether a Who hostname is a pod of the object's
// own Jobs (jobs.OwnsPod); any other holder is left unknown. Pass c as an
// uncached reader (manager.GetAPIReader): a cached Get on a Lease or Pod
// would start a cluster-wide informer. It returns a non-nil error only when
// reading the Lease or the holder Pod fails for a reason other than not
// found.
func Check(ctx context.Context, c client.Reader, namespace, suffix string, ours func(pod string) bool) (Status, error) {
	lease := &coordinationv1.Lease{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: state.LeaseName(suffix)}, lease); err != nil {
		if apierrors.IsNotFound(err) {
			return Status{}, nil
		}
		return Status{}, fmt.Errorf("locks: get Lease: %w", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		return Status{}, nil
	}
	st := Status{Held: true, LockID: *lease.Spec.HolderIdentity}
	var info Info
	if raw := lease.Annotations[LockInfoAnnotation]; raw != "" && json.Unmarshal([]byte(raw), &info) == nil {
		if info.ID != "" {
			st.LockID = info.ID
		}
		if i := strings.LastIndex(info.Who, "@"); i >= 0 && ours(info.Who[i+1:]) {
			st.Holder = info.Who[i+1:]
		}
	}
	if st.Holder == "" {
		// Holder unknown or not ours: never considered stale.
		return st, nil
	}
	pod := &corev1.Pod{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: st.Holder}, pod)
	switch {
	case err == nil:
		// A finished Job keeps its pod object (phase Failed or Succeeded)
		// until the Job is pruned. Its containers have exited, so it holds
		// nothing: an OOM-killed or evicted runner leaves exactly this. A
		// pod that is only terminating may still be running the runtime
		// inside its grace period, so it stays alive until it has a
		// terminal phase.
		st.HolderPodExists = pod.Status.Phase != corev1.PodSucceeded && pod.Status.Phase != corev1.PodFailed
	case apierrors.IsNotFound(err):
	default:
		return Status{}, fmt.Errorf("locks: get holder pod %s: %w", st.Holder, err)
	}
	return st, nil
}
