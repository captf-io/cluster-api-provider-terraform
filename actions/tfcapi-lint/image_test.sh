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

# Lints a known-good and a known-bad module of every role with the
# tfcapi-lint image, through tfcapi-lint.sh as the action runs it: each
# must exit as expected under --strict, with JSON byte-identical to the
# fixture's expected.json (the golden files TestFixtures checks the
# binary against). Needs docker and the image named by IMAGE (default
# ghcr.io/captf-io/tfcapi-lint:dev, what `make docker-build-lint` builds).
# Run by `make test-lint-image`.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
script="${root}/actions/tfcapi-lint/tfcapi-lint.sh"
image="${IMAGE:-ghcr.io/captf-io/tfcapi-lint:dev}"

work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

# dir role exit: the fixtures, relative to cmd/tfcapi-lint/testdata.
fixtures=(
	"good/cluster cluster 0"
	"good/machine machine 0"
	"good/machinepool machinepool 0"
	"bad/no-tags cluster 1"
	"bad/missing-output machine 1"
	"bad/pool-no-provider-id-list machinepool 1"
)

status=0
for f in "${fixtures[@]}"; do
	read -r dir role want <<<"${f}"
	rc=0
	# From cmd/tfcapi-lint, so the report's module path matches the golden.
	(cd "${root}/cmd/tfcapi-lint" && env GITHUB_WORKSPACE="${root}" GITHUB_OUTPUT=/dev/null \
		INPUT_IMAGE="${image}" INPUT_COMMAND=module INPUT_TARGET="testdata/${dir}" \
		INPUT_ROLE="${role}" INPUT_STRICT=true INPUT_JSON=true \
		"${script}") >"${work}/out.json" 2>"${work}/err" || rc=$?
	golden="${root}/cmd/tfcapi-lint/testdata/${dir}/expected.json"
	if [[ "${rc}" != "${want}" ]]; then
		echo "image_test: ${dir} (${role}): exit ${rc}, want ${want}" >&2
		cat "${work}/err" >&2
		status=1
	elif ! diff -u "${golden}" "${work}/out.json" >&2; then
		echo "image_test: ${dir} (${role}): output differs from ${golden#"${root}/"}" >&2
		status=1
	else
		echo "image_test: ${dir} (${role}): exit ${rc}: OK"
	fi
done
exit "${status}"
