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

package state

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Reader errors.
var (
	// ErrNoState means no state Secret exists: never applied, or deleted.
	ErrNoState = errors.New("state: no state")
	// ErrStateInconsistent means the chunk set is incomplete or malformed,
	// for example while a Job writes chunks one at a time.
	ErrStateInconsistent = errors.New("state: inconsistent chunk set")
	// ErrStateEncrypted means OpenTofu client-side state encryption is in
	// use; outputs cannot be read without the key (unsupported in v1).
	ErrStateEncrypted = errors.New("state: encrypted")
	// ErrUnsupportedStateVersion means the state file is not version 4.
	ErrUnsupportedStateVersion = errors.New("state: unsupported state file version")
	// ErrStateCorrupt means the payload is not gzip-compressed state JSON.
	ErrStateCorrupt = errors.New("state: corrupt")
)

// DataKey is the Secret data key holding the gzip-compressed state.
const DataKey = "tfstate"

// Limits on what the reader accepts. A kubernetes-backend Secret holds at
// most 1 MiB of compressed state, and real states compress 10–20×, so 32
// chunks and 64 MiB decompressed are far beyond any plausible cluster or
// machine state; beyond them the state is reported corrupt, not read.
const (
	MaxChunks     = 32
	MaxStateBytes = 64 << 20
)

// State is what the controller needs from a state file.
type State struct {
	Serial           int64
	Lineage          string
	TerraformVersion string
	// Outputs are the root module outputs. An empty map is valid: the
	// backends persist an empty state before the first apply.
	Outputs map[string]Output
	// Secrets are the Secrets the state was read from, base first.
	Secrets []types.NamespacedName
	// Metadata is the metadata of each of Secrets, in the same order, as
	// read: what an owner-reference repair checks and locks its patch to,
	// without listing the chunks again.
	Metadata []metav1.ObjectMeta
	// InputsHash is the InputsHashAnnotation of the base Secret, "" when
	// absent (no successful apply recorded yet).
	InputsHash string
	// ManagedResources counts the state's resources of mode "managed" (not
	// data sources); a resource with count or for_each counts once.
	ManagedResources int
	// Bytes is the compressed state's size summed over its chunks: the
	// kubernetes backend's Secrets hold at most 1 MiB each.
	Bytes int
}

// Output is one root output as stored in state. Value may be sensitive:
// never log it.
type Output struct {
	Value     json.RawMessage `json:"value"`
	Type      json.RawMessage `json:"type"`
	Sensitive bool            `json:"sensitive,omitempty"`
}

// Reader reads the state of one object.
type Reader interface {
	// Read returns the state of the object identified by suffix in
	// namespace, using ctx for the underlying calls.
	Read(ctx context.Context, namespace, suffix string) (*State, error)
}

// NewReader returns a Reader listing Secrets through c. Pass an uncached
// reader, or a client whose cache holds state Secrets.
func NewReader(c client.Reader) Reader {
	return &reader{c: c}
}

// reader is the default Reader, listing state Secrets through c.
type reader struct {
	c client.Reader
}

// chunk is one state Secret paired with its chunk index (0 for the base
// Secret, N for a "-part-N" Secret).
type chunk struct {
	index  int
	secret *corev1.Secret
}

// Read lists every chunk of suffix in namespace through r.c using ctx,
// reassembles and parses the state, and returns it.
func (r *reader) Read(ctx context.Context, namespace, suffix string) (*State, error) {
	var list corev1.SecretList
	if err := r.c.List(ctx, &list, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: Selector(suffix)}); err != nil {
		return nil, fmt.Errorf("state: list Secrets: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, ErrNoState
	}
	chunks, err := orderChunks(list.Items, suffix)
	if err != nil {
		return nil, err
	}
	var payload []byte
	names := make([]types.NamespacedName, 0, len(chunks))
	metas := make([]metav1.ObjectMeta, 0, len(chunks))
	for _, c := range chunks {
		data, ok := c.secret.Data[DataKey]
		if !ok {
			return nil, fmt.Errorf("%w: Secret %s has no %q key", ErrStateInconsistent, c.secret.Name, DataKey)
		}
		payload = append(payload, data...)
		names = append(names, types.NamespacedName{Namespace: c.secret.Namespace, Name: c.secret.Name})
		metas = append(metas, c.secret.ObjectMeta)
	}
	st, err := parse(payload, MaxStateBytes)
	if err != nil {
		return nil, err
	}
	st.Secrets = names
	st.Metadata = metas
	st.Bytes = len(payload)
	st.InputsHash = chunks[0].secret.Annotations[InputsHashAnnotation]
	return st, nil
}

