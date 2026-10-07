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
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/identity"
	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/manager"
)

// IdentityIndex indexes TerraformClusters, TerraformMachines and
// TerraformMachinePools by the identity names they reference, for the
// identity fan-out.
const IdentityIndex = "captf.identity"

// ClusterIdentityIndexer returns o, a TerraformCluster's, own identityRef
// and its defaults.identityRef (machines and pools inherit the latter). A
// kind: Secret reference names no identity and is left out.
func ClusterIdentityIndexer(o client.Object) []string {
	tc, ok := o.(*infrav1.TerraformCluster)
	if !ok {
		return nil
	}
	var out []string
	if n := tc.Spec.IdentityRef.ClusterIdentityName(); n != "" {
		out = append(out, n)
	}
	if d := tc.Spec.Defaults; d != nil && d.IdentityRef.ClusterIdentityName() != "" && d.IdentityRef.ClusterIdentityName() != tc.Spec.IdentityRef.ClusterIdentityName() {
		out = append(out, d.IdentityRef.ClusterIdentityName())
	}
	return out
}

// MachineIdentityIndexer returns o, a TerraformMachine's, own identityRef.
func MachineIdentityIndexer(o client.Object) []string {
	tm, ok := o.(*infrav1.TerraformMachine)
	if !ok || tm.Spec.IdentityRef.ClusterIdentityName() == "" {
		return nil
	}
	return []string{tm.Spec.IdentityRef.Name}
}

// PoolIdentityIndexer returns o, a TerraformMachinePool's, own identityRef.
func PoolIdentityIndexer(o client.Object) []string {
	mp, ok := o.(*infrav1.TerraformMachinePool)
	if !ok || mp.Spec.IdentityRef.ClusterIdentityName() == "" {
		return nil
	}
	return []string{mp.Spec.IdentityRef.Name}
}

// VariablesSourceIndex indexes TerraformClusters, TerraformMachines and
// TerraformMachinePools by the variablesFrom sources they reference, as
// "ConfigMap/<name>" and "Secret/<name>" (VariablesSourceKeys), for the
// source fan-out.
const VariablesSourceIndex = "captf.variablesSource"

// ClusterVariablesSourceIndexer returns o, a TerraformCluster's,
// variablesFrom sources.
func ClusterVariablesSourceIndexer(o client.Object) []string {
	tc, ok := o.(*infrav1.TerraformCluster)
	if !ok {
		return nil
	}
	return VariablesSourceKeys(tc.Spec.VariablesFrom)
}

// MachineVariablesSourceIndexer returns o, a TerraformMachine's,
// variablesFrom sources.
func MachineVariablesSourceIndexer(o client.Object) []string {
	tm, ok := o.(*infrav1.TerraformMachine)
	if !ok {
		return nil
	}
	return VariablesSourceKeys(tm.Spec.VariablesFrom)
}

// PoolVariablesSourceIndexer returns o, a TerraformMachinePool's,
// variablesFrom sources.
func PoolVariablesSourceIndexer(o client.Object) []string {
	mp, ok := o.(*infrav1.TerraformMachinePool)
	if !ok {
		return nil
	}
	return VariablesSourceKeys(mp.Spec.VariablesFrom)
}

// MirrorIdentityIndex indexes the metadata of credential mirror Secrets
// (manager.SecretMeta) by the identity they mirror, for the identity
// controller's status.namespaces.
const MirrorIdentityIndex = "captf.mirrorIdentity"

// MirrorIdentityIndexer returns the identity o mirrors: the
// inputs.IdentityAnnotation value of a Secret labeled
// identity.MirroredLabel=true; nothing for any other object. The mirror's
// name is left for the reader to check.
func MirrorIdentityIndexer(o client.Object) []string {
	name := o.GetAnnotations()[inputs.IdentityAnnotation]
	if o.GetLabels()[identity.MirroredLabel] != "true" || name == "" {
		return nil
	}
	return []string{name}
}

// SetupIndexes registers the field indexes on mgr, using ctx; call it
// before the controllers start. Only machine templates are indexed (by
// variables source): the other templates run nothing. Indexing a type starts
// its informer, so a Secret index must be on manager.SecretMeta: a typed one
// would cache every payload. It returns any error registering an index.
func SetupIndexes(ctx context.Context, mgr ctrl.Manager) error {
	for _, ix := range []struct {
		obj   client.Object
		field string
		fn    client.IndexerFunc
	}{
		{&infrav1.TerraformCluster{}, IdentityIndex, ClusterIdentityIndexer},
		{&infrav1.TerraformMachine{}, IdentityIndex, MachineIdentityIndexer},
		{&infrav1.TerraformCluster{}, VariablesSourceIndex, ClusterVariablesSourceIndexer},
		{&infrav1.TerraformMachine{}, VariablesSourceIndex, MachineVariablesSourceIndexer},
		{&infrav1.TerraformMachinePool{}, IdentityIndex, PoolIdentityIndexer},
		{&infrav1.TerraformMachinePool{}, VariablesSourceIndex, PoolVariablesSourceIndexer},
		{&infrav1.TerraformMachineTemplate{}, VariablesSourceIndex, TemplateVariablesSourceIndexer},
		{&infrav1.TerraformPlan{}, PlanTargetIndex, PlanTargetIndexer},
		// Metadata only: the informer the managed-Secret watches share.
		{manager.SecretMeta(), MirrorIdentityIndex, MirrorIdentityIndexer},
	} {
		if err := mgr.GetFieldIndexer().IndexField(ctx, ix.obj, ix.field, ix.fn); err != nil {
			return fmt.Errorf("index %T by %s: %w", ix.obj, ix.field, err)
		}
	}
	return nil
}
