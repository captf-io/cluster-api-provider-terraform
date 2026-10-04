#!/usr/bin/env bash
# Verifies the contract JSON Schemas in internal/contract/schemas:
#  1. every schema is a valid draft 2020-12 schema;
#  2. every internal/contract/testdata/examples/<schema>.valid.json
#     validates and every <schema>.invalid.json fails with at least the
#     expected keywords;
#  3. internal/contract's golden inputs and internal/outputs' golden decoded
#     outputs validate against the role schemas.
# Needs python3 with jsonschema >= 4.18 (referencing).
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

python3 - "${ROOT}" <<'PY'
import json
import pathlib
import sys

from jsonschema import Draft202012Validator
from referencing import Registry, Resource

repo = pathlib.Path(sys.argv[1])
schemas_dir = repo / "internal" / "contract" / "schemas"
examples_dir = repo / "internal" / "contract" / "testdata" / "examples"
failures = []

# Expected validation keywords for each invalid example (all must fire).
# "type" is deliberately absent: every non-null value of a nullable anyOf
# property reports a "type" error from its null branch, so it would pass
# whether or not the example's real type violation is present.
EXPECT = {
    "cluster-inputs": {"additionalProperties", "const", "required", "maximum"},
    "cluster-outputs": {"minimum", "minLength", "enum", "required"},
    "machine-inputs": {"required", "pattern", "enum"},
    "machine-outputs": {"maxLength", "enum", "required"},
    "machinepool-inputs": {"required", "pattern", "enum"},
    "machinepool-outputs": {"maxLength", "enum", "required"},
}

docs = {p.name: json.loads(p.read_text()) for p in sorted(schemas_dir.glob("*.json"))}
registry = Registry().with_resources(
    (d["$id"], Resource.from_contents(d)) for d in docs.values()
)

# 1. Metaschema.
for name, d in docs.items():
    try:
        Draft202012Validator.check_schema(d)
    except Exception as e:  # noqa: BLE001 - report every schema error
        failures.append(f"{name}: not a valid 2020-12 schema: {e}")


def keywords(errors):
    out = set()
    for e in errors:
        out.add(e.validator)
        out |= keywords(e.context)
    return out


# 2. Examples.
for role, want in EXPECT.items():
    v = Draft202012Validator(docs[f"{role}.json"], registry=registry)
    valid = json.loads((examples_dir / f"{role}.valid.json").read_text())
    errs = sorted(v.iter_errors(valid), key=lambda e: list(e.path))
    for e in errs:
        failures.append(f"{role}.valid.json: {list(e.path)}: {e.message}")
    invalid = json.loads((examples_dir / f"{role}.invalid.json").read_text())
    got = keywords(v.iter_errors(invalid))
    if not got:
        failures.append(f"{role}.invalid.json: validated, expected failure")
    elif not want <= got:
        failures.append(f"{role}.invalid.json: expected keywords {sorted(want)}, got {sorted(got)}")
    else:
        print(f"verify-schemas: {role}: valid ok; invalid fails with {sorted(want)}")

# 3. The Go goldens: rendered inputs (internal/contract) and decoded,
# normalized outputs (internal/outputs) validate against the role schemas.
for pkg, kind in (("contract", "inputs"), ("outputs", "outputs")):
    goldens = sorted((repo / "internal" / pkg / "testdata").glob(f"*-{kind}*.golden.json"))
    if not goldens:
        failures.append(f"no internal/{pkg}/testdata/*-{kind}*.golden.json to validate")
    for g in goldens:
        role = g.name.split("-", 1)[0]
        v = Draft202012Validator(docs[f"{role}-{kind}.json"], registry=registry)
        errs = sorted(v.iter_errors(json.loads(g.read_text())), key=lambda e: list(e.path))
        for e in errs:
            failures.append(f"internal/{pkg}/testdata/{g.name}: {list(e.path)}: {e.message}")
        if not errs:
            print(f"verify-schemas: internal/{pkg}/testdata/{g.name}: valid against {role}-{kind}.json")

if failures:
    print("\n".join("verify-schemas: FAIL " + f for f in failures), file=sys.stderr)
    sys.exit(1)
print("verify-schemas: OK")
PY
