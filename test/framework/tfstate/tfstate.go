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

package tfstate

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Kinds whose state SuffixFor names.
const (
	// KindTerraformCluster is the cluster kind (short name c).
	KindTerraformCluster = "TerraformCluster"
	// KindTerraformMachine is the machine kind (short name m).
	KindTerraformMachine = "TerraformMachine"
	// KindTerraformMachinePool is the machine pool kind (short name mp).
	KindTerraformMachinePool = "TerraformMachinePool"
)

// DataKey is the Secret data key holding the gzip-compressed state.
const DataKey = "tfstate"

// maxChunks bounds the -part-N Secrets Read follows, as the product does.
const maxChunks = 32

// maxStateBytes bounds the decompressed state, as the product does.
const maxStateBytes = 64 << 20

// ErrNoState means the base state Secret does not exist.
var ErrNoState = errors.New("tfstate: no state")

// ErrCorrupt means the Secret data is not gzip-compressed state JSON.
var ErrCorrupt = errors.New("tfstate: corrupt state")

// Output is one root output of the state.
type Output struct {
	// Value is the decoded JSON value of the output.
	Value any `json:"value"`
	// Sensitive is whether Terraform marked the output sensitive.
	Sensitive bool `json:"sensitive,omitempty"`
}

// State is the part of a Terraform state file the suites inspect.
type State struct {
	// Serial is the state serial, incremented by every write.
	Serial int64 `json:"serial"`
	// Lineage identifies the state's history.
	Lineage string `json:"lineage"`
	// TerraformVersion is the version that wrote the state; OpenTofu
	// writes its own version here.
	TerraformVersion string `json:"terraform_version"`
	// Outputs are the root module outputs by name.
	Outputs map[string]Output `json:"outputs"`
	// Resources are the state's resources, decoded generically.
	Resources []any `json:"resources"`
}

// SuffixFor returns the backend secret suffix of the object of kind named
// name in namespace: the first 16 hex characters of
// sha256("<namespace>/<kind>/<name>"), a dash, and c, m or mp for
// TerraformCluster, TerraformMachine or TerraformMachinePool. It returns ""
// for any other kind, so a wrong kind shows up as a missing Secret name
// rather than a panic.
func SuffixFor(namespace, kind, name string) string {
	var short string
	switch kind {
	case KindTerraformCluster:
		short = "c"
	case KindTerraformMachine:
		short = "m"
	case KindTerraformMachinePool:
		short = "mp"
	default:
		return ""
	}
	sum := sha256.Sum256([]byte(namespace + "/" + kind + "/" + name))
	return hex.EncodeToString(sum[:])[:16] + "-" + short
}

// SecretName returns the name of the base state Secret of suffix, in the
// "default" workspace.
func SecretName(suffix string) string {
	return "tfstate-default-" + suffix
}

// Read returns the state of the object with suffix in namespace, read
// through kube using ctx. It gets the base Secret and then
// SecretName(suffix)+"-part-N" for N = 1, 2, ... until one is missing,
// concatenates their "tfstate" values, gunzips the result and decodes it. A
// missing base Secret wraps ErrNoState; undecodable data wraps ErrCorrupt.
func Read(ctx context.Context, kube kubernetes.Interface, namespace, suffix string) (*State, error) {
	base := SecretName(suffix)
	var payload []byte
	for i := 0; i <= maxChunks; i++ {
		name := base
		if i > 0 {
			name = base + "-part-" + strconv.Itoa(i)
		}
		s, err := kube.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			if i == 0 {
				return nil, fmt.Errorf("%w: Secret %s/%s", ErrNoState, namespace, name)
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("tfstate: get Secret %s/%s: %w", namespace, name, err)
		}
		data, ok := s.Data[DataKey]
		if !ok {
			return nil, fmt.Errorf("%w: Secret %s/%s has no %q key", ErrCorrupt, namespace, name, DataKey)
		}
		payload = append(payload, data...)
		if i == maxChunks {
			return nil, fmt.Errorf("%w: more than %d chunks", ErrCorrupt, maxChunks)
		}
	}
	return decode(payload)
}

// decode gunzips payload (the first gzip member only, like the product, so a
// stale trailing chunk is ignored) and decodes the state JSON. It returns an
// error wrapping ErrCorrupt when either step fails.
func decode(payload []byte) (*State, error) {
	zr, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("%w: gzip: %w", ErrCorrupt, err)
	}
	zr.Multistream(false)
	raw, err := io.ReadAll(io.LimitReader(zr, maxStateBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: gunzip: %w", ErrCorrupt, err)
	}
	if len(raw) > maxStateBytes {
		return nil, fmt.Errorf("%w: decompressed state exceeds %d bytes", ErrCorrupt, maxStateBytes)
	}
	var st State
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("%w: JSON: %w", ErrCorrupt, err)
	}
	if st.Outputs == nil {
		st.Outputs = map[string]Output{}
	}
	return &st, nil
}
