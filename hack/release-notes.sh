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

# Usage: hack/release-notes.sh VERSION > notes.md
#
# Writes the GitHub Release body for the tag VERSION to stdout:
#
#   summary   the pre-alpha status, the API version, the module contract
#             (internal/contract) and the Cluster API contract
#             (metadata.yaml), all read at the tag
#   Install   `clusterctl init` with the hosted captf.io/clusterctl.yaml
#   Images    both images with :VERSION and their digests
#   Verify    cosign and `gh attestation verify`
#   Changes   the commit subjects since the previous tag, without merge
#             commits and Dependabot's dependency bumps ("First release."
#             when there is no previous tag)
#
# Image digests come from MANAGER_DIGEST and LINT_DIGEST (publish.yaml
# passes the ones it just pushed), else from the registry through
# hack/image-digest.sh. The tfcapi-lint image starts at LINT_FROM (v0.1.1:
# v0.1.0 shipped without one); for an earlier VERSION the notes say so
# instead of failing, and for a later one a missing digest is an error.
# GITHUB_REPOSITORY (default captf-io/cluster-api-provider-terraform)
# names the repository in the commands. Needs the tag and its history.
# The single-quoted printf formats hold literal Markdown backticks.
# shellcheck disable=SC2016
set -euo pipefail

version="${1:?usage: release-notes.sh VERSION}"
root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${root}"

repo="${GITHUB_REPOSITORY:-captf-io/cluster-api-provider-terraform}"
manager_image="${MANAGER_REPO:-ghcr.io/captf-io/cluster-api-provider-terraform}"
lint_image="${LINT_REPO:-ghcr.io/captf-io/tfcapi-lint}"
lint_from="${LINT_FROM:-v0.1.1}"

[[ "${version}" =~ ^v([0-9]+)\.([0-9]+)\.[0-9]+(-rc\.[0-9]+)?$ ]] ||
	{ echo "release-notes: VERSION ${version} is not vX.Y.Z or vX.Y.Z-rc.N" >&2; exit 1; }
major="${BASH_REMATCH[1]}"
minor="${BASH_REMATCH[2]}"
git rev-parse --verify --quiet "${version}^{commit}" >/dev/null ||
	{ echo "release-notes: no tag ${version} (fetch the tags)" >&2; exit 1; }

# The module contract and the API version at the tag.
contract="$(git show "${version}:internal/contract/version.go" | sed -n 's/^const Version = "\(.*\)"$/\1/p')"
api="$(git ls-tree -d --name-only "${version}:api/" | grep -E '^v[0-9]' | sort -V | tail -1)"
[[ -n "${contract}" && -n "${api}" ]] ||
	{ echo "release-notes: cannot read the contract and API versions at ${version}" >&2; exit 1; }

# The Cluster API contract of this release series, from metadata.yaml at the
# tag; the highest series when the release's own is missing.
capi_contract="$(git show "${version}:metadata.yaml" | awk -v maj="${major}" -v min="${minor}" '
	/^- major:/ { m = $3 }
	/^  minor:/ { n = $2 }
	/^  contract:/ { last = $2; if (m == maj && n == min) found = $2 }
	END { print (found != "" ? found : last) }')"
[[ -n "${capi_contract}" ]] ||
	{ echo "release-notes: cannot read the Cluster API contract from metadata.yaml at ${version}" >&2; exit 1; }

# digest_of REPO NAME prints the digest of REPO:VERSION, from the variable
# NAME when set, else from the registry; empty when there is none.
digest_of() {
	local img="$1" set="${2:-}"
	if [[ -n "${set}" ]]; then
		echo "${set}"
	else
		hack/image-digest.sh "${img}:${version}" 2>/dev/null || true
	fi
}

manager_digest="$(digest_of "${manager_image}" "${MANAGER_DIGEST:-}")"
[[ -n "${manager_digest}" ]] ||
	{ echo "release-notes: no digest for ${manager_image}:${version}" >&2; exit 1; }

