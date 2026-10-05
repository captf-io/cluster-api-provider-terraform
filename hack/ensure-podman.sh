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

# Fails with a clear message when the container tool (default: podman) is
# not installed. Usage: hack/ensure-podman.sh [tool]
set -euo pipefail

tool="${1:-podman}"

if ! command -v "${tool}" >/dev/null 2>&1; then
	echo "error: '${tool}' not found in PATH." >&2
	echo "The container targets (docker-build, docker-buildx, docker-push) need it." >&2
	echo "On Fedora: sudo dnf install podman. Or set CONTAINER_TOOL=<tool>." >&2
	exit 1
fi

echo "using ${tool}: $(command -v "${tool}")"
