#!/usr/bin/env bash
# Checks the release assets through clusterctl's local repository, with no
# cluster:
#   1. `make manifests-release` into a temp dir, laid out as
#      <repo>/infrastructure-terraform/<version>/ (the provider label is the
#      directory; the config entry is `name: terraform`);
#   2. `clusterctl generate provider --infrastructure terraform:<version>`
#      reads the components and metadata from that repository;
#   3. `clusterctl generate cluster --infrastructure terraform:<version>`
#      finds cluster-template.yaml and, with --flavor clusterclass,
#      cluster-template-clusterclass.yaml by its release name.
# GOPROXY=off, and HOME/XDG_CONFIG_HOME/KUBECONFIG point into the temp dir,
# so nothing is fetched and no user config or cluster is read.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
clusterctl="${CLUSTERCTL:-${root}/hack/tools/bin/clusterctl}"
version="v0.1.0"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

dir="${work}/repository/infrastructure-terraform/${version}"
make -s -C "${root}" manifests-release RELEASE_DIR="${dir}" VERSION="${version}" >/dev/null
cat >"${work}/clusterctl.yaml" <<EOF
providers:
- name: terraform
  type: InfrastructureProvider
  url: file://${dir}/infrastructure-components.yaml
EOF

export HOME="${work}" XDG_CONFIG_HOME="${work}/.config" KUBECONFIG="${work}/no-kubeconfig" GOPROXY=off
export CLUSTER_NAME=smoke KUBERNETES_VERSION=v1.36.2 CONTROL_PLANE_MACHINE_COUNT=3 WORKER_MACHINE_COUNT=1 \
  TERRAFORM_CLUSTER_IMAGE=ghcr.io/example/cluster:v1 TERRAFORM_MACHINE_IMAGE=ghcr.io/example/machine:v1 \
  TERRAFORM_IDENTITY_NAME=example

status=0
if "${clusterctl}" generate provider --config "${work}/clusterctl.yaml" \
  --infrastructure "terraform:${version}" >"${work}/provider.yaml" 2>"${work}/err" &&
  grep -q "ghcr.io/captf-io/cluster-api-provider-terraform:${version}" "${work}/provider.yaml"; then
  echo "verify-local-repository: generate provider terraform:${version} OK"
else
  echo "verify-local-repository: generate provider failed: $(cat "${work}/err")" >&2
  status=1
fi
for flavor in "" clusterclass; do
  label="${flavor:-default}"
  if "${clusterctl}" generate cluster smoke --config "${work}/clusterctl.yaml" \
    --infrastructure "terraform:${version}" ${flavor:+--flavor "${flavor}"} --target-namespace smoke \
    >"${work}/cluster.yaml" 2>"${work}/err" && grep -q '^kind: Cluster$' "${work}/cluster.yaml"; then
    echo "verify-local-repository: generate cluster, ${label} flavor, OK"
  else
    echo "verify-local-repository: generate cluster, ${label} flavor, failed: $(cat "${work}/err")" >&2
    status=1
  fi
done
exit "${status}"
