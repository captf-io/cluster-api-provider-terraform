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

# Usage: hack/require-ci.sh [COMMIT]   (default: HEAD)
#
# Succeeds only when the `ci` workflow has a successful push run on main for
# COMMIT. publish.yaml uses it to gate a tag push and a manual republish on
# the same checks a pull request had to pass. A run that is still queued or
# in progress is waited for; a failed, cancelled or missing run is an error.
#
# Needs an authenticated gh (GH_TOKEN with actions: read, GH_REPO or a git
# remote). Tunables, in seconds:
#
#   CI_WAIT_SECONDS    how long to wait for a running ci (default 1800)
#   CI_GRACE_SECONDS   how long to wait for a run to appear at all, since a
#                      tag can be pushed right behind its commit (default 120)
#   CI_POLL_SECONDS    the polling interval (default 30)
#
# Run locally, it answers "would publish accept this commit?"; the release
# preflight (`make release-preflight`) deliberately does not call it, so
# that stays offline. Use `make release-ci-check`.
set -euo pipefail

readonly WORKFLOW=ci.yaml
readonly BRANCH=main

wait_seconds="${CI_WAIT_SECONDS:-1800}"
grace_seconds="${CI_GRACE_SECONDS:-120}"
poll_seconds="${CI_POLL_SECONDS:-30}"

die() {
	echo "::error title=require-ci::$*" >&2
	exit 1
}

command -v gh >/dev/null || die "gh is required"

commit="$(git rev-parse --verify "${1:-HEAD}^{commit}" 2>/dev/null)" ||
	die "cannot resolve '${1:-HEAD}' to a commit"

# runs prints one "status conclusion url" line per ci push run on main for
# the commit, newest first.
runs() {
	gh run list --workflow "${WORKFLOW}" --branch "${BRANCH}" --event push \
		--commit "${commit}" --limit 20 \
		--json status,conclusion,url \
		--jq '.[] | "\(.status) \(.conclusion) \(.url)"'
}

start="${SECONDS}"
while true; do
	listing="$(runs)"
	elapsed=$((SECONDS - start))

	if grep -q '^completed success ' <<<"${listing}"; then
		echo "ci passed for ${commit}: $(grep -m1 '^completed success ' <<<"${listing}" | cut -d' ' -f3)"
		exit 0
	fi

	if [[ -n "${listing}" ]] && grep -qv '^completed ' <<<"${listing}"; then
		# Queued, in progress, waiting or pending: still a chance.
		if ((elapsed >= wait_seconds)); then
			die "ci for ${commit} did not finish within ${wait_seconds}s"
		fi
		echo "ci for ${commit} is still running (${elapsed}s); waiting"
	elif [[ -n "${listing}" ]]; then
		die "ci did not pass for ${commit}; publishing needs a green run on ${BRANCH}:
${listing}"
	else
		if ((elapsed >= grace_seconds)); then
			die "no ci run on ${BRANCH} for ${commit}: publish only accepts commits that were pushed to ${BRANCH} and passed ci"
		fi
		echo "no ci run for ${commit} yet (${elapsed}s); waiting"
	fi
	sleep "${poll_seconds}"
done
