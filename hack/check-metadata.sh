#!/usr/bin/env bash
# Validates metadata.yaml (clusterctl provider metadata) and checks that its
# releaseSeries is append-only.
#
#   hack/check-metadata.sh [OLD [NEW]]
#
# NEW defaults to metadata.yaml at the repo root. OLD is the previous
# release's metadata.yaml; when omitted it is read from the newest v* git
# tag before HEAD (a tag on HEAD itself is the release being checked), and
# when there is no git repository or no earlier tag only NEW is validated.
#
# Checks on NEW (clusterctl >= 1.11 validates the first three strictly):
#   - apiVersion clusterctl.cluster.x-k8s.io/v1alpha3, kind Metadata;
#   - at least one releaseSeries entry, each with integer major/minor >= 0
#     and a contract, and no (major, minor) listed twice;
#   - the highest series' contract equals the CRD contract label key
#     (cluster.x-k8s.io/<contract> in config/crd/kustomization.yaml); older
#     series keep the contract they shipped with.
# Append-only: every (major, minor) in OLD is in NEW with the same contract.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
new="${2:-${root}/metadata.yaml}"
old="${1:-}"
crd_kustomization="${CRD_KUSTOMIZATION:-${root}/config/crd/kustomization.yaml}"

work="$(mktemp -d)"
trap 'rm -rf -- "${work}"' EXIT

if [[ -z "${old}" ]]; then
  # The previous release: the newest v* tag before HEAD. When HEAD itself
  # carries a v* tag (the release being cut), start from its parent, or the
  # check would compare the release with itself and never fire.
  from=HEAD
  if [[ -n "$(git -C "${root}" tag --points-at HEAD --list 'v*' 2>/dev/null)" ]]; then
    from=HEAD^
  fi
  if git -C "${root}" rev-parse --is-inside-work-tree >/dev/null 2>&1 &&
    tag="$(git -C "${root}" describe --tags --abbrev=0 --match 'v*' "${from}" 2>/dev/null)"; then
    git -C "${root}" show "${tag}:metadata.yaml" >"${work}/old.yaml"
    old="${work}/old.yaml"
    echo "check-metadata: comparing against ${tag}"
  else
    echo "check-metadata: no git repository or release tag, validating ${new} only"
  fi
fi

python3 - "${new}" "${old}" "${crd_kustomization}" <<'PY'
import re, sys, yaml

new_path, old_path, crd_kustomization = sys.argv[1], sys.argv[2], sys.argv[3]

def fail(msg):
    print(f"check-metadata: {msg}", file=sys.stderr)
    sys.exit(1)

def load(path):
    try:
        doc = yaml.safe_load(open(path))
    except (OSError, yaml.YAMLError) as e:
        fail(f"{path}: {e}")
    if not isinstance(doc, dict):
        fail(f"{path}: not a YAML mapping")
    if doc.get("apiVersion") != "clusterctl.cluster.x-k8s.io/v1alpha3":
        fail(f"{path}: apiVersion must be clusterctl.cluster.x-k8s.io/v1alpha3, got {doc.get('apiVersion')!r}")
    if doc.get("kind") != "Metadata":
        fail(f"{path}: kind must be Metadata, got {doc.get('kind')!r}")
    series = doc.get("releaseSeries")
    if not isinstance(series, list) or not series:
        fail(f"{path}: releaseSeries must list at least one series")
    out = {}
    for s in series:
        if not isinstance(s, dict):
            fail(f"{path}: releaseSeries entry {s!r} is not a mapping")
        major, minor, contract = s.get("major"), s.get("minor"), s.get("contract")
        if not (isinstance(major, int) and isinstance(minor, int) and major >= 0 and minor >= 0):
            fail(f"{path}: releaseSeries entry {s!r} needs integer major and minor >= 0")
        if not isinstance(contract, str) or not re.fullmatch(r"v\d+(alpha|beta)?\d*", contract):
            fail(f"{path}: releaseSeries {major}.{minor} has no valid contract ({contract!r})")
        if (major, minor) in out:
            fail(f"{path}: releaseSeries {major}.{minor} is listed twice")
        out[(major, minor)] = contract
    return out

new = load(new_path)

labels = {}
for block in (yaml.safe_load(open(crd_kustomization)) or {}).get("labels", []):
    labels.update(block.get("pairs", {}))
keys = [k.split("/", 1)[1] for k in labels if k.startswith("cluster.x-k8s.io/v")]
if len(keys) != 1:
    fail(f"{crd_kustomization}: want exactly one cluster.x-k8s.io/<contract> label, found {keys}")
latest = max(new)
if new[latest] != keys[0]:
    fail(f"{new_path}: newest series {latest[0]}.{latest[1]} has contract {new[latest]}, CRDs are labelled {keys[0]}")

if old_path:
    old = load(old_path)
    for key, contract in sorted(old.items()):
        if key not in new:
            fail(f"{new_path}: releaseSeries {key[0]}.{key[1]} was removed; it is append-only")
        if new[key] != contract:
            fail(f"{new_path}: releaseSeries {key[0]}.{key[1]} changed contract {contract} -> {new[key]}")

print(f"check-metadata: {new_path} OK ({len(new)} series, newest {latest[0]}.{latest[1]} {new[latest]})")
PY
