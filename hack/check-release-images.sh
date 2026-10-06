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

# Usage: hack/check-release-images.sh VERSION
#
# Fails unless BOTH release images carry :VERSION in the registry: the
# manager (ghcr.io/captf-io/cluster-api-provider-terraform) and the linter
# (ghcr.io/captf-io/tfcapi-lint). v0.1.0 was released without a
# tfcapi-lint:v0.1.0, so a missing tag must stop the release, not pass
# silently.
#
# When MANAGER_DIGEST and LINT_DIGEST are set (publish.yaml passes the
# digests the image job just pushed), each tag must also resolve to exactly
# that digest, so the tag a user pulls is the image that was signed and
# attested. Repositories default to the Makefile's RELEASE_REPO and
# RELEASE_LINT_REPO; override with MANAGER_REPO and LINT_REPO.
set -euo pipefail

version="${1:?usage: check-release-images.sh VERSION}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

failed=0
check() {
	local repo="$1" want="${2:-}" got
	if ! got="$("${root}/hack/image-digest.sh" "${repo}:${version}")"; then
		echo "::error::${repo}:${version} does not exist in the registry" >&2
		failed=1
	elif [[ -n "${want}" && "${got}" != "${want}" ]]; then
		echo "::error::${repo}:${version} is ${got}, expected the pushed ${want}" >&2
		failed=1
	else
		echo "${repo}:${version} ${got}"
	fi
}

check "${MANAGER_REPO:-ghcr.io/captf-io/cluster-api-provider-terraform}" "${MANAGER_DIGEST:-}"
check "${LINT_REPO:-ghcr.io/captf-io/tfcapi-lint}" "${LINT_DIGEST:-}"
exit "${failed}"
