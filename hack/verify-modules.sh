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

# Verifies the Go workspace:
#  - no replace directives in go.work or any go.mod, except a go.mod
#    replacing one of this repository's own modules with its local
#    directory (the root module's api => ./api). Tools that ignore go.work
#    (Dependabot, go mod tidy, GOWORK=off) need it to resolve the api
#    module without fetching it, and `go install ...@version` cannot work
#    here anyway (the api module is not versioned);
#  - go.work and every go.mod declare the same go version;
#  - controller-runtime and the k8s.io staging modules stay on the pinned
#    minor version (they move together with a Cluster API release), and
#    every module resolves the same patch version, on its own (GOWORK=off)
#    and in the workspace. Patch bumps are fine as long as they land in all
#    modules at once.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULES=(. api test)

# Pinned minor versions ("v0.36" matches v0.36.x).
declare -A PINS=(
	[sigs.k8s.io/controller-runtime]=v0.24
	[k8s.io/api]=v0.36
	[k8s.io/apimachinery]=v0.36
	[k8s.io/client-go]=v0.36
	[k8s.io/apiextensions-apiserver]=v0.36
	[k8s.io/component-base]=v0.36
)

# First version seen for each pinned path, and where: every other module
# and mode must resolve the same one.
declare -A SEEN=()
declare -A SEEN_AT=()

fail=0
err() {
	echo "verify-modules: $*" >&2
	fail=1
}

cd "${ROOT}"

REPO_MOD="$(go mod edit -json "${ROOT}/go.mod" | jq -r '.Module.Path')"

work_go="$(go work edit -json | jq -r '.Go')"
if [[ "$(go work edit -json | jq '.Replace // [] | length')" != 0 ]]; then
	err "go.work contains replace directives"
fi

for m in "${MODULES[@]}"; do
	gomod="${ROOT}/${m}/go.mod"
	json="$(go mod edit -json "${gomod}")"

	# Only replaces of this repository's own modules by a local path that
	# holds that module are allowed.
	while IFS=$'\t' read -r old new; do
		[[ -z "${old}" ]] && continue
		if [[ "${old}" != "${REPO_MOD}/"* || "${new}" != ./* && "${new}" != ../* ]]; then
			err "${m}/go.mod replaces ${old} => ${new}; only this repository's modules may be replaced, by a local path"
			continue
		fi
		target="$(cd "${ROOT}/${m}" && go mod edit -json "${new}/go.mod" 2>/dev/null | jq -r '.Module.Path' || true)"
		if [[ "${target}" != "${old}" ]]; then
			err "${m}/go.mod replaces ${old} => ${new}, but ${new}/go.mod is module ${target:-<none>}"
		fi
	done < <(jq -r '.Replace // [] | .[] | [.Old.Path, .New.Path] | @tsv' <<<"${json}")

	mod_go="$(jq -r '.Go' <<<"${json}")"
	if [[ "${mod_go}" != "${work_go}" ]]; then
		err "${m}/go.mod declares go ${mod_go}, go.work declares go ${work_go}"
	fi

	for path in "${!PINS[@]}"; do
		want="${PINS[${path}]}"
		for mode in module workspace; do
			if [[ "${mode}" == module ]]; then gowork=off; else gowork="${ROOT}/go.work"; fi
			got="$(cd "${ROOT}/${m}" && GOWORK="${gowork}" go list -m -e -f '{{if not .Error}}{{.Version}}{{end}}' "${path}")"
			# A module outside this module's graph is not a disagreement.
			if [[ -z "${got}" ]]; then
				continue
			fi
			if [[ "${got}" != "${want}" && "${got}" != "${want}."* ]]; then
				err "${m} (${mode}): ${path} is ${got}, want ${want}.x"
			fi
			if [[ -z "${SEEN[${path}]:-}" ]]; then
				SEEN[${path}]="${got}"
				SEEN_AT[${path}]="${m} (${mode})"
			elif [[ "${got}" != "${SEEN[${path}]}" ]]; then
				err "${m} (${mode}): ${path} is ${got}, but ${SEEN_AT[${path}]} has ${SEEN[${path}]}; bump every module together"
			fi
		done
	done
done

# go.work must use exactly MODULES (the Makefile's GO_MODULES), so no module
# escapes the per-module checks above.
want_use="$(printf '%s\n' "${MODULES[@]}" | sed 's|^\.$|.|; s|^\([^.]\)|./\1|' | sort)"
got_use="$(go work edit -json | jq -r '.Use[].DiskPath' | sort)"
if [[ "${got_use}" != "${want_use}" ]]; then
	err "go.work uses [$(paste -sd' ' <<<"${got_use}")], want [$(paste -sd' ' <<<"${want_use}")]"
fi

# The test module is independent of the product: it never imports the root
# or api module, so test code can't reach into product internals and the
# product never depends on test-only code.
root_mod="$(go mod edit -json "${ROOT}/go.mod" | jq -r '.Module.Path')"
test_deps="$(cd "${ROOT}/test" && go list -deps -test -f '{{with .Module}}{{.Path}}{{end}}' ./...)"
bad="$(grep -x -e "${root_mod}" -e "${root_mod}/api" <<<"${test_deps}" | sort -u || true)"
if [[ -n "${bad}" ]]; then
	err "test module imports product module(s): $(paste -sd' ' <<<"${bad}")"
fi

if [[ "${fail}" != 0 ]]; then
	exit 1
fi
echo "verify-modules: OK"
