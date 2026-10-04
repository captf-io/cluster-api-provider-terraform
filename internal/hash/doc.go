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

// Package hash computes captf.io/inputs-hash: a sha256 over a canonical
// JSON encoding of the Hashable struct.
//
// The canonical form follows RFC 8785 in spirit: object keys sorted, no
// insignificant whitespace, HTML characters unescaped, and numbers limited
// to integers written as -?(0|[1-9][0-9]*). Keys are sorted by bytes, which
// equals RFC 8785's UTF-16 order for the ASCII keys of the contract. The
// hash only needs to be self-consistent between CAPTF builds: it is not an
// RFC 8785 implementation, and for module-authored keys (captf_cluster_outputs)
// byte order and UTF-16 order may differ, as may the escaping of U+2028 and
// U+2029. A fractional or exponent number is rejected rather than
// normalized, so a value that cannot round-trip exactly can never change the
// hash silently. User module variables are the exception: they may hold any
// JSON number, which is hashed exactly as written (Hashable.Variables).
package hash
