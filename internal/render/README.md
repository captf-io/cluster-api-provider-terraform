# internal/render

Renders the root module every Job runs against: `main.tf.json` and
`terraform.tfvars.json`, plus the `provider_installation` CLI config for
images with a provider mirror.

## Golden files

`testdata/{cluster,machine,machinepool}/` hold the rendered files for fixed
inputs, and `testdata/machine-variables/` the machine files with user
variables (one from a Secret, so declared `sensitive`). The variables golden
plans cleanly on OpenTofu against the machine stub plus typed declarations
of its five variables; string values from a ConfigMap (`"40"`) convert to
the module's `number`, and an undeclared variable fails with `Extraneous
JSON object property`.
`go test ./internal/render` compares against them. To refresh them after a
deliberate change, run the following and review the diff:

```sh
UPDATE_SNAPSHOTS=1 go test ./internal/render
```

## Validating the goldens with real binaries

This check is not part of `go test`: it needs the pinned runtimes, run here
as their container images. It lays out each golden as a Job does. The root
is at `work/root` and the stub module at `module`, so `../../module`
resolves. Both are mounted at `/captf`.

```sh
W=$(mktemp -d)
for rt in terraform tofu; do for role in cluster machine; do
  d="$W/$rt-$role"; mkdir -p "$d/work/root" "$d/module"
  cp internal/render/testdata/$role/*.json "$d/work/root/"
  cp internal/render/testdata/stubmodule/$role/main.tf "$d/module/"
done; done
```

Then, for each `<rt>-<role>`, run `init` and `validate` as two separate
`podman run` calls. The OpenTofu `-minimal` image has no shell:

```sh
podman run --rm --userns=keep-id -v "$W/terraform-machine:/captf:Z" -w /captf/work/root \
  docker.io/hashicorp/terraform:1.16.4 init -backend=false -input=false -no-color
podman run --rm --userns=keep-id -v "$W/terraform-machine:/captf:Z" -w /captf/work/root \
  docker.io/hashicorp/terraform:1.16.4 validate -no-color
podman run --rm --userns=keep-id -v "$W/tofu-machine:/captf:Z" -w /captf/work/root \
  ghcr.io/opentofu/opentofu:1.12.6-minimal init -backend=false -input=false -no-color
podman run --rm --userns=keep-id -v "$W/tofu-machine:/captf:Z" -w /captf/work/root \
  ghcr.io/opentofu/opentofu:1.12.6-minimal validate -no-color
```

`validate` does not read variable values. To check that the rendered
tfvars fit the stub module's precise types, run `plan` on a copy without
the backend block, since a partial kubernetes backend cannot plan offline:

```sh
jq 'del(.terraform)' "$W/terraform-machine/work/root/main.tf.json" > main.tf.json   # in a copy of the dir
podman run … init -input=false -no-color
podman run … plan -input=false -no-color -var-file=terraform.tfvars.json
```

Both roles pass `validate` and `plan` on Terraform 1.16.4 and OpenTofu
1.12.6. A stub missing an output passes `init` and fails `validate` with
`Unsupported attribute`. Re-run this check after changing the rendering
logic or bumping the pinned runtime versions.
