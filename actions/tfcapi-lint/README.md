# tfcapi-lint GitHub Action

Lints a CAPTF module directory, or a module image, against the
[CAPTF module contract](https://captf.io/docs/module-author/contract/README.html)
by running the `tfcapi-lint` container image,
`ghcr.io/captf-io/tfcapi-lint`, with `docker`. It needs a Linux runner with
docker, as GitHub's `ubuntu-*` runners have.

```yaml
- uses: actions/checkout@<commit> # v7
  with:
    persist-credentials: false
- uses: captf-io/cluster-api-provider-terraform/actions/tfcapi-lint@<commit> # vX.Y.Z
  with:
    command: module
    target: .
    role: machine
    strict: true
```

Pin the commit of a provider release. The linter image follows the
action's ref:

| Pinned to | Image |
| --- | --- |
| `vX.Y.Z` | `ghcr.io/captf-io/tfcapi-lint:vX.Y.Z` |
| a commit on `main` | `ghcr.io/captf-io/tfcapi-lint:sha-<7>` |
| `main` | `ghcr.io/captf-io/tfcapi-lint:edge` |

The `version` input picks a tag, and `image` names any image (one pinned
by digest, or one built by an earlier step). The step fails with the
linter's exit code: `1` for findings (warnings too, under `strict`), `2`
when the module or image cannot be read, `3` for a usage error.

| Input | Default | Meaning |
| --- | --- | --- |
| `command` | (required) | `module` or `image` |
| `target` | (required) | The module directory, or the image reference or `oci:<dir>` |
| `role` | (required) | `cluster`, `machine` or `machinepool` |
| `strict` | `false` | Fail on warnings too |
| `allow-warnings` | | Check IDs whose warnings become info, separated by spaces or newlines |
| `contract` | | Contract version |
| `platform` | `linux/amd64` | `image` only: the platform to check |
| `all-platforms` | `false` | `image` only: check every platform |
| `insecure` | `false` | `image` only: allow a plain-HTTP registry |
| `json` | `false` | Print the JSON report |
| `args` | | Extra `tfcapi-lint` flags |
| `version` | (from the ref) | The image tag |
| `image` | | The full image; overrides `version` |

Output: `image`, the image that ran.

## Documentation

- [tfcapi-lint in CI](https://captf.io/docs/module-author/tfcapi-lint-ci.html):
  complete workflows, image linting, Dependabot, private registries,
  self-hosted runners, other CI systems and troubleshooting.
- [tfcapi-lint](https://captf.io/docs/module-author/tfcapi-lint.html):
  installing the linter and what it checks.
- [tfcapi-lint CLI](https://captf.io/docs/reference/tfcapi-lint-cli.html):
  every flag, check and exit code.

## Files

| File | Purpose |
| --- | --- |
| `action.yml` | The composite action: its inputs, passed to the script as environment variables. |
| `tfcapi-lint.sh` | Picks the image, builds the arguments and runs the container. |
| `tfcapi-lint_test.sh` | Tests the image choice for every input and ref (`make verify-action`). |
| `image_test.sh` | Lints a known-good and a known-bad module of every role with the image (`make test-lint-image`). |
