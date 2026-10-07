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

# Verifies the clusterctl components built from config/default:
#  1. exactly one Namespace, and every namespaced object is in it;
#  2. the provider label is on every object;
#  3. the Deployment has one container named `manager`, passes --leader-elect,
#     never --namespace, a real image, and only flags cmd/manager defines;
#     CAPTF_MANAGER_IMAGE equals the container image, POD_NAMESPACE and
#     SERVICE_ACCOUNT_NAME come from the downward API (metadata.namespace,
#     spec.serviceAccountName), and the `healthz` port
#     (used by both probes) is where --health-addr listens;
#  4. every CRD carries the contract label, whose value is a served and stored
#     version, and the contract matches metadata.yaml;
#  5. the identity CRD carries clusterctl's move-hierarchy label;
#  6. RBAC has no wildcards and no aggregate-to-manager label, the runner
#     ClusterRole matches the credentials contract, and the manager's `bind`
#     rule names exactly the built runner ClusterRole.
# Needs python3 with PyYAML. KUSTOMIZE overrides the kustomize binary, and
# MANAGER_BIN a prebuilt manager (default: `go run ./cmd/manager`).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
KUSTOMIZE="${KUSTOMIZE:-${ROOT}/hack/tools/bin/kustomize}"

tmp="$(mktemp -d)"
trap 'rm -rf -- "${tmp}"' EXIT

"${KUSTOMIZE}" build "${ROOT}/config/default" >"${tmp}/components.yaml"

# The manager's flag set, from its own usage text (exit status of -help varies
# by flag package, so only the output is used).
if [[ -n "${MANAGER_BIN:-}" ]]; then
	"${MANAGER_BIN}" --help >"${tmp}/help.txt" 2>&1 || true
else
	(cd "${ROOT}" && go run ./cmd/manager --help) >"${tmp}/help.txt" 2>&1 || true
fi
if ! grep -q -- '-leader-elect' "${tmp}/help.txt"; then
	echo "verify-components: could not read the manager's flags:" >&2
	cat "${tmp}/help.txt" >&2
	exit 1
fi

python3 - "${tmp}/components.yaml" "${tmp}/help.txt" "${ROOT}/metadata.yaml" <<'PY'
import re
import sys

import yaml

components_path, help_path, metadata_path = sys.argv[1:4]
objs = [o for o in yaml.safe_load_all(open(components_path)) if o]
failures = []


def fail(msg):
    failures.append(msg)


NAMESPACE = "captf-system"
PROVIDER_LABEL = ("cluster.x-k8s.io/provider", "infrastructure-terraform")
CLUSTER_SCOPED = {
    "Namespace",
    "CustomResourceDefinition",
    "ClusterRole",
    "ClusterRoleBinding",
    "ValidatingWebhookConfiguration",
    "MutatingWebhookConfiguration",
}


def name(o):
    return f'{o["kind"]}/{o["metadata"]["name"]}'


def labels(o):
    return o["metadata"].get("labels") or {}


# 1. Namespace.
namespaces = [o for o in objs if o["kind"] == "Namespace"]
if [o["metadata"]["name"] for o in namespaces] != [NAMESPACE]:
    fail(f"want exactly one Namespace {NAMESPACE}, got {[name(o) for o in namespaces]}")
for o in objs:
    ns = o["metadata"].get("namespace")
    if o["kind"] in CLUSTER_SCOPED:
        if ns:
            fail(f"{name(o)}: cluster-scoped object has namespace {ns}")
    elif ns != NAMESPACE:
        fail(f"{name(o)}: namespace {ns!r}, want {NAMESPACE}")

# 2. Provider label.
for o in objs:
    if labels(o).get(PROVIDER_LABEL[0]) != PROVIDER_LABEL[1]:
        fail(f"{name(o)}: missing label {PROVIDER_LABEL[0]}={PROVIDER_LABEL[1]}")

# 3. Deployment.
help_text = open(help_path).read()
flags = set(re.findall(r"^\s+(?:-\w, )?-{1,2}([A-Za-z0-9][-A-Za-z0-9_.]*)", help_text, re.M))
m = re.search(r'--health-addr string.*?\(default "([^"]*)"\)', help_text)
health_default = m.group(1) if m else None
deployments = [o for o in objs if o["kind"] == "Deployment"]
if len(deployments) != 1:
    fail(f"want exactly one Deployment, got {[name(o) for o in deployments]}")
for d in deployments:
    containers = d["spec"]["template"]["spec"]["containers"]
    if [c["name"] for c in containers] != ["manager"]:
        fail(f"{name(d)}: containers {[c['name'] for c in containers]}, want [manager]")
    for c in containers:
        args = c.get("args") or []
        if "--leader-elect" not in args:
            fail(f"{name(d)}: args lack --leader-elect")
        for a in args:
            m = re.match(r"^--?([^=]+)", a)
            if not m:
                fail(f"{name(d)}: arg {a!r} is not a flag")
                continue
            if m.group(1) == "namespace":
                fail(f"{name(d)}: --namespace must not be passed (clusterctl never injects it)")
            if m.group(1) not in flags:
                fail(f"{name(d)}: flag --{m.group(1)} is not defined by cmd/manager")
        image = c.get("image", "")
        if not image or image.startswith("controller"):
            fail(f"{name(d)}: image {image!r} was not set by config/default images")
        # The runner image defaults to the manager's own image.
        env = {e["name"]: e.get("value") for e in c.get("env") or []}
        if env.get("CAPTF_MANAGER_IMAGE") != image:
            fail(f"{name(d)}: env CAPTF_MANAGER_IMAGE={env.get('CAPTF_MANAGER_IMAGE')!r}, want the container image {image!r}")
        # The manager's identity (the only user the webhook lets set
        # providerID) comes from the downward API; without both it is empty
        # and no machine finishes provisioning.
        env_from = {e["name"]: ((e.get("valueFrom") or {}).get("fieldRef") or {}).get("fieldPath") for e in c.get("env") or []}
        for var, path in (("POD_NAMESPACE", "metadata.namespace"), ("SERVICE_ACCOUNT_NAME", "spec.serviceAccountName")):
            if env_from.get(var) != path:
                fail(f"{name(d)}: env {var} fieldRef is {env_from.get(var)!r}, want {path!r}")
        # The port named healthz must be where --health-addr listens.
        health = next((a.split("=", 1)[1] for a in args if a.startswith("--health-addr=")), health_default)
        ports = {p.get("name"): p.get("containerPort") for p in c.get("ports") or []}
        if health is None or ports.get("healthz") != int(health.rsplit(":", 1)[1]):
            fail(f"{name(d)}: containerPort healthz={ports.get('healthz')}, but --health-addr is {health!r}")
        for probe in ("livenessProbe", "readinessProbe"):
            if (c.get(probe) or {}).get("httpGet", {}).get("port") != "healthz":
                fail(f"{name(d)}: {probe} must use the healthz port")

