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

# Guards the test tiers: e2e and envtest code must never reach the default
# runner. A tier is a build tag and the directories that may use it:
#
#   e2e      test/e2e/, test/env/lifecycle/
#   envtest  internal/envtest/
#
# For each tier:
#   - Every .go file under its directories must carry `//go:build <tag>` as
#     its first line, or as the first line after a leading copyright comment
#     block.
#   - No other .go file in the repository (.git, .claude, bin and hack/tools
#     excluded) may mention the tag in a build constraint.
#
# Usage: hack/verify-test-tiers.sh [root]   (default: the repository root)
# Run by `make verify-test-tiers`; hack/verify-test-tiers_test.sh tests it.
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "${root}"

# The tiers: build tag, then the directories whose files must carry it.
tiers=(e2e envtest)
declare -A tier_dirs=(
	[e2e]="test/e2e test/env/lifecycle"
	[envtest]="internal/envtest"
)

# first_code_line FILE: print the first line that is not blank and not part
# of a leading /* ... */ comment block mentioning Copyright.
first_code_line() {
	awk '
		function end_block() {
			in_block = 0
			if (block !~ /Copyright/) { print first; exit }
		}
		in_block { block = block "\n" $0; if ($0 ~ /\*\//) end_block(); next }
		/^[[:space:]]*$/ { next }
		!seen && /^\/\*/ {
			seen = 1; in_block = 1; first = $0; block = $0
			if ($0 ~ /\*\//) end_block()
			next
		}
		{ print; exit }
	' "$1"
}

status=0
all_dirs=()
for tag in "${tiers[@]}"; do
	# shellcheck disable=SC2206 # the directory lists hold no spaces
	dirs=(${tier_dirs[${tag}]})
	all_dirs+=("${dirs[@]}")
	for dir in "${dirs[@]}"; do
		[[ -d "${dir}" ]] || continue
		while IFS= read -r -d '' f; do
			line="$(first_code_line "${f}")"
			if [[ "${line}" != "//go:build ${tag}" ]]; then
				echo "verify-test-tiers: ${f}: first line must be //go:build ${tag} (after the license header, if any), found: ${line:-<nothing>}" >&2
				status=1
			fi
		done < <(find "${dir}" -name '*.go' -type f -print0)
	done

	# Build constraints that mention the tag, outside the tier's directories.
	prune=(-path ./.git -o -path ./.claude -o -path ./bin -o -path ./hack/tools)
	for dir in "${dirs[@]}"; do
		prune+=(-o -path "./${dir}")
	done
	while IFS= read -r -d '' f; do
		if grep -En "^//(go:build|[[:space:]]*\+build)([[:space:]].*)?[^[:alnum:]_.]${tag}([^[:alnum:]_.]|\$)" "${f}" >/dev/null; then
			echo "verify-test-tiers: ${f}: only files under ${dirs[*]} may use the ${tag} build tag" >&2
			status=1
		fi
	done < <(find . \( "${prune[@]}" \) -prune -o -name '*.go' -type f -print0)
done

if [[ ${status} -eq 0 ]]; then
	echo "verify-test-tiers: tier code is tagged and confined to ${all_dirs[*]}"
fi
exit "${status}"
