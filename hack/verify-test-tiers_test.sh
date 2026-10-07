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

# Tests hack/verify-test-tiers.sh on throwaway trees: a well-tiered tree
# passes, and each kind of violation fails. Run by `make verify-test-tiers`.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
script="${root}/hack/verify-test-tiers.sh"

work="$(mktemp -d "${HOME}/tmp/captf-tiers-test-XXXXXX" 2>/dev/null || mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

license='/*
Copyright 2026 The CAPTF Authors.
*/'

# tree NAME: create an empty, well-tiered tree and print its path.
tree() {
	local t="${work}/$1"
	mkdir -p "${t}/test/e2e/suite" "${t}/test/env/lifecycle" "${t}/internal/x" "${t}/internal/envtest" "${t}/hack/tools/src" "${t}/bin"
	printf '//go:build e2e\n\n%s\n\npackage lifecycle\n' "${license}" >"${t}/test/env/lifecycle/a_test.go"
	printf '%s\n\n//go:build e2e\n\npackage suite\n' "${license}" >"${t}/test/e2e/suite/b_test.go"
	printf '%s\n\npackage x\n' "${license}" >"${t}/internal/x/x.go"
	printf '%s\n\n//go:build envtest\n\npackage envtest\n' "${license}" >"${t}/internal/envtest/c_test.go"
	# Ignored locations may carry anything.
	printf '//go:build e2e\n\npackage src\n' >"${t}/hack/tools/src/s.go"
	printf '//go:build e2e\n\npackage bin\n' >"${t}/bin/b.go"
	echo "${t}"
}

status=0
# expect NAME WANT DIR: the script run on DIR must exit with WANT (pass or
# fail).
expect() {
	local name=$1 want=$2 dir=$3 got
	if "${script}" "${dir}" >"${work}/out" 2>&1; then got=pass; else got=fail; fi
	if [[ "${got}" != "${want}" ]]; then
		echo "verify-test-tiers_test: ${name}: got ${got}, want ${want}:" >&2
		cat "${work}/out" >&2
		status=1
	else
		echo "verify-test-tiers_test: ${name}: ${got}: OK"
	fi
}

expect "well-tiered tree" pass "$(tree good)"

t="$(tree untagged)"
printf '%s\n\npackage suite\n' "${license}" >"${t}/test/e2e/suite/c_test.go"
expect "untagged e2e file" fail "${t}"

t="$(tree late-tag)"
printf 'package lifecycle\n\n//go:build e2e\n' >"${t}/test/env/lifecycle/d.go"
expect "tag after the package clause" fail "${t}"

t="$(tree other-comment)"
printf '/*\nNot a license.\n*/\n\n//go:build e2e\n\npackage suite\n' >"${t}/test/e2e/suite/e_test.go"
expect "tag after a non-copyright block" fail "${t}"

t="$(tree stray)"
printf '//go:build e2e\n\npackage x\n' >"${t}/internal/x/y_test.go"
expect "e2e tag outside the e2e directories" fail "${t}"

t="$(tree stray-expr)"
printf '//go:build linux && !e2e\n\npackage x\n' >"${t}/internal/x/z.go"
expect "e2e in a build expression outside" fail "${t}"

t="$(tree legacy)"
printf '// +build e2e\n\npackage x\n' >"${t}/internal/x/w.go"
expect "legacy +build e2e outside" fail "${t}"

t="$(tree lookalike)"
printf '//go:build e2etest\n\npackage x\n' >"${t}/internal/x/v.go"
expect "a different tag that starts with e2e" pass "${t}"

t="$(tree untagged-envtest)"
printf '%s\n\npackage envtest\n' "${license}" >"${t}/internal/envtest/d_test.go"
expect "untagged envtest file" fail "${t}"

t="$(tree stray-envtest)"
printf '//go:build envtest\n\npackage x\n' >"${t}/internal/x/e_test.go"
expect "envtest tag outside internal/envtest" fail "${t}"

t="$(tree envtest-in-e2e)"
printf '//go:build envtest\n\npackage suite\n' >"${t}/test/e2e/suite/f_test.go"
expect "envtest tag in an e2e directory" fail "${t}"

t="$(tree e2e-in-envtest)"
printf '//go:build e2e\n\npackage envtest\n' >"${t}/internal/envtest/g_test.go"
expect "e2e tag in the envtest directory" fail "${t}"

t="$(tree envtest-expr)"
printf '//go:build linux && !envtest\n\npackage x\n' >"${t}/internal/x/h.go"
expect "envtest in a build expression outside" fail "${t}"

exit "${status}"
