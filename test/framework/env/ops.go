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

package env

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
)

// Down deletes the configured cluster, or with All every captf-test-*
// cluster, under ctx. For each it removes the work directory except
// artifacts/. Once no captf-test-* cluster is left it also removes the
// dedicated framework.KindNetwork network, which kind creates but never
// deletes. A name failing kindcluster.ValidateName is never touched. It
// returns every error joined.
func (e *Env) Down(ctx context.Context) error {
	eng, cl, err := e.preflight(ctx, false)
	if err != nil {
		return err
	}
	names := []string{e.cfg.Name}
	if e.cfg.All {
		if names, err = cl.List(); err != nil {
			return err
		}
		if len(names) == 0 {
			e.logf("down: no captf-test-* cluster exists")
		}
	}
	var errs []error
	for _, name := range names {
		if err := kindcluster.ValidateName(name); err != nil {
			errs = append(errs, err)
			continue
		}
		start := e.now()
		kubeconfig := filepath.Join(e.cfg.workDirFor(name), "kubeconfig")
		if err := cl.Delete(name, kubeconfig); err != nil {
			errs = append(errs, err)
			continue
		}
		if err := removeWorkDir(e.cfg.TestenvRoot(), name); err != nil {
			errs = append(errs, err)
			continue
		}
		e.logf("down: deleted %s in %s (artifacts, if any, kept in %s)", name, e.now().Sub(start).Round(time.Millisecond), filepath.Join(e.cfg.workDirFor(name), artifactsDir))
	}
	left, err := cl.List()
	switch {
	case err != nil:
		errs = append(errs, err)
	case len(left) == 0:
		if err := e.deps.Host.RemoveNetwork(ctx, eng, framework.KindNetwork); err != nil {
			errs = append(errs, err)
		}
	default:
		e.logf("down: keeping network %s, still used by %s", framework.KindNetwork, strings.Join(left, ", "))
	}
	return errors.Join(errs...)
}

// StatusReport is what Status found.
type StatusReport struct {
	// Name is the cluster name.
	Name string
	// Engine is the detected container engine.
	Engine string
	// Exists reports whether the cluster exists.
	Exists bool
	// Nodes are its node names.
	Nodes []string
	// Pods is wait.Report on the provider namespaces: the non-ready pods.
	Pods string
	// State is the parsed state.json, or nil.
	State *State
	// Problems are the checks that failed (unreadable state, unreachable
	// API server, ...); Status reports them instead of failing.
	Problems []string
}

// Status reports, under ctx, whether the cluster exists, its nodes, the
// non-ready provider pods and state.json, and writes the report to the
// Env's output. A missing cluster or a failing check is reported, not
// returned. It returns the report, or an error when the engine or kind
// cannot be reached at all.
func (e *Env) Status(ctx context.Context) (*StatusReport, error) {
	eng, cl, err := e.preflight(ctx, false)
	if err != nil {
		return nil, err
	}
	rep := &StatusReport{Name: e.cfg.Name, Engine: string(eng.Name)}
	if rep.Exists, err = cl.Exists(e.cfg.Name); err != nil {
		return nil, err
	}
	if st, err := ReadState(e.cfg.WorkDir()); err != nil {
		rep.Problems = append(rep.Problems, err.Error())
	} else {
		rep.State = st
	}
	if rep.Exists {
		if rep.Nodes, err = cl.Nodes(e.cfg.Name); err != nil {
			rep.Problems = append(rep.Problems, err.Error())
		}
		if pods, err := e.report(ctx); err != nil {
			rep.Problems = append(rep.Problems, err.Error())
		} else {
			rep.Pods = pods
		}
	}
	rep.write(e.out)
	return rep, nil
}

// report returns the Kube report of the cluster behind the work
// directory's kubeconfig, read under ctx, or an error.
func (e *Env) report(ctx context.Context) (string, error) {
	kube, err := e.deps.Kube(e.cfg.Kubeconfig())
	if err != nil {
		return "", err
	}
	return kube.Report(ctx)
}

// write prints r to w as an indented, human-readable block.
func (r *StatusReport) write(w io.Writer) {
	fmt.Fprintf(w, "testenv: status of %s (%s)\n", r.Name, r.Engine)
	if !r.Exists {
		fmt.Fprintf(w, "  cluster: does not exist (make testenv-up creates it)\n")
	} else {
		fmt.Fprintf(w, "  cluster: exists, nodes: %s\n", strings.Join(r.Nodes, ", "))
		fmt.Fprintf(w, "  provider pods: %s\n", strings.ReplaceAll(r.Pods, "\n", "\n    "))
	}
	if s := r.State; s != nil {
		fmt.Fprintf(w, "  state: %s at %s\n", s.Operation, s.UpdatedAt.Format(time.RFC3339))
		fmt.Fprintf(w, "  kubeconfig: %s\n", s.Kubeconfig)
		fmt.Fprintf(w, "  manager: %s\n", s.ManagerRef)
		fmt.Fprintf(w, "  pins: kind %s, node %s, cluster-api %s, cert-manager %s, captf %s\n",
			s.Pins.KindVersion, s.Pins.KindNodeImage, s.Pins.CAPIVersion, s.Pins.CertManagerVersion, s.Pins.CAPTFVersion)
		if !s.Pins.Equal(CurrentPins()) {
			fmt.Fprintf(w, "  pins: differ from this framework build; run make testenv-down, then make testenv-up\n")
		}
		fmt.Fprintf(w, "  noop repo digests on the nodes: %s\n", DigestFinding(s.NodeImages, s.NoopImages))
		var total float64
		for _, t := range s.Timings {
			total += t.Seconds
		}
		fmt.Fprintf(w, "  last %s took %.0fs over %d steps\n", s.Operation, total, len(s.Timings))
	}
	for _, p := range r.Problems {
		fmt.Fprintf(w, "  problem: %s\n", p)
	}
}

