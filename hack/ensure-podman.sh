#!/usr/bin/env bash
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
