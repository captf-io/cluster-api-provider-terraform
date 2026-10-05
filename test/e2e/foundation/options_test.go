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
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
)

// The environment variables the suite reads, besides
// framework.EngineEnv, CLUSTERCTL, KUSTOMIZE and
// greenlight.MaxAgeEnv.
const (
	// clusterEnv names the cluster; default defaultCluster.
	clusterEnv = "CAPTF_E2E_CLUSTER"
	// reuseEnv, when true, lets the suite reuse an existing cluster.
	reuseEnv = "CAPTF_E2E_REUSE"
	// teardownEnv, when true, deletes the cluster after a passing run.
	teardownEnv = "CAPTF_E2E_TEARDOWN"
	// stabilityEnv is the stage 5 stability window, a Go duration.
	stabilityEnv = "CAPTF_E2E_STABILITY"
	// workersEnv is the number of kind worker nodes; default 0.
	workersEnv = "CAPTF_E2E_WORKERS"
)

// defaultCluster is the e2e cluster's name. It is not the development
// cluster (framework.DefaultClusterName), so the suite never collides
// with `make testenv-up`.
const defaultCluster = framework.ClusterNamePrefix + "e2e"

// defaultStability is the stability window when stabilityEnv is unset.
const defaultStability = 2 * time.Minute

// options is the suite's configuration, read once from the environment.
type options struct {
	// cluster is the kind cluster name; it passes kindcluster.ValidateName.
	cluster string
	// reuse lets the suite run against an existing cluster.
	reuse bool
	// teardown deletes the cluster after a passing run.
	teardown bool
	// stability is the stage 5 stability window.
	stability time.Duration
	// workers is the number of kind worker nodes.
	workers int
}

// optionsFrom reads the suite's options through getenv. It returns an
// error naming the variable at fault for an invalid cluster name,
// boolean, duration or worker count.
func optionsFrom(getenv func(string) string) (options, error) {
	o := options{cluster: strings.TrimSpace(getenv(clusterEnv)), stability: defaultStability}
	if o.cluster == "" {
		o.cluster = defaultCluster
	}
	if err := kindcluster.ValidateName(o.cluster); err != nil {
		return options{}, fmt.Errorf("%s: %w", clusterEnv, err)
	}
	var err error
	if o.reuse, err = boolVar(getenv, reuseEnv); err != nil {
		return options{}, err
	}
	if o.teardown, err = boolVar(getenv, teardownEnv); err != nil {
		return options{}, err
	}
	if v := strings.TrimSpace(getenv(stabilityEnv)); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return options{}, fmt.Errorf("%s=%q: want a positive Go duration such as 2m", stabilityEnv, v)
		}
		o.stability = d
	}
	if v := strings.TrimSpace(getenv(workersEnv)); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return options{}, fmt.Errorf("%s=%q: want a non-negative integer", workersEnv, v)
		}
		o.workers = n
	}
	return o, nil
}

// boolVar returns the boolean in the variable key read through getenv,
// false when unset. It returns an error naming key for anything
// strconv.ParseBool rejects.
func boolVar(getenv func(string) string, key string) (bool, error) {
	v := strings.TrimSpace(getenv(key))
	if v == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return false, fmt.Errorf("%s=%q: want a boolean (1, true, 0, false)", key, v)
	}
	return b, nil
}

// envConfig returns the env.Config for o: the process environment with
// the cluster name, reuse flag and worker count taken from o, so the
// development variables (TESTENV_NAME, CAPTF_TESTENV_REUSE) never steer
// the suite. It returns an error when the configuration is invalid.
func envConfig(o options) (env.Config, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return env.Config{}, err
	}
	cache, err := providers.DefaultCacheDir()
	if err != nil {
		return env.Config{}, err
	}
	getenv := func(key string) string {
		switch key {
		case env.NameEnv:
			return o.cluster
		case env.ReuseEnv:
			return strconv.FormatBool(o.reuse)
		case env.WorkersEnv:
			return strconv.Itoa(o.workers)
		case env.AllEnv:
			return ""
		default:
			return os.Getenv(key)
		}
	}
	return env.LoadConfig(getenv, cwd, cache)
}
