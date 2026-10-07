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

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/captf-io/cluster-api-provider-terraform/internal/inputs"
	"github.com/captf-io/cluster-api-provider-terraform/internal/render"
)

// seedJob is the apply Job name the records writeInputs leaves name.
const seedJob = "captf-m-m1-apply-a1-seeded"

// testMeta is what an earlier apply ran with, for writeInputs.
type testMeta struct {
	Image, Identity, IdentityKind string
	// ImageDigest, when set, marks the apply as one that succeeded: only
	// that pins a digest.
	ImageDigest string
	// InputsHash is the inputs hash the apply rendered; "" when the test
	// does not care.
	InputsHash string
}

// writeInputs records files as the inputs an earlier apply of owner ran
// with, through c using ctx: the attempt record, and, when meta has a
// digest, the applied record as well. It returns any write error.
func writeInputs(ctx context.Context, c client.Client, owner client.Object, files render.Files, meta testMeta) error {
	rec := inputs.Record{
		Files: files, Image: meta.Image, Identity: meta.Identity, IdentityKind: meta.IdentityKind,
		InputsHash: meta.InputsHash, Job: seedJob,
	}
	if err := inputs.WriteAttempt(ctx, c, owner, rec); err != nil {
		return err
	}
	if meta.ImageDigest == "" {
		return nil
	}
	rec.Digest = meta.ImageDigest
	return inputs.Promote(ctx, c, owner, rec)
}
