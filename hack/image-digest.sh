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

# Usage: hack/image-digest.sh IMAGE:TAG
#
# Prints the registry digest (sha256:...) of IMAGE:TAG: the manifest-list
# digest for a multi-arch image, which is what clusters pull. Exits 1 with
# nothing on stdout when the tag does not exist (or the registry cannot be
# read). Uses skopeo ($SKOPEO, default skopeo) when installed, else
# `docker buildx imagetools`, which the GitHub-hosted runners have.
set -euo pipefail

ref="${1:?usage: image-digest.sh IMAGE:TAG}"
skopeo="${SKOPEO:-skopeo}"

if command -v "${skopeo}" >/dev/null; then
	digest="$("${skopeo}" inspect --format '{{.Digest}}' "docker://${ref}" 2>/dev/null)" || exit 1
else
	digest="$(docker buildx imagetools inspect "${ref}" --format '{{.Manifest.Digest}}' 2>/dev/null)" || exit 1
fi

[[ "${digest}" =~ ^sha256:[0-9a-f]{64}$ ]] || exit 1
echo "${digest}"
