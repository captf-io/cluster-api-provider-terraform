#!/usr/bin/env bash
# Copyright 2026 The CAPTF Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Checks that no MPL-2.0 source tfcapi-lint links carries Exhibit B, the
# "Incompatible With Secondary Licenses" notice, which would forbid linking
# it into the Apache-2.0 binary (see https://captf.io/docs/reference/third-party-licenses.html).
#
# Every MPL-2.0 LICENSE file contains the phrase as the licence's own
# definitions (§1.5, §3.3, §10.4), so LICENSE* files are excluded: only a
# source file that applies the notice counts.
set -euo pipefail

modules=(
  github.com/hashicorp/terraform-config-inspect
  github.com/hashicorp/hcl/v2
  github.com/hashicorp/hcl
)

status=0
for m in "${modules[@]}"; do
  dir="$(go list -m -f '{{.Dir}}' "$m")"
  version="$(go list -m -f '{{.Version}}' "$m")"
  if [[ -z "$dir" || ! -d "$dir" ]]; then
    echo "check-licenses: $m is not in the module cache (go mod download)" >&2
    status=1
    continue
  fi
  if hits="$(grep -rIl --exclude='LICENSE*' 'Incompatible With Secondary Licenses' "$dir")"; then
    echo "check-licenses: $m $version applies Exhibit B in:" >&2
    echo "$hits" >&2
    status=1
  else
    echo "check-licenses: $m $version: no source file carries Exhibit B"
  fi
done
exit "$status"
