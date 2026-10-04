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
	"context"
	"encoding/json"
	"maps"
	"slices"

	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/klog/v2"

	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// Log levels: 0 is errors and irreversible actions (Job created, destroy
// started, finalizer removed, force-unlock, Machine annotated), LogFlow
// the default flow, LogDebug debugging detail, LogTrace the rendered
// root. Nothing logs bootstrap data, credentials or tfvars values: tfvars
// only through render.RedactedTFVars.
const (
	LogFlow  = 2
	LogDebug = 4
	LogTrace = 5
)

// WithObjectLogger adds owner's Cluster, Machine and MachinePool to ctx's
// logger under their kind keys, so every line of a reconcile carries them
// (CAPI logging conventions). The object's own key (TerraformCluster,
// TerraformMachine, TerraformMachinePool, TerraformMachineTemplate) is added
// by its Reconciler.
// It returns ctx with the enriched logger.
func WithObjectLogger(ctx context.Context, owner OwnerInfo) context.Context {
	logger := klog.FromContext(ctx)
	if owner.Cluster != nil {
		logger = klog.LoggerWithValues(logger, "Cluster", klog.KObj(owner.Cluster))
	}
	if owner.Machine != nil {
		logger = klog.LoggerWithValues(logger, "Machine", klog.KObj(owner.Machine))
	}
	if owner.MachinePool != nil {
		logger = klog.LoggerWithValues(logger, "MachinePool", klog.KObj(owner.MachinePool))
	}
	return klog.NewContext(ctx, logger)
}

// withJob adds the running job to ctx's logger. It returns ctx with the
// enriched logger.
func withJob(ctx context.Context, job *batchv1.Job) context.Context {
	return klog.NewContext(ctx, klog.LoggerWithValues(klog.FromContext(ctx), "Job", klog.KObj(job)))
}

// logRendered logs, through ctx's logger, what a Job will run: inputsHash
// and the tfvars variable names of files at LogDebug, the rendered root
// at LogTrace with the tfvars redacted.
func logRendered(ctx context.Context, inputsHash string, files render.Files) {
	logger := klog.FromContext(ctx)
	if logger.V(LogDebug).Enabled() {
		var vars map[string]json.RawMessage
		_ = json.Unmarshal(files.TFVars, &vars) // names only; an unparsable file logs none
		logger.V(LogDebug).Info("Rendered inputs", "inputsHash", inputsHash, "variables", slices.Sorted(maps.Keys(vars)))
	}
	if logger.V(LogTrace).Enabled() {
		logger.V(LogTrace).Info("Rendered root", "mainTF", string(files.MainTF), "tfvars", string(render.RedactedTFVars(files)))
	}
}
