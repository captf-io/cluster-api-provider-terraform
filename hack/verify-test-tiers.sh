#!/usr/bin/env bash
# Guards the test tiers: e2e code must never reach the default runner.
#
#   - Every .go file under test/e2e/ and test/env/lifecycle/ must carry
#     `//go:build e2e` as its first line, or as the first line after a
#     leading copyright comment block.
#   - No other .go file in the repository (.git, .claude, bin and hack/tools
#     excluded) may mention the e2e build tag in a build constraint.
#
# Usage: hack/verify-test-tiers.sh [root]   (default: the repository root)
# Run by `make verify-test-tiers`; hack/verify-test-tiers_test.sh tests it.
set -euo pipefail

root="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
cd "${root}"

# The directories whose files must be e2e-tagged.
e2e_dirs=(test/e2e test/env/lifecycle)

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
for dir in "${e2e_dirs[@]}"; do
	[[ -d "${dir}" ]] || continue
	while IFS= read -r -d '' f; do
		line="$(first_code_line "${f}")"
		if [[ "${line}" != "//go:build e2e" ]]; then
			echo "verify-test-tiers: ${f}: first line must be //go:build e2e (after the license header, if any), found: ${line:-<nothing>}" >&2
			status=1
		fi
	done < <(find "${dir}" -name '*.go' -type f -print0)
done

# Build constraints that mention the e2e tag, outside the e2e directories.
while IFS= read -r -d '' f; do
	if grep -En '^//(go:build|[[:space:]]*\+build)([[:space:]].*)?[^[:alnum:]_.]e2e([^[:alnum:]_.]|$)' "${f}" >/dev/null; then
		echo "verify-test-tiers: ${f}: only files under ${e2e_dirs[*]} may use the e2e build tag" >&2
		status=1
	fi
done < <(find . \( -path ./.git -o -path ./.claude -o -path ./bin -o -path ./hack/tools \
	-o -path ./test/e2e -o -path ./test/env/lifecycle \) -prune -o -name '*.go' -type f -print0)

if [[ ${status} -eq 0 ]]; then
	echo "verify-test-tiers: e2e code is tagged and confined to ${e2e_dirs[*]}"
fi
exit "${status}"
