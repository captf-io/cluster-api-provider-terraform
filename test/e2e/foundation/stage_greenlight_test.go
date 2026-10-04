//go:build e2e

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

package foundation

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/greenlight"
)

// greenLight is stage 6: it writes greenlight.json with every stage's
// duration (this one's up to the write), then runs greenlight.Require
// against it exactly as a later test would, and prints the record's
// summary.
// It fails t on any problem.
func (s *suite) greenLight(_ context.Context, t *testing.T) {
	start := time.Now()
	path := greenlight.Path(s.cfg.WorkDir())
	stages := slices.Clone(s.stages)
	rec := greenlight.Record{
		Cluster:           s.cfg.Name,
		TreeID:            s.treeID,
		ManagerRef:        s.managerRef,
		KubernetesVersion: s.serverVersion,
		Pins:              env.CurrentPins(),
		StabilityWindow:   s.opts.stability,
	}
	stages = append(stages, greenlight.Stage{Name: "green-light", Duration: time.Since(start), Passed: true})
	rec.Stages = stages
	rec.CreatedAt = time.Now().UTC()
	if err := greenlight.Write(path, rec); err != nil {
		t.Fatalf("write the green light: %v", err)
	}
	check, err := greenlight.CheckFromState(s.cfg.WorkDir(), s.kind.Exists)
	if err != nil {
		t.Fatalf("build the green-light check from state.json: %v", err)
	}
	greenlight.Require(t, path, check)
	s.stages = stages
	t.Logf("green light written to %s: cluster %s, Kubernetes %s, manager %s (tree %s), stability window %s",
		path, rec.Cluster, rec.KubernetesVersion, rec.ManagerRef, rec.TreeID, rec.StabilityWindow)
}
