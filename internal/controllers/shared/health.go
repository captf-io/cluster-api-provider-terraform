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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/cluster-api/util/conditions"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
)

// definiteHealth are the InfrastructureHealthy reasons of an observed
// reading that is neither pending nor unknown.
var definiteHealth = sets.New(
	infrav1.HealthyReason,
	infrav1.InstanceUnhealthyReason,
	infrav1.InstanceDegradedReason,
	infrav1.InstanceStoppedReason,
	infrav1.InstanceTerminatedReason,
)

// sample does readState's health bookkeeping once InfrastructureHealthy
// and provisioned are set from the state: it lets bk, the pass's
// bookkeeping, and the apply's own reading stand in for the post-apply
// refresh (applySample), then counts the pass's sample (countSample) and
// the pending streak (countPending). prevRefresh is status.lastRefresh
// before bookkeeping; valid is whether the outputs decoded.
func (r *reconciler) sample(bk *Bookkeeping, prevRefresh *metav1.Time, valid bool) {
	// A refresh or drift Job completed this pass: one health sample.
	sampled := r.st.LastRefresh != nil && (prevRefresh == nil || prevRefresh.Before(r.st.LastRefresh))
	applied := false
	if t := lastApplySucceeded(bk); t != nil {
		applied = prevRefresh == nil || prevRefresh.Time.Before(*t)
	}
	if r.applySample(bk, valid) {
		sampled = true
	}
	if r.provisioned() {
		countSample(r.obj, r.st, sampled)
	}
	countPending(r.obj, r.st, sampled, applied)
}

// applySample makes a successful apply's own reading the post-apply sample
// of a kind that refreshes after each apply (RefreshAfterApply): when the
// apply finished after status.lastRefresh, the object is provisioned, its
// outputs are valid and its health reading is definite (neither pending nor
// unknown), status.lastRefresh advances to the apply's finish. DecideOp then
// starts no refresh for this apply, and the reading counts as one sample,
// once: the next pass finds lastRefresh already there. A pending or unknown
// reading, or unreadable outputs, keep the refresh. bk is the pass's
// bookkeeping, used to find the last successful apply. It reports whether
// the apply's reading was taken.
func (r *reconciler) applySample(bk *Bookkeeping, valid bool) bool {
	applied := lastApplySucceeded(bk)
	if !r.k.RefreshAfterApply() || applied == nil || !valid || !r.provisioned() {
		return false
	}
	if last := r.st.LastRefresh; last != nil && !last.Time.Before(*applied) {
		return false
	}
	c := conditions.Get(r.obj, infrav1.InfrastructureHealthyCondition)
	if c == nil || !definiteHealth.Has(c.Reason) {
		return false
	}
	advance(&r.st.LastRefresh, *applied)
	return true
}

// countSample updates status.unhealthySamples, for kinds that have it,
// once per health sample after provisioning (sampled): a completed
// refresh or drift Job (status.lastRefresh advanced this reconcile), or an
// apply whose own reading stood in for the post-apply refresh. Not once per
// state serial: a refresh that changes nothing keeps the serial, and a
// steadily unhealthy instance must still reach the threshold. An unhealthy,
// degraded or stopped reading adds one, a healthy one resets it, and
// anything else (pending, unknown, terminated) leaves it. The count is what
// remediation.unhealthyThreshold compares against; it lives in
// status only, so it restarts at 0 after clusterctl move. obj is read for
// its InfrastructureHealthy condition and st holds the UnhealthySamples
// field to update.
func countSample(obj Object, st CommonStatus, sampled bool) {
	if st.UnhealthySamples == nil || !sampled {
		return
	}
	c := conditions.Get(obj, infrav1.InfrastructureHealthyCondition)
	if c == nil {
		return
	}
	switch c.Reason {
	case infrav1.HealthyReason:
		*st.UnhealthySamples = 0
	case infrav1.InstanceUnhealthyReason, infrav1.InstanceDegradedReason, infrav1.InstanceStoppedReason:
		*st.UnhealthySamples++
	}
}

// countPending updates status.pendingRefreshes: the consecutive samples that
// read pending, which PendingRefreshDelay turns into the pending Refresh's
// backoff. A sample reading pending adds one (and starts at 1 when a
// successful apply finished since the previous sample, applied); any other
// sample resets it, and so does a new successful apply before its first
// sample. A pass without either leaves it, so repeated reconciles change
// nothing. obj is read for its InfrastructureHealthy condition and st holds
// the PendingRefreshes field to update; sampled is whether a health sample
// was taken this pass.
func countPending(obj Object, st CommonStatus, sampled, applied bool) {
	if !sampled {
		if applied {
			st.PendingRefreshes = 0
		}
		return
	}
	c := conditions.Get(obj, infrav1.InfrastructureHealthyCondition)
	switch {
	case c == nil || c.Reason != infrav1.InstancePendingReason:
		st.PendingRefreshes = 0
	case applied:
		st.PendingRefreshes = 1
	default:
		st.PendingRefreshes++
	}
}
