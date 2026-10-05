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
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// Annotations on the durable Secret recording the execution context of the
// last apply (digest pinning).
const (
	// ImageAnnotation is spec.source.image as written (tag or digest).
	ImageAnnotation = "captf.io/image"
	// ImageDigestAnnotation is the digest the runtime resolved the image to,
	// repo@sha256:…, from the apply Job's pod.
	ImageDigestAnnotation = "captf.io/image-digest"
	// IdentityAnnotation is the TerraformClusterIdentity used.
	IdentityAnnotation = "captf.io/identity"
	// AppliedAnnotation is "true" once an apply of the object succeeded or
	// a state backup was restored into its backend: a missing state is
	// then a lost one, not one that was never written. It moves with the
	// Secret (clusterctl move does not carry status), and nothing but the
	// Secret's deletion removes it.
	AppliedAnnotation = "captf.io/applied"
	// PendingClusterOutputsAnnotation is a TerraformMachinePool's pending
	// change of the cluster's exports (Pending, as JSON): its guarded apply
	// was blocked before a destructive plan, and the pool keeps applying
	// with the exports of its last successful apply until the change is
	// approved, or the exports change again or return to those. While the
	// exports are back at those it is ignored but kept, so a return to the
	// change holds it again; a successful apply of the cluster's own
	// exports removes it. It holds hashes, a Job name and the runner's
	// summary (addresses and actions), never an exported value. Nothing
	// else carries it.
	PendingClusterOutputsAnnotation = "captf.io/pending-cluster-outputs"
	// PartialClusterOutputsAnnotation is a TerraformMachinePool's change of
	// the cluster's exports that may be partly applied (Partial, as JSON):
	// a guarded apply of it failed in its apply step, or after its runner
	// started without a result to tell how far it got. Until an apply
	// succeeds, the pool's state may not match the exports of its last
	// successful apply, so it no longer holds those in place of a change,
	// every apply it starts is guarded, and an apply stays due. It holds a
	// hash and a Job name, never an exported value.
	PartialClusterOutputsAnnotation = "captf.io/partial-cluster-outputs"
	// AppliedClusterOutputsHashAnnotation is hash.Exports of the
	// captf_cluster_outputs a TerraformMachinePool's last successful apply
	// rendered (RecordClusterOutputs). It is set whether or not those
	// exports fit next to the rendered files (AppliedClusterOutputsKey),
	// and Write never touches it, so a pool whose record was dropped for
	// size still tells a change of its exports from none. A hash, never
	// an exported value.
	AppliedClusterOutputsHashAnnotation = "captf.io/applied-cluster-outputs-hash"
	// InterruptedApplyAnnotation names an apply Job of a TerraformCluster
	// or TerraformMachinePool that is gone before it finished (deleted
	// while it ran): no result tells how far it got, and the state's
	// inputs hash, which only a successful apply writes, does not show
	// what it may have changed. Until an apply started after it succeeds,
	// an apply stays due, even of the state's own inputs. It holds a Job
	// name, nothing else; SetInterruptedApply sets it and
	// ClearInterruptedApply removes it.
	InterruptedApplyAnnotation = "captf.io/interrupted-apply"
)

// Data keys: the two rendered files, and on a TerraformMachinePool's
// Secret the cluster exports of its last successful apply.
const (
	MainTFKey = render.MainTFFile
	TFVarsKey = render.TFVarsFile
	// AppliedClusterOutputsKey holds the captf_cluster_outputs value a
	// TerraformMachinePool's last successful apply rendered
	// (RecordClusterOutputs). It is data, not an annotation, because
	// exports may be sensitive. A pool renders it in place of the
	// cluster's current exports while a destructive change of those waits
	// for approval (PendingClusterOutputsAnnotation). It shares the
	// Secret's 1,000,000-byte data budget with the rendered files, and is
	// left out, or dropped by Write, when it does not fit next to them;
	// AppliedClusterOutputsHashAnnotation stays.
	AppliedClusterOutputsKey = "applied-cluster-outputs.json"
)

// Name prefixes.
const (
	durablePrefix = "captf-inputs-"
	runPrefix     = "captf-run-"
)
