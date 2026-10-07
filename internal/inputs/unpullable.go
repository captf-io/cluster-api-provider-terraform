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

package inputs

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MaxUnpullable caps UnpullableImagesAnnotation: a Job picks among at most
// three images (the pinned digest, the recorded tag and
// spec.source.image), so older entries no longer matter.
const MaxUnpullable = 3

// parseUnpullable returns the image references raw, an
// UnpullableImagesAnnotation value, lists; nil when it is empty or does
// not parse, which only makes the next Job try the pinned image again.
func parseUnpullable(raw string) []string {
	if raw == "" {
		return nil
	}
	var refs []string
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}
	return refs
}

// AddUnpullable records ref, an image a non-apply Job of owner could not
// pull, on owner's durable Secret (UnpullableImagesAnnotation) with one
// merge patch through c using ctx. current is the list as Read returned
// it: ref is appended unless listed already, and only the newest
// MaxUnpullable entries are kept. It returns the list now recorded and
// whether ref was added, ErrNotFound when the Secret does not exist, or
// any other patch error.
func AddUnpullable(ctx context.Context, c client.Client, owner client.Object, current []string, ref string) ([]string, bool, error) {
	if slices.Contains(current, ref) {
		return current, false, nil
	}
	refs := append(slices.Clone(current), ref)
	if len(refs) > MaxUnpullable {
		refs = refs[len(refs)-MaxUnpullable:]
	}
	raw, err := json.Marshal(refs)
	if err != nil {
		return nil, false, fmt.Errorf("inputs: encode unpullable images: %w", err)
	}
	if err := patchAnnotation(ctx, c, owner, UnpullableImagesAnnotation, string(raw)); err != nil {
		return nil, false, err
	}
	return refs, true, nil
}

// ClearUnpullable removes UnpullableImagesAnnotation from owner's durable
// Secret, with one merge patch through c using ctx, once a successful
// apply pinned the image it ran. It returns ErrNotFound when the Secret
// does not exist, or any other patch error.
func ClearUnpullable(ctx context.Context, c client.Client, owner client.Object) error {
	return patchAnnotation(ctx, c, owner, UnpullableImagesAnnotation, nil)
}
