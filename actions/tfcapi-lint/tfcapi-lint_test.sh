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

# Tests the image selection of tfcapi-lint.sh (`--resolve`): the image and
# version inputs, and every form of action ref. Running the image is
# covered by the action jobs in .github/workflows/ci.yaml.
# Run by `make verify-action`.
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/tfcapi-lint.sh"
actions=/home/runner/work/_actions/captf-io/cluster-api-provider-terraform
sha=0123456789abcdef0123456789abcdef01234567

status=0
# check NAME WANT [VAR=VALUE...]: --resolve with only the given inputs must
# print WANT, or fail when WANT is "fail".
check() {
	local name=$1 want=$2 got rc=0
	shift 2
	got="$(env -i PATH="${PATH}" "$@" "${script}" --resolve 2>/dev/null)" || rc=$?
	if [[ "${want}" == fail ]]; then
		if ((rc == 0)); then
			echo "tfcapi-lint_test: ${name}: got ${got}, want a failure" >&2
			status=1
			return
		fi
		got=fail
	elif ((rc != 0)); then
		echo "tfcapi-lint_test: ${name}: exit ${rc}, want ${want}" >&2
		status=1
		return
	fi
	if [[ "${got}" != "${want}" ]]; then
		echo "tfcapi-lint_test: ${name}: got ${got}, want ${want}" >&2
		status=1
	else
		echo "tfcapi-lint_test: ${name}: ${got}: OK"
	fi
}

repo=ghcr.io/captf-io/tfcapi-lint
check "release ref" "${repo}:v0.2.0" GITHUB_ACTION_PATH="${actions}/v0.2.0/actions/tfcapi-lint"
check "rc ref" "${repo}:v0.2.0-rc.1" GITHUB_ACTION_PATH="${actions}/v0.2.0-rc.1/actions/tfcapi-lint"
check "commit ref" "${repo}:sha-0123456" GITHUB_ACTION_PATH="${actions}/${sha}/actions/tfcapi-lint"
check "main" "${repo}:edge" GITHUB_ACTION_PATH="${actions}/main/actions/tfcapi-lint"
check "fork" "${repo}:v0.2.0" GITHUB_ACTION_PATH=/r/_actions/someone/fork/v0.2.0/actions/tfcapi-lint
check "other branch" fail GITHUB_ACTION_PATH="${actions}/feature/x/actions/tfcapi-lint"
check "short sha" fail GITHUB_ACTION_PATH="${actions}/0123456/actions/tfcapi-lint"
check "local action" fail GITHUB_ACTION_PATH=/home/runner/work/repo/repo/actions/tfcapi-lint
check "no action path" fail
check "version input" "${repo}:edge" INPUT_VERSION=edge GITHUB_ACTION_PATH="${actions}/v0.2.0/actions/tfcapi-lint"
check "version input, local" "${repo}:v0.1.0" INPUT_VERSION=v0.1.0
check "bad version input" fail INPUT_VERSION='v1 --rm'
check "image input" "tfcapi-lint:ci" INPUT_IMAGE=tfcapi-lint:ci INPUT_VERSION=edge
exit "${status}"
