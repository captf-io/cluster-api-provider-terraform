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

package strutil

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// maxName is the Secret name limit (DNS-1123 subdomain).
const maxName = 253

// BoundedName returns prefix+kindShort+"-"+name. A result longer than 253
// characters keeps its start and gets "-" plus the first 16 hex digits of the
// SHA-256 of name appended (trailing "-" and "." trimmed from the kept
// start), so it stays deterministic and unique. prefix is the caller's name
// prefix, kindShort the short kind, and name the object name the hash is
// taken over.
func BoundedName(prefix, kindShort, name string) string {
	full := prefix + kindShort + "-" + name
	if len(full) <= maxName {
		return full
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "-" + hex.EncodeToString(sum[:])[:16]
	head := strings.TrimRight(full[:maxName-len(suffix)], "-.")
	return head + suffix
}
