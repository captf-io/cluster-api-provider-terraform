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

package feature

import (
	"testing"

	"github.com/spf13/pflag"
	logsv1 "k8s.io/component-base/logs/api/v1"
)

// TestNewGates proves NewGates registers the --feature-gates flag, rejects
// an unknown gate name, and returns a fresh instance on each call rather
// than a shared global.
func TestNewGates(t *testing.T) {
	t.Parallel()
	g := NewGates()
	fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
	g.AddFlag(fs)
	if fs.Lookup("feature-gates") == nil {
		t.Fatal("--feature-gates not registered")
	}
	// v1 defines no CAPTF gates; an unknown gate is rejected.
	if err := fs.Parse([]string{"--feature-gates=Bogus=true"}); err == nil {
		t.Error("unknown feature gate accepted")
	}
	// The component-base logging gates are registered, so logsv1 can
	// query them and --feature-gates can set them. A fresh set: the
	// rejected parse above leaves its bad entry behind in g.
	lg := NewGates()
	if err := lg.Set("ContextualLogging=false"); err != nil {
		t.Fatalf("ContextualLogging not settable: %v", err)
	}
	if lg.Enabled(logsv1.ContextualLogging) {
		t.Error("ContextualLogging=false did not take")
	}
	// Each call returns an independent gate set, not a shared global.
	if NewGates() == g {
		t.Error("NewGates returned a shared instance")
	}
}