# 4 and 5. CRDs.
metadata = yaml.safe_load(open(metadata_path))
if metadata.get("apiVersion") != "clusterctl.cluster.x-k8s.io/v1alpha3" or metadata.get("kind") != "Metadata":
    fail("metadata.yaml: want apiVersion clusterctl.cluster.x-k8s.io/v1alpha3, kind Metadata")
series = metadata.get("releaseSeries") or []
if not series:
    fail("metadata.yaml: releaseSeries is empty")
contracts = {s.get("contract") for s in series}
crds = [o for o in objs if o["kind"] == "CustomResourceDefinition"]
if len(crds) != 8:
    fail(f"want 8 CRDs, got {len(crds)}")
for crd in crds:
    ls = labels(crd)
    contract_labels = {k.split("/", 1)[1]: v for k, v in ls.items() if k.startswith("cluster.x-k8s.io/v1")}
    if set(contract_labels) != {"v1beta2"}:
        fail(f"{name(crd)}: contract labels {contract_labels}, want only cluster.x-k8s.io/v1beta2")
    if not set(contract_labels) <= contracts:
        fail(f"{name(crd)}: contract {sorted(contract_labels)} not in metadata.yaml {sorted(contracts)}")
    stored = {v["name"] for v in crd["spec"]["versions"] if v.get("served") and v.get("storage")}
    for version in contract_labels.values():
        if version not in stored:
            fail(f"{name(crd)}: contract label names {version}, served+stored versions are {sorted(stored)}")
    move = ls.get("clusterctl.cluster.x-k8s.io/move-hierarchy")
    if crd["spec"]["names"]["kind"] == "TerraformClusterIdentity":
        if move != "":
            fail(f"{name(crd)}: want label clusterctl.cluster.x-k8s.io/move-hierarchy: \"\"")
    elif move is not None:
        fail(f"{name(crd)}: unexpected move-hierarchy label")

# 6. RBAC.
roles = [o for o in objs if o["kind"] in ("ClusterRole", "Role")]
for r in roles:
    if "cluster.x-k8s.io/aggregate-to-manager" in labels(r):
        fail(f"{name(r)}: must not carry cluster.x-k8s.io/aggregate-to-manager")
    for rule in r.get("rules") or []:
        for field in ("apiGroups", "resources", "verbs", "resourceNames"):
            for v in rule.get(field) or []:
                if "*" in v:
                    fail(f"{name(r)}: wildcard {v!r} in {field}")


def rule_set(role):
    out = set()
    for rule in role.get("rules") or []:
        for g in rule.get("apiGroups") or []:
            for res in rule.get("resources") or []:
                for v in rule.get("verbs") or []:
                    out.add((g, res, v))
    return out


cluster_roles = {o["metadata"]["name"]: o for o in objs if o["kind"] == "ClusterRole"}
runner = cluster_roles.get("captf-runner")
if runner is None:
    fail("ClusterRole captf-runner is missing")
else:
    want = {("", "secrets", v) for v in ("get", "list", "create", "update", "delete")}
    want |= {("coordination.k8s.io", "leases", v) for v in ("get", "create", "update")}
    # Runner progress events (https://captf.io/docs/operator-guide/observability.html "Events"): create only.
    want.add(("events.k8s.io", "events", "create"))
    if rule_set(runner) != want:
        fail(f"ClusterRole/captf-runner: rules {sorted(rule_set(runner))}, want {sorted(want)}")
manager = cluster_roles.get("captf-manager-role")
if manager is None:
    fail("ClusterRole captf-manager-role is missing")
else:
    binds = [r for r in manager["rules"] if "bind" in r.get("verbs", [])]
    if len(binds) != 1 or binds[0].get("resources") != ["clusterroles"] or binds[0].get("verbs") != ["bind"]:
        fail(f"ClusterRole/captf-manager-role: want one bind-only rule on clusterroles, got {binds}")
    elif binds[0].get("resourceNames") != [runner["metadata"]["name"] if runner else "captf-runner"]:
        fail(f"ClusterRole/captf-manager-role: bind resourceNames {binds[0].get('resourceNames')}, want [captf-runner]")
    for rule in manager["rules"]:
        if rule.get("resourceNames") and rule.get("verbs") != ["bind"]:
            fail(f"ClusterRole/captf-manager-role: unexpected resourceNames rule {rule}")

if failures:
    for f in failures:
        print(f"verify-components: {f}", file=sys.stderr)
    sys.exit(1)
print(f"verify-components: {len(objs)} objects OK")
PY
