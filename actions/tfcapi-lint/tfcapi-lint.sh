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

# The tfcapi-lint GitHub Action (action.yml): runs the tfcapi-lint image
# with docker. Inputs arrive as INPUT_* environment variables, never as
# expanded expressions, so no input is ever parsed as shell.
#
# Image selection, first match wins:
#   INPUT_IMAGE    used as given; pulled only when not present locally
#   INPUT_VERSION  ghcr.io/captf-io/tfcapi-lint:<version>
#   the action ref, read from GITHUB_ACTION_PATH (the runner checks a remote
#   action out at .../_actions/<owner>/<repo>/<ref>/actions/tfcapi-lint;
#   github.action_ref is empty in composite run steps, actions/runner#2473):
#     vX.Y.Z[-rc.N]  :vX.Y.Z[-rc.N]  (the release)
#     <40-hex sha>   :sha-<7>        (that commit's main build)
#     main           :edge
# So a pinned action runs the linter built from the same commit.
#
# `tfcapi-lint.sh --resolve` prints the image and exits (the tests use it).
set -euo pipefail

readonly REPO=ghcr.io/captf-io/tfcapi-lint

die() {
	echo "::error title=tfcapi-lint::$*" >&2
	exit 3
}

# resolve_image prints the image to run, from the inputs and the action ref.
resolve_image() {
	if [[ -n "${INPUT_IMAGE:-}" ]]; then
		echo "${INPUT_IMAGE}"
		return
	fi
	local version="${INPUT_VERSION:-}"
	if [[ -z "${version}" ]]; then
		local path="${GITHUB_ACTION_PATH:-}" ref
		if [[ "${path}" =~ /_actions/[^/]+/[^/]+/(.+)/actions/tfcapi-lint/?$ ]]; then
			ref="${BASH_REMATCH[1]}"
		else
			die "cannot tell this action's ref from '${path}' (a local action?); set the version or image input"
		fi
		if [[ "${ref}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
			version="${ref}"
		elif [[ "${ref}" =~ ^[0-9a-f]{40}$ ]]; then
			version="sha-${ref:0:7}"
		elif [[ "${ref}" == main ]]; then
			version=edge
		else
			die "no tfcapi-lint image is published for action ref '${ref}'; pin a release tag, a commit on main or main, or set the version input"
		fi
	fi
	[[ "${version}" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "version '${version}' is not an image tag"
	echo "${REPO}:${version}"
}

# is_true reports whether an action input is "true" (any case).
is_true() {
	[[ "${1,,}" == true ]]
}

image="$(resolve_image)"
if [[ "${1:-}" == --resolve ]]; then
	echo "${image}"
	exit 0
fi

command="${INPUT_COMMAND:-}"
[[ "${command}" == module || "${command}" == image ]] || die "command must be module or image, not '${command}'"
[[ -n "${INPUT_TARGET:-}" ]] || die "target is required"
[[ -n "${INPUT_ROLE:-}" ]] || die "role is required"

args=("${command}" --role "${INPUT_ROLE}")
if is_true "${INPUT_STRICT:-false}"; then args+=(--strict); fi
if is_true "${INPUT_JSON:-false}"; then args+=(--json); fi
if [[ -n "${INPUT_CONTRACT:-}" ]]; then args+=(--contract "${INPUT_CONTRACT}"); fi
# Word splitting is the point: IDs separated by spaces or newlines.
for id in ${INPUT_ALLOW_WARNINGS:-}; do args+=(--allow-warning "${id}"); done
if [[ "${command}" == image ]]; then
	if [[ -n "${INPUT_PLATFORM:-}" ]]; then args+=(--platform "${INPUT_PLATFORM}"); fi
	if is_true "${INPUT_ALL_PLATFORMS:-false}"; then args+=(--all-platforms); fi
	if is_true "${INPUT_INSECURE:-false}"; then args+=(--insecure); fi
fi
if [[ -n "${INPUT_ARGS:-}" ]]; then
	# -d '': every line, not just the first; read fails at the end of input.
	read -r -d '' -a extra <<<"${INPUT_ARGS}" || true
	args+=("${extra[@]}")
fi
args+=("${INPUT_TARGET}")

# The workspace and the runner's temp directory are mounted read-only at
# their own paths, so relative and absolute paths (a module directory, an
# oci:<dir> layout) mean the same inside the container. The registry
# credentials of `docker login` are mounted for `tfcapi-lint image`; the
# container runs as the runner's user so it can read them.
workspace="${GITHUB_WORKSPACE:-${PWD}}"
mounts=(-v "${workspace}:${workspace}:ro")
under=("${workspace}")
if [[ -n "${RUNNER_TEMP:-}" && -d "${RUNNER_TEMP}" ]]; then
	mounts+=(-v "${RUNNER_TEMP}:${RUNNER_TEMP}:ro")
	under+=("${RUNNER_TEMP}")
fi
inside=false
for dir in "${under[@]}"; do
	if [[ "${PWD}/" == "${dir%/}/"* ]]; then inside=true; fi
done
${inside} || die "the working directory ${PWD} is outside the workspace and RUNNER_TEMP, which are all the container sees"
docker_config="${DOCKER_CONFIG:-${HOME}/.docker}"
if [[ -f "${docker_config}/config.json" ]]; then
	mounts+=(-v "${docker_config}:/captf/docker-config:ro" -e DOCKER_CONFIG=/captf/docker-config)
	# The image has no credential helpers: only inline auths work in it.
	if grep -Eq '"(credsStore|credHelpers)"' "${docker_config}/config.json"; then
		echo "::warning title=tfcapi-lint::${docker_config}/config.json uses a credential helper, which the tfcapi-lint container cannot run; a private registry will be pulled anonymously" >&2
	fi
fi

# A derived tag is always pulled (edge and sha-* tags of a reused
# self-hosted runner could be stale); an explicit image may be local.
pull=missing
if [[ -z "${INPUT_IMAGE:-}" ]]; then pull=always; fi

echo "tfcapi-lint: ${image}" >&2
echo "image=${image}" >>"${GITHUB_OUTPUT:-/dev/null}"

# --network host: a registry on the runner (a service container on
# localhost) is reachable as it is from the runner.
exec docker run --rm --pull "${pull}" --network host \
	--user "$(id -u):$(id -g)" --security-opt label=disable \
	"${mounts[@]}" -w "${PWD}" "${image}" "${args[@]}"
