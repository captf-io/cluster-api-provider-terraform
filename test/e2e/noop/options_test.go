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

package noop

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/env"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/kindcluster"
	"github.com/captf-io/cluster-api-provider-terraform/test/framework/providers"
)

// The environment variables the suite reads, besides framework.EngineEnv
// and greenlight.MaxAgeEnv.
const (
	// clusterEnv names the green-lit cluster; default defaultCluster.
	clusterEnv = "CAPTF_E2E_CLUSTER"
	// badDigestEnv, when true, points machine B at a digest that does not
	// exist (badDigest), so stage 3 must fail on the image pull. It exists
	// only for the suite's negative check; test/README.md documents it.
	badDigestEnv = "CAPTF_E2E_NOOP_BAD_DIGEST"
)

// defaultCluster is the e2e cluster's name, as the foundation suite
// creates it.
const defaultCluster = framework.ClusterNamePrefix + "e2e"

// badDigest is the digest badDigestEnv substitutes: well formed, so the
// webhook admits it, but no registry holds it.
const badDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

// options is the suite's configuration, read once from the environment.
type options struct {
	// cluster is the kind cluster name; it passes kindcluster.ValidateName.
	cluster string
	// badDigest points machine B at badDigest (the negative check).
	badDigest bool
}

// optionsFrom reads the suite's options through getenv. It returns an
// error naming the variable at fault for an invalid cluster name or
// boolean.
func optionsFrom(getenv func(string) string) (options, error) {
	o := options{cluster: strings.TrimSpace(getenv(clusterEnv))}
	if o.cluster == "" {
		o.cluster = defaultCluster
	}
	if err := kindcluster.ValidateName(o.cluster); err != nil {
		return options{}, fmt.Errorf("%s: %w", clusterEnv, err)
	}
	if v := strings.TrimSpace(getenv(badDigestEnv)); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return options{}, fmt.Errorf("%s=%q: want a boolean (1, true, 0, false)", badDigestEnv, v)
		}
		o.badDigest = b
	}
	return o, nil
}

// envConfig returns the env.Config for o: the process environment with
// the cluster name taken from o and reuse forced on, so the development
// variables (TESTENV_NAME, CAPTF_TESTENV_REUSE) never steer the suite.
// The suite only reads the configuration (work directory, kubeconfig,
// engine) and collects diagnostics; it never creates or changes the
// cluster. It returns an error when the configuration is invalid.
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
			return "true"
		case env.WorkersEnv, env.AllEnv:
			return ""
		default:
			return os.Getenv(key)
		}
	}
	return env.LoadConfig(getenv, cwd, cache)
}