lint_digest=""
lint_published=true
if [[ "$(printf '%s\n%s\n' "${lint_from}" "${version}" | sort -V | head -1)" != "${lint_from}" ]]; then
	# VERSION sorts before lint_from: the image does not exist for it.
	lint_published=false
else
	lint_digest="$(digest_of "${lint_image}" "${LINT_DIGEST:-}")"
	[[ -n "${lint_digest}" ]] ||
		{ echo "release-notes: no digest for ${lint_image}:${version}" >&2; exit 1; }
fi

status="Pre-alpha"
[[ "${major}" == 0 ]] || status="Release"

printf '## %s\n\n' "${version}"
printf '%s release of Cluster API Provider Terraform: the `infrastructure.cluster.x-k8s.io/%s` API, module image contract `%s`, Cluster API contract `%s`. The API and the contract may still change between releases.\n\n' \
	"${status}" "${api}" "${contract}" "${capi_contract}"

cat <<EOF
### Install

CAPTF is not a built-in \`clusterctl\` provider: register it with the hosted config, [captf.io/clusterctl.yaml](https://captf.io/clusterctl.yaml), and initialize this release:

\`\`\`sh
clusterctl init --config https://captf.io/clusterctl.yaml --infrastructure terraform:${version}
\`\`\`

See the [installation guide](https://captf.io/docs/operator-guide/installation/) for the prerequisites, what the install creates, and how to use your own \`clusterctl\` config instead.

### Images

| Image | Digest |
|---|---|
| \`${manager_image}:${version}\` | \`${manager_digest}\` |
EOF
if [[ "${lint_published}" == true ]]; then
	printf '| `%s:%s` | `%s` |\n' "${lint_image}" "${version}" "${lint_digest}"
else
	printf '| `%s` | none for this release: the image starts at `%s` |\n' "${lint_image}" "${lint_from}"
fi
printf '\nBoth are linux/amd64 and linux/arm64. Pin the digest in anything you deploy.\n\n'

cat <<EOF
### Verify

Images are signed with keyless cosign and carry SLSA provenance and SBOM attestations, made by this repository's publish workflow:

\`\`\`sh
cosign verify ${manager_image}:${version} \\
  --certificate-identity https://github.com/${repo}/.github/workflows/publish.yaml@refs/tags/${version} \\
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://${manager_image}:${version} -R ${repo}
\`\`\`

Release assets carry a provenance attestation too (the bundle is also attached as \`provenance.intoto.jsonl\`):

\`\`\`sh
gh release download ${version} -R ${repo} -p infrastructure-components.yaml
gh attestation verify infrastructure-components.yaml -R ${repo}
\`\`\`
EOF
if [[ "${lint_published}" == true ]]; then
	printf '\nThe same checks work for `%s:%s`, still with `-R %s`.\n' "${lint_image}" "${version}" "${repo}"
fi

printf '\n### Changes\n\n'
prev="$(git describe --tags --abbrev=0 --match 'v[0-9]*' "${version}^" 2>/dev/null || true)"
# Dependabot's bumps are filtered by author, so a maintainer's own deps:
# commit stays. -P is for the negative lookahead.
# The first release has no previous tag: its whole history is not a
# changelog.
changes=""
[[ -z "${prev}" ]] ||
	changes="$(git log --no-merges --perl-regexp --author='^(?!dependabot)' --format='- %s' "${prev}..${version}")"
if [[ -z "${prev}" ]]; then
	printf -- '- First release.\n'
elif [[ -n "${changes}" ]]; then
	printf '%s\n' "${changes}"
else
	printf -- '- No changes besides dependency updates.\n'
fi
if [[ -n "${prev}" ]]; then
	printf '\nFull diff: https://github.com/%s/compare/%s...%s\n' "${repo}" "${prev}" "${version}"
fi
if [[ "${lint_published}" != true ]]; then
	printf '\nThe `tfcapi-lint` image starts at %s; its binaries are attached to this release.\n' "${lint_from}"
fi
