//go:build e2e

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

package foundation

import (
	"context"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
)

// installTimeout bounds stage 3: clusterctl init and the readiness waits.
const installTimeout = 10 * time.Minute

// captfInstall is stage 3: env.InstallProviders installs cert-manager,
// the CAPI core and kubeadm providers and CAPTF (on a reused cluster it
// points the manager at the freshly built image instead), waits for
// readiness and writes state.json, whose manager reference the later
// stages and the green light use.
// It runs under ctx and fails t on any problem.
func (s *suite) captfInstall(ctx context.Context, t *testing.T) {
	ctx, cancel := context.WithTimeout(ctx, installTimeout)
	defer cancel()
	if err := s.env.InstallProviders(ctx, s.res); err != nil {
		s.envCollected = true
		t.Fatalf("env.InstallProviders failed: %v", err)
	}
	st, err := env.ReadState(s.cfg.WorkDir())
	if err != nil {
		t.Fatalf("expected state.json after InstallProviders: %v", err)
	}
	if st.ManagerRef != s.managerRef {
		t.Fatalf("expected state.json to record manager %s, observed %s", s.managerRef, st.ManagerRef)
	}
	t.Logf("providers installed (%s); state.json records manager %s", st.Operation, st.ManagerRef)
}
