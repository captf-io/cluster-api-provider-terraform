#!/usr/bin/env bash
# Verifies the Go workspace:
#  - no replace directives in go.work or any go.mod;
#  - go.work and every go.mod declare the same go version;
#  - controller-runtime and the k8s.io staging modules resolve to the pinned
#    versions in each module on its own (GOWORK=off) and in the workspace.
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
MODULES=(. api test)

declare -A PINS=(
	[sigs.k8s.io/controller-runtime]=v0.24.1
	[k8s.io/api]=v0.36.3
	[k8s.io/apimachinery]=v0.36.3
	[k8s.io/client-go]=v0.36.3
	[k8s.io/apiextensions-apiserver]=v0.36.3
	[k8s.io/component-base]=v0.36.3
)

fail=0
err() {
	echo "verify-modules: $*" >&2
	fail=1
}

cd "${ROOT}"

work_go="$(go work edit -json | jq -r '.Go')"
if [[ "$(go work edit -json | jq '.Replace // [] | length')" != 0 ]]; then
	err "go.work contains replace directives"
fi

for m in "${MODULES[@]}"; do
	gomod="${ROOT}/${m}/go.mod"
	json="$(go mod edit -json "${gomod}")"

	if [[ "$(jq '.Replace // [] | length' <<<"${json}")" != 0 ]]; then
		err "${m}/go.mod contains replace directives"
	fi

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
			if [[ -n "${got}" && "${got}" != "${want}" ]]; then
				err "${m} (${mode}): ${path} is ${got}, want ${want}"
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
