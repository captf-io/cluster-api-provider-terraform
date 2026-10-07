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

package jobs

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// Op is an operation a Job runs.
type Op string

// The operations a Job can run.
const (
	OpApply   Op = "apply"
	OpDestroy Op = "destroy"
	OpRefresh Op = "refresh"
	OpDrift   Op = "drift"
	// OpRestore pushes a state backup into the backend
	// (infrav1.RestoreStateAnnotation).
	OpRestore Op = "restore"
	// OpPlan plans the current inputs for review and applies nothing (a
	// TerraformCluster's applyPolicy Manual).
	OpPlan Op = "plan"
)

// RestoreSerialAnnotation records, on a restore Job, the serial of the
// backup it pushes.
const RestoreSerialAnnotation = "captf.io/restore-serial"

// StateLockVersionAnnotation records, on an apply Job, the
// resourceVersion of its object's state lock Lease when the Job was
// created, or StateLockAbsent when there was no Lease. Terraform and
// OpenTofu take that lock before they change anything, and every lock and
// unlock writes the Lease: an apply Job that ended with no pod and no
// result left the Lease as recorded only if it never reached its runtime,
// so it created nothing.
const StateLockVersionAnnotation = "captf.io/state-lock-version"

// StateLockAbsent is StateLockVersionAnnotation's value when the state
// lock Lease did not exist.
const StateLockAbsent = "none"

// RestoreChunkDir is where a restore Job's config volume holds the backup's
// chunks, named 0, 1, …: a subdirectory, so the runner's copy of the
// rendered root (top-level files only) leaves them out.
const RestoreChunkDir = "restore"

// Labels on every Job and its pods, besides the state package's owner
// labels and cluster-name.
const (
	OpLabel      = "captf.infrastructure.cluster.x-k8s.io/op"
	AttemptLabel = "captf.infrastructure.cluster.x-k8s.io/attempt"
)

// MaxNameLength caps Job names at 57 characters, short of Kubernetes' usual
// 63-character DNS label limit. The Job controller names pods
// <job>-<5 random characters>, and the kubelet truncates a pod hostname
// over 63 characters. At 57 the pod name, and
// therefore the hostname Terraform and OpenTofu write into the lock's Who
// field, is the full pod name, so stale-lock detection can look the pod up
// by name (kubelet truncatePodHostnameIfNeeded, kubernetes v1.36.2).
const MaxNameLength = 57

// Hash6 returns the first six hex characters of
// sha256(inputsHash ‖ op ‖ attempt ‖ driftTick). It makes repeated drift and
// refresh ticks distinct and ties a Job name to its inputs.
func Hash6(inputsHash string, op Op, attempt int32, driftTick string) string {
	sum := sha256.Sum256([]byte(inputsHash + "\x00" + string(op) + "\x00" + strconv.Itoa(int(attempt)) + "\x00" + driftTick))
	return hex.EncodeToString(sum[:])[:6]
}

// Name returns captf-<kindshort>-<name>-<op>-a<attempt>-<hash6>. When that
// exceeds MaxNameLength, name is replaced by sha256(name)[:16].
func Name(kindshort, name string, op Op, attempt int32, hash6 string) string {
	n := fmt.Sprintf("captf-%s-%s-%s-a%d-%s", kindshort, name, op, attempt, hash6)
	if len(n) <= MaxNameLength {
		return n
	}
	sum := sha256.Sum256([]byte(name))
	return fmt.Sprintf("captf-%s-%s-%s-a%d-%s", kindshort, hex.EncodeToString(sum[:])[:16], op, attempt, hash6)
}

// OwnsPod reports whether pod is named like a pod of a Job that Name returns
// for kindshort and name: <job>-<5 characters>. Stale-lock detection uses
// it to tell this object's runner pods from any other lock holder, such as
// an operator's workstation.
func OwnsPod(kindshort, name, pod string) bool {
	job, podSuffix, ok := cutLast(pod)
	if !ok || len(podSuffix) != 5 {
		return false
	}
	rest, hash6, ok := cutLast(job)
	if !ok {
		return false
	}
	rest, a, ok := cutLast(rest)
	if !ok || len(a) < 2 || a[0] != 'a' {
		return false
	}
	attempt, err := strconv.ParseInt(a[1:], 10, 32)
	if err != nil {
		return false
	}
	_, op, ok := cutLast(rest)
	if !ok {
		return false
	}
	switch Op(op) {
	case OpApply, OpDestroy, OpRefresh, OpDrift, OpRestore, OpPlan:
	default:
		return false
	}
	return Name(kindshort, name, Op(op), int32(attempt), hash6) == job
}

// cutLast splits s around its last "-" and returns the parts before and
// after it, and whether "-" was found at all.
func cutLast(s string) (before, after string, ok bool) {
	i := strings.LastIndexByte(s, '-')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// Labels returns the labels of a Job and its pods: ownerKind and ownerName
// identify the owning Terraform* object, clusterName its Cluster, and op and
// attempt the operation and its attempt number.
func Labels(ownerKind, ownerName, clusterName string, op Op, attempt int32) map[string]string {
	return map[string]string{
		state.OwnerKindLabel:       ownerKind,
		state.OwnerNameLabel:       state.LabelValue(ownerName),
		clusterv1.ClusterNameLabel: state.LabelValue(clusterName),
		state.ManagedLabel:         "true",
		OpLabel:                    string(op),
		AttemptLabel:               strconv.Itoa(int(attempt)),
	}
}
