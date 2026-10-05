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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
)

// The Terraform* kinds whose state this package names.
const (
	KindTerraformCluster     = "TerraformCluster"
	KindTerraformMachine     = "TerraformMachine"
	KindTerraformMachinePool = "TerraformMachinePool"
)

// ErrUnknownKind is returned for a kind without a short name.
var ErrUnknownKind = errors.New("state: unknown kind")

// KindShort returns the short kind used in suffixes and names: c, m or mp.
func KindShort(kind string) (string, error) {
	switch kind {
	case KindTerraformCluster:
		return "c", nil
	case KindTerraformMachine:
		return "m", nil
	case KindTerraformMachinePool:
		return "mp", nil
	default:
		return "", fmt.Errorf("%w: %q", ErrUnknownKind, kind)
	}
}

// Suffix returns the backend secret_suffix of an object:
// hex(sha256(namespace/kind/name))[:16] + "-" + KindShort(kind).
//
// It depends only on identity that survives clusterctl move (never the UID),
// always fits a label value, and never ends in "-<digits>", which Terraform
// rejects because it parses chunk indexes from the last segment.
func Suffix(namespace, kind, name string) (string, error) {
	short, err := KindShort(kind)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(namespace + "/" + kind + "/" + name))
	return hex.EncodeToString(sum[:])[:16] + "-" + short, nil
}

// SecretName returns the name of the base state Secret of suffix (workspace
// "default"). Terraform adds chunks named SecretName(suffix)+"-part-N".
func SecretName(suffix string) string {
	return "tfstate-default-" + suffix
}

// LeaseName returns the name of the state lock Lease of suffix.
func LeaseName(suffix string) string {
	return "lock-" + SecretName(suffix)
}
