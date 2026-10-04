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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/cluster-api/util"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrav1 "github.com/captf-io/cluster-api-provider-terraform/api/v1alpha1"
	"github.com/captf-io/cluster-api-provider-terraform/internal/outputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/state"
)

// FieldOwner is the field manager of CAPTF's writes to CAPI objects (a
// Machine's remediation annotation, a MachinePool's replicas).
const FieldOwner = "captf-manager"

// Bootstrap Secret keys: value is the contract's single mandatory key;
// format is CABPK's.
const (
	BootstrapValueKey  = "value"
	BootstrapFormatKey = "format"
	// DefaultBootstrapFormat is cloud-init, CABPK's default when format is
	// unset.
	DefaultBootstrapFormat = "cloud-config"
)

// BootstrapReady reports whether bootstrap Secret s carries data.
func BootstrapReady(s *corev1.Secret) bool {
	return s != nil && len(s.Data[BootstrapValueKey]) > 0
}

// BootstrapData returns the base64 (standard, padded) of s's raw value
// bytes: a gzip payload (CAPRKE2 gzipUserData) is not a string, so every
// module receives base64 for every bootstrap provider.
func BootstrapData(s *corev1.Secret) string {
	return base64.StdEncoding.EncodeToString(s.Data[BootstrapValueKey])
}

// BootstrapFormat returns s's format key, else DefaultBootstrapFormat.
func BootstrapFormat(s *corev1.Secret) string {
	if f := string(s.Data[BootstrapFormatKey]); f != "" {
		return f
	}
	return DefaultBootstrapFormat
}

// ReadBootstrapSecret reads the bootstrap data Secret name in namespace
// through the reader r, using ctx: it is not captf.io/managed, so it is not
// cached. found is false while name is unset ("" counts as unset) or the
// Secret is missing. It returns the Secret (nil unless found), found, and
// an error from a read failure other than not-found.
func ReadBootstrapSecret(ctx context.Context, r client.Reader, namespace string, name *string) (s *corev1.Secret, found bool, err error) {
	if name == nil || *name == "" {
		return nil, false, nil
	}
	s = &corev1.Secret{}
	err = r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: *name}, s)
	switch {
	case apierrors.IsNotFound(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("get bootstrap Secret: %w", err)
	}
	return s, true, nil
}

// LookupCluster completes owner for obj, a machine or pool, using ctx and
// the client c: the Cluster through obj's cluster-name label, then the
// Cluster's TerraformCluster. A missing label, Cluster, infrastructureRef
// or TerraformCluster leaves the field nil (the gates wait for it); a
// Cluster whose infrastructureRef is not a TerraformCluster sets the
// ClusterNotTerraform gate. It returns an error from a lookup failure
// other than not-found.
func LookupCluster(ctx context.Context, c client.Client, obj metav1.ObjectMeta, owner *OwnerInfo) error {
	cluster, err := util.GetClusterFromMetadata(ctx, c, obj)
	switch {
	case errors.Is(err, util.ErrNoCluster), apierrors.IsNotFound(err):
		return nil
	case err != nil:
		return fmt.Errorf("get Cluster: %w", err)
	}
	owner.Cluster = cluster

	ref := cluster.Spec.InfrastructureRef
	if ref.Name == "" {
		return nil
	}
	if ref.APIGroup != infrav1.GroupVersion.Group || ref.Kind != state.KindTerraformCluster {
		owner.Gate = &Gate{
			Status: metav1.ConditionFalse, Reason: infrav1.ClusterNotTerraformReason,
			Message: fmt.Sprintf("Cluster %s uses infrastructure %s %s, not a TerraformCluster", cluster.Name, ref.APIGroup, ref.Kind),
		}
		return nil
	}
	tc := &infrav1.TerraformCluster{}
	err = c.Get(ctx, client.ObjectKey{Namespace: obj.Namespace, Name: ref.Name}, tc)
	switch {
	case apierrors.IsNotFound(err):
	case err != nil:
		return fmt.Errorf("get TerraformCluster: %w", err)
	default:
		owner.InfraCluster = tc
	}
	return nil
}

// ReadClusterState reads tc's state through d.State, using ctx. A state
// not written yet, unreadable or inconsistent (the cluster Job may be
// writing chunks) is a wait, not an error. It returns the state and true,
// false while waiting, or an error from any other read failure.
func ReadClusterState(ctx context.Context, d Deps, tc *infrav1.TerraformCluster) (*state.State, bool, error) {
	suffix, err := state.Suffix(tc.Namespace, state.KindTerraformCluster, tc.Name)
	if err != nil {
		return nil, false, err
	}
	st, err := d.State.Read(ctx, tc.Namespace, suffix)
	switch {
	case isStateWait(err):
		return nil, false, nil
	case err != nil:
		return nil, false, fmt.Errorf("read the cluster state: %w", err)
	}
	return st, true, nil
}

// ReadClusterOutputs reads, using ctx, d.State and one read of tc's state, the
// cluster outputs a machine or pool renders: tc's exports output and the
// names of its failure domains. An externally managed TerraformCluster has
// no state: exports {} and the names of its status.failureDomains. concerns
// are the output names whose decode problem (an invalid or pending value)
// counts as not readable. It returns the exports, the failure-domain names
// (unsorted) and true once readable, false while waiting (a nil tc, a state
// not written or mid-write, or a concerning output), or an error from any
// other read failure.
func ReadClusterOutputs(ctx context.Context, d Deps, tc *infrav1.TerraformCluster, concerns ...string) (exports json.RawMessage, failureDomains []string, ok bool, err error) {
	if tc == nil {
		return nil, nil, false, nil
	}
	if annotations.IsExternallyManaged(tc) {
		names := make([]string, 0, len(tc.Status.FailureDomains))
		for _, fd := range tc.Status.FailureDomains {
			names = append(names, fd.Name)
		}
		return json.RawMessage("{}"), names, true, nil
	}
	st, ok, err := ReadClusterState(ctx, d, tc)
	if err != nil || !ok {
		return nil, nil, false, err
	}
	out, res := outputs.DecodeCluster(st)
	for _, name := range concerns {
		if res.Concerns(name) {
			return nil, nil, false, nil
		}
	}
	names := make([]string, 0, len(out.FailureDomains))
	for _, fd := range out.FailureDomains {
		names = append(names, fd.Name)
	}
	return out.Exports, names, true, nil
}

// isStateWait reports whether err is one of the state read errors that
// mean "try again later" rather than a real failure.
func isStateWait(err error) bool {
	for _, target := range []error{state.ErrNoState, state.ErrStateInconsistent, state.ErrStateEncrypted, state.ErrStateCorrupt, state.ErrUnsupportedStateVersion} {
		if errors.Is(err, target) {
			return true
		}
	}
	return false
}
