#!/usr/bin/env bash
# Tests hack/check-metadata.sh against the fixtures in hack/testdata/metadata
# and the real metadata.yaml. Run by `make verify-metadata`.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
check="${root}/hack/check-metadata.sh"
fx="${root}/hack/testdata/metadata"

status=0
expect() { # expect WANT_EXIT NAME ARGS...: run the check, compare exit codes
  local want=$1 name=$2 got=0
  shift 2
  "${check}" "$@" >/dev/null 2>&1 || got=$?
  if [[ "${got}" == "${want}" ]]; then
    echo "check-metadata_test: ${name}: OK"
  else
    echo "check-metadata_test: ${name}: exit ${got}, want ${want}" >&2
    status=1
  fi
}

expect 0 "repo metadata.yaml, validate only" "" "${root}/metadata.yaml"
expect 0 "repo metadata.yaml against itself" "${root}/metadata.yaml" "${root}/metadata.yaml"
expect 0 "series added" "${root}/metadata.yaml" "${fx}/two-series.yaml"
expect 0 "older series keep an older contract" "" "${fx}/older-contract.yaml"
expect 1 "series removed" "${fx}/two-series.yaml" "${fx}/one-series.yaml"
expect 1 "contract of an existing series changed" "${fx}/two-series.yaml" "${fx}/changed-contract.yaml"
expect 1 "wrong apiVersion" "" "${fx}/bad-apiversion.yaml"
expect 1 "no series" "" "${fx}/empty-series.yaml"
expect 1 "series listed twice" "" "${fx}/duplicate-series.yaml"
expect 1 "newest contract differs from the CRD label" "" "${fx}/wrong-newest-contract.yaml"
exit "${status}"
