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

# Renders templates/ with the pinned clusterctl, as users will:
#   - cluster-template.yaml with every variable set: succeeds, and every
#     object lands in --target-namespace;
#   - the same without TERRAFORM_MACHINE_IMAGE: fails, naming the variable;
#   - cluster-template-clusterclass.yaml, and clusterclass-noop.yaml (no
#     variables) via `clusterctl generate yaml`;
#   - identity.yaml via `clusterctl generate yaml`.
# HOME, XDG_CONFIG_HOME and KUBECONFIG point into a temp dir, so clusterctl
# never reads the user's config or kubeconfig. The typed checks live in
# templates/templates_test.go.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
clusterctl="${CLUSTERCTL:-${root}/hack/tools/bin/clusterctl}"
work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT
export HOME="${work}" XDG_CONFIG_HOME="${work}/.config" KUBECONFIG="${work}/no-kubeconfig"

export CLUSTER_NAME=verify KUBERNETES_VERSION=v1.36.2 CONTROL_PLANE_MACHINE_COUNT=3 WORKER_MACHINE_COUNT=2 \
  TERRAFORM_CLUSTER_IMAGE=ghcr.io/example/cluster:v1 TERRAFORM_MACHINE_IMAGE=ghcr.io/example/machine:v1 \
  TERRAFORM_IDENTITY_NAME=example NAMESPACE=verify-ns

status=0
"${clusterctl}" generate cluster verify --from "${root}/templates/cluster-template.yaml" \
  --target-namespace verify-ns >"${work}/cluster.yaml"
objects="$(grep -c '^kind:' "${work}/cluster.yaml")"
namespaced="$(grep -c '^  namespace: verify-ns$' "${work}/cluster.yaml")"
if [[ "${objects}" == 9 && "${namespaced}" == 9 ]]; then
  echo "verify-templates: cluster-template.yaml renders 9 objects in verify-ns"
else
  echo "verify-templates: cluster-template.yaml: ${objects} objects, ${namespaced} in verify-ns, want 9 and 9" >&2
  status=1
fi

if (unset TERRAFORM_MACHINE_IMAGE && "${clusterctl}" generate cluster verify \
  --from "${root}/templates/cluster-template.yaml" --target-namespace verify-ns) >/dev/null 2>"${work}/err"; then
  echo "verify-templates: rendering without TERRAFORM_MACHINE_IMAGE succeeded" >&2
  status=1
elif grep -q TERRAFORM_MACHINE_IMAGE "${work}/err"; then
  echo "verify-templates: missing TERRAFORM_MACHINE_IMAGE fails: $(tr -s '\n' ' ' <"${work}/err")"
else
  echo "verify-templates: missing TERRAFORM_MACHINE_IMAGE fails without naming it: $(cat "${work}/err")" >&2
  status=1
fi

"${clusterctl}" generate cluster verify --from "${root}/templates/cluster-template-clusterclass.yaml" \
  --target-namespace verify-ns >"${work}/cc-cluster.yaml"
if grep -q '^kind: Cluster$' "${work}/cc-cluster.yaml" && grep -q 'value: ghcr.io/example/machine:v1' "${work}/cc-cluster.yaml"; then
  echo "verify-templates: cluster-template-clusterclass.yaml renders"
else
  echo "verify-templates: cluster-template-clusterclass.yaml did not render as expected" >&2
  status=1
fi

# A ClusterClass file has no variables: it renders unchanged.
"${clusterctl}" generate yaml --from "${root}/templates/clusterclass-noop.yaml" >"${work}/cc.yaml"
if grep -q '^kind: ClusterClass$' "${work}/cc.yaml"; then
  echo "verify-templates: clusterclass-noop.yaml renders"
else
  echo "verify-templates: clusterclass-noop.yaml did not render" >&2
  status=1
fi

"${clusterctl}" generate yaml --from "${root}/templates/identity.yaml" >"${work}/identity.yaml"
if grep -q -- '- verify-ns' "${work}/identity.yaml" && grep -q 'namespace: captf-system' "${work}/identity.yaml"; then
  echo "verify-templates: identity.yaml renders"
else
  echo "verify-templates: identity.yaml did not render as expected" >&2
  status=1
fi

exit "${status}"
