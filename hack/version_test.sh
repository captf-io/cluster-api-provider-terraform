#!/usr/bin/env bash
# Tests hack/version.sh in throwaway git repositories: every case must print
# the expected form, and every output must be a valid semantic version.
# Run by `make verify-version`.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/hack/version.sh"

# The same grammar as version.sh, kept separate so a bug in one is caught
# by the other.
num='(0|[1-9][0-9]*)'
ident='([0-9]*[A-Za-z-][0-9A-Za-z-]*|0|[1-9][0-9]*)'
semver="^v${num}\.${num}\.${num}(-${ident}(\.${ident})*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$"

work="$(mktemp -d "${HOME}/tmp/captf-version-test-XXXXXX" 2>/dev/null || mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

# Isolate git from the user's configuration (commit signing, hooks).
export HOME="${work}/home" GIT_CONFIG_NOSYSTEM=1
mkdir -p "${HOME}"
git config --global user.name test
git config --global user.email test@example.invalid
git config --global init.defaultBranch main
git config --global commit.gpgsign false
git config --global tag.gpgsign false

status=0
# check NAME DIR PATTERN: version.sh run in DIR must match PATTERN and semver.
check() {
	local name=$1 dir=$2 want=$3 got
	got="$(cd "${dir}" && "${script}")"
	if [[ ! "${got}" =~ ${want} ]]; then
		echo "version_test: ${name}: got ${got}, want ${want}" >&2
		status=1
	elif [[ ! "${got}" =~ ${semver} ]]; then
		echo "version_test: ${name}: ${got} is not a semantic version" >&2
		status=1
	else
		echo "version_test: ${name}: ${got}: OK"
	fi
}

mkdir -p "${work}/plain"
check "not a git checkout" "${work}/plain" '^v0\.0\.0-dev$'

repo="${work}/repo"
mkdir -p "${repo}"
(cd "${repo}" && git init -q)
check "no commits" "${repo}" '^v0\.0\.0-dev$'

(cd "${repo}" && echo a >f && git add f && git commit -qm one)
check "untagged, clean" "${repo}" '^v0\.0\.0-dev\.g[0-9a-f]{12}$'

(cd "${repo}" && echo b >f)
check "untagged, dirty" "${repo}" '^v0\.0\.0-dev\.g[0-9a-f]{12}\.dirty$'

(cd "${repo}" && git commit -qam two && git tag latest)
check "only a non-release tag" "${repo}" '^v0\.0\.0-dev\.g[0-9a-f]{12}$'

(cd "${repo}" && git tag -a v0.1.0 -m v0.1.0)
check "at a release tag" "${repo}" '^v0\.1\.0$'

(cd "${repo}" && echo c >f && git commit -qam three)
check "after a release tag" "${repo}" '^v0\.1\.0-1-g[0-9a-f]{12}$'

(cd "${repo}" && echo d >f)
check "after a release tag, dirty" "${repo}" '^v0\.1\.0-1-g[0-9a-f]{12}-dirty$'

# The leading-zero case the "g" prefix exists for: a bare all-digit hash
# with a leading zero is not semver, the prefixed form is.
for v in v0.0.0-dev.012345678901 v0.0.0-dev.g012345678901; do
	if [[ "${v}" =~ ${semver} ]]; then ok=valid; else ok=invalid; fi
	echo "version_test: grammar: ${v} is ${ok}"
done
if [[ v0.0.0-dev.012345678901 =~ ${semver} || ! v0.0.0-dev.g012345678901 =~ ${semver} ]]; then
	echo "version_test: grammar: leading-zero handling is wrong" >&2
	status=1
fi

exit "${status}"
