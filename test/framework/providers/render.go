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

package providers

import (
	"context"
	"fmt"

	"github.com/captf-io/cluster-api-provider-terraform/test/framework/engine"
)

// CAPTFVersion is the CAPTF provider version the environment installs. It
// falls in the 0.1 release series of the repository's metadata.yaml
// (contract v1beta2), and is the version directory of the local
// repository, so it never has to match a real release.
const CAPTFVersion = "v0.1.0"

// RenderCAPTF renders the CAPTF provider from the tree under ctx by running
// `make manifests-release RELEASE_IMG=<image> RELEASE_DIR=<outDir>` through
// r, which must run in the repository root (engine.OSRunner{Dir: root})
// with make's tools available. outDir then holds
// infrastructure-components.yaml with the manager image set to image,
// metadata.yaml and the cluster templates. It returns an error wrapping
// the make failure.
func RenderCAPTF(ctx context.Context, r engine.Runner, image, outDir string) error {
	if image == "" || outDir == "" {
		return fmt.Errorf("providers: render: image and outDir are required")
	}
	if _, err := r.Run(ctx, "make", "manifests-release", "RELEASE_IMG="+image, "RELEASE_DIR="+outDir); err != nil {
		return fmt.Errorf("providers: render: %w", err)
	}
	return nil
}
