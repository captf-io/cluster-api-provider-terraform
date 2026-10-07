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
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

// MaxUnpullable caps UnpullableImagesAnnotation: a Job picks among at most
// four images (the pinned digest, the recorded tag, the source and
// spec.source.image), so older entries no longer matter.
const MaxUnpullable = 4

// UnpullableTTL is how long an image a Job could not pull is passed over:
// after it, the next Job tries that image again, as it may have been
// pushed back, and records it again if it still does not pull.
const UnpullableTTL = time.Hour

// Unpullable is one image a non-apply Job could not pull
// (UnpullableImagesAnnotation).
type Unpullable struct {
	// Ref is the image reference.
	Ref string `json:"ref"`
	// At is when it was recorded.
	At time.Time `json:"at"`
}

// parseUnpullable returns the entries raw, an UnpullableImagesAnnotation
// value, lists; nil when it is empty or does not parse, which only makes
// the next Job try the pinned image again. A list of bare references, as
// earlier releases wrote it, reads with zero times: already expired.
func parseUnpullable(raw string) []Unpullable {
	if raw == "" {
		return nil
	}
	var parsed []Unpullable
	if err := json.Unmarshal([]byte(raw), &parsed); err == nil {
		return parsed
	}
	var refs []string
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		return nil
	}
	entries := make([]Unpullable, 0, len(refs))
	for _, r := range refs {
		entries = append(entries, Unpullable{Ref: r})
	}
	return entries
}

// UnpullableRefs returns the references of entries recorded within
// UnpullableTTL before now, oldest first: the images a Job passes over.
func UnpullableRefs(entries []Unpullable, now time.Time) []string {
	var out []string
	for _, e := range entries {
		if now.Sub(e.At) < UnpullableTTL {
			out = append(out, e.Ref)
		}
	}
	return out
}

// AddUnpullable records ref, an image a non-apply Job of owner could not
// pull, at now, on owner's durable Secret (UnpullableImagesAnnotation)
// with one merge patch through c using ctx. current is the list as Read
// returned it: an entry for ref is replaced by the new one, entries
// expired at now are dropped, and only the newest MaxUnpullable are kept.
// It returns the list now recorded and whether ref was added (false when
// it was listed and still current, which costs no call), ErrNotFound when
// the Secret does not exist, or any other patch error.
func AddUnpullable(ctx context.Context, c client.Client, owner client.Object, current []Unpullable, ref string, now time.Time) ([]Unpullable, bool, error) {
	if slices.Contains(UnpullableRefs(current, now), ref) {
		return current, false, nil
	}
	entries := slices.DeleteFunc(slices.Clone(current), func(e Unpullable) bool {
		return e.Ref == ref || now.Sub(e.At) >= UnpullableTTL
	})
	entries = append(entries, Unpullable{Ref: ref, At: now.UTC().Truncate(time.Second)})
	if len(entries) > MaxUnpullable {
		entries = entries[len(entries)-MaxUnpullable:]
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return nil, false, fmt.Errorf("inputs: encode unpullable images: %w", err)
	}
	if err := patchAnnotation(ctx, c, owner, UnpullableImagesAnnotation, string(raw)); err != nil {
		return nil, false, err
	}
	return entries, true, nil
}

// ClearUnpullable removes UnpullableImagesAnnotation from owner's durable
// Secret, with one merge patch through c using ctx, once a successful
// apply pinned the image it ran. It returns ErrNotFound when the Secret
// does not exist, or any other patch error.
func ClearUnpullable(ctx context.Context, c client.Client, owner client.Object) error {
	return patchAnnotation(ctx, c, owner, UnpullableImagesAnnotation, nil)
}