// Collect writes a diagnostics bundle of the running cluster under ctx
// into <work>/artifacts/<UTC timestamp>/ and returns its path. It returns
// an error when the cluster does not exist or nothing could be written.
func (e *Env) Collect(ctx context.Context) (string, error) {
	_, cl, err := e.preflight(ctx, false)
	if err != nil {
		return "", err
	}
	if err := e.requireCluster(cl); err != nil {
		return "", err
	}
	dir, err := e.collect(ctx, cl)
	if err != nil {
		return "", err
	}
	e.logf("collect: diagnostics written to %s", dir)
	return dir, nil
}

// requireCluster returns an error unless the configured cluster exists,
// as cl reports, and its kubeconfig is refreshed from kind into the work
// directory.
func (e *Env) requireCluster(cl Cluster) error {
	exists, err := cl.Exists(e.cfg.Name)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("env: cluster %s does not exist: run make testenv-up", e.cfg.Name)
	}
	kc, err := cl.Kubeconfig(e.cfg.Name)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(e.cfg.Kubeconfig(), []byte(kc), 0o600); err != nil {
		return fmt.Errorf("env: write kubeconfig: %w", err)
	}
	return nil
}

// reloadStepCount is the number of steps Reload logs.
const reloadStepCount = 5

// Reload is the inner loop, under ctx: it rebuilds and smoke-checks the
// manager image for the current tree, side-loads it into every node,
// points the captf-controller-manager Deployment (image and
// CAPTF_MANAGER_IMAGE) at it and waits for the rollout, then updates
// state.json and env.sh. When the Deployment already runs that reference
// (an unchanged tree) it reports "unchanged" and restarts nothing. It
// returns the first error, after collecting diagnostics.
func (e *Env) Reload(ctx context.Context) (err error) {
	started := e.now()
	r := e.newRun("reload", reloadStepCount)
	var st *State
	var eng *engine.Engine
	var cl Cluster
	var kube Kube
	if err := r.step("preflight", func() error {
		var err error
		if eng, cl, err = e.preflight(ctx, false); err != nil {
			return err
		}
		if err := e.requireCluster(cl); err != nil {
			return err
		}
		if st, err = ReadState(e.cfg.WorkDir()); err != nil {
			return fmt.Errorf("env: reload needs the state of make testenv-up: %w", err)
		}
		kube, err = e.deps.Kube(e.cfg.Kubeconfig())
		return err
	}); err != nil {
		return err
	}
	defer func() {
		if err != nil {
			e.collectAfterFailure(ctx, cl)
		}
	}()
	var ref, treeID string
	if err := r.step("build and smoke-check the manager image", func() error {
		var err error
		ref, err = e.buildManager(ctx, eng, &treeID)
		return err
	}); err != nil {
		return err
	}

	if err := r.step("side-load the manager image", func() error {
		if err := cl.SideLoad(ctx, e.cfg.Name, []string{ref}, filepath.Join(e.cfg.WorkDir(), imagesDir)); err != nil {
			return err
		}
		recs, err := cl.NodeImages(ctx, e.cfg.Name, []string{ref})
		if err != nil {
			return err
		}
		st.NodeImages = slices.DeleteFunc(st.NodeImages, func(n NodeImage) bool { return n.Ref == st.ManagerRef || n.Ref == ref })
		st.NodeImages = append(recs, st.NodeImages...)
		return nil
	}); err != nil {
		return err
	}

	if err := r.step("roll out the manager", func() error {
		return e.pointManagerAt(ctx, kube, ref)
	}); err != nil {
		return err
	}

	if err := r.step("update state.json and env.sh", func() error {
		st.TreeID, st.ManagerRef = treeID, ref
		st.Operation = "reload"
		st.Timings = r.timings
		st.UpdatedAt = e.now().UTC()
		if err := WriteState(e.cfg.WorkDir(), st); err != nil {
			return err
		}
		return writeEnvScript(e.cfg.WorkDir(), st)
	}); err != nil {
		return err
	}
	e.logf("reload: done in %s; manager %s", e.now().Sub(started).Round(time.Second), ref)
	return nil
}