// orderChunks sorts secrets, the state Secrets of suffix, by chunk index:
// the base Secret is 0 and "-part-N" is N, as Terraform's getSecrets does,
// and returns them in that order. A missing index, a duplicate or an
// unexpected name is ErrStateInconsistent.
func orderChunks(secrets []corev1.Secret, suffix string) ([]chunk, error) {
	if len(secrets) > MaxChunks {
		return nil, fmt.Errorf("%w: %d state Secrets, more than %d", ErrStateCorrupt, len(secrets), MaxChunks)
	}
	base := SecretName(suffix)
	chunks := make([]chunk, 0, len(secrets))
	for i := range secrets {
		s := &secrets[i]
		idx := 0
		if s.Name != base {
			n, ok := strings.CutPrefix(s.Name, base+"-part-")
			if !ok {
				return nil, fmt.Errorf("%w: unexpected state Secret name %s", ErrStateInconsistent, s.Name)
			}
			v, err := strconv.Atoi(n)
			if err != nil || v < 1 || strconv.Itoa(v) != n {
				return nil, fmt.Errorf("%w: unexpected chunk name %s", ErrStateInconsistent, s.Name)
			}
			idx = v
		}
		chunks = append(chunks, chunk{index: idx, secret: s})
	}
	slices.SortFunc(chunks, func(a, b chunk) int { return a.index - b.index })
	for i, c := range chunks {
		if c.index != i {
			return nil, fmt.Errorf("%w: chunk %d missing or duplicated among %d Secrets", ErrStateInconsistent, i, len(chunks))
		}
	}
	return chunks, nil
}

// stateFile is the subset of statefile v4 the controller reads.
type stateFile struct {
	Version           *int              `json:"version"`
	TerraformVersion  string            `json:"terraform_version"`
	Serial            int64             `json:"serial"`
	Lineage           string            `json:"lineage"`
	Outputs           map[string]Output `json:"outputs"`
	EncryptionVersion *string           `json:"encryption_version"`
	// Resources decodes only each entry's mode: the instances, with their
	// attribute values, are skipped.
	Resources []struct {
		Mode string `json:"mode"`
	} `json:"resources"`
}

// parse decompresses at most limit bytes of gz, the concatenated
// gzip-compressed state payload, decodes it and returns the resulting
// State.
func parse(gz []byte, limit int) (*State, error) {
	zr, err := gzip.NewReader(bytes.NewReader(gz))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %w", ErrStateCorrupt, err)
	}
	// A shrinking Put() (Terraform v1.16.4
	// internal/backend/remote-state/kubernetes/client.go: it writes the
	// lower chunks first, then deletes the surplus -part-N Secrets above the
	// new count) can leave a stale trailing chunk when one of those deletes
	// fails partway: the concatenated payload is then one complete, valid
	// gzip stream (the real state) followed by a fragment of the old one,
	// which is not a second gzip member. Multistream(false) stops reading
	// at the end of that first, real stream instead of treating the
	// leftover bytes as (or an attempt at) another one: verified, without
	// it gzip.Reader's default multistream mode returns a "gzip: invalid
	// header" error on exactly this shape, turning a self-healing leftover
	// into a permanent false StateCorrupt. It also means switching an
	// object from Terraform (chunked) to OpenTofu (single Secret, no
	// chunking at all in OpenTofu v1.12.6's client.go: verified, no `-part-`
	// or chunk logic exists there) on the same state Secrets tolerates
	// whatever Terraform left behind.
	zr.Multistream(false)
	// The Secrets are writable by anyone who can write Secrets in the
	// namespace, including every module image: an unbounded read is a gzip
	// bomb that OOM-kills the shared manager.
	raw, err := io.ReadAll(io.LimitReader(zr, int64(limit)+1))
	if err != nil {
		return nil, fmt.Errorf("%w: gunzip: %w", ErrStateCorrupt, err)
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("%w: decompressed state exceeds %d bytes", ErrStateCorrupt, limit)
	}
	var f stateFile
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("%w: JSON: %w", ErrStateCorrupt, err)
	}
	// The encrypted envelope has no version: check it first.
	if f.EncryptionVersion != nil {
		return nil, ErrStateEncrypted
	}
	if f.Version == nil || *f.Version != 4 {
		v := "missing"
		if f.Version != nil {
			v = strconv.Itoa(*f.Version)
		}
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedStateVersion, v)
	}
	outputs := f.Outputs
	if outputs == nil {
		outputs = map[string]Output{}
	}
	managed := 0
	for _, r := range f.Resources {
		if r.Mode == "managed" {
			managed++
		}
	}
	return &State{Serial: f.Serial, Lineage: f.Lineage, TerraformVersion: f.TerraformVersion, Outputs: outputs, ManagedResources: managed}, nil
}
