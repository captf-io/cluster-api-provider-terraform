<!-- captf:header -->
<h1 align="center">
  <a href="https://captf.io/docs/"><img
    src="https://raw.githubusercontent.com/captf-io/.github/refs/heads/main/readme/banners/cluster-api-provider-terraform.svg"
    width="100%"
    alt="cluster-api-provider-terraform: Run Terraform and OpenTofu modules as
    Cluster API providers"></a>
</h1>
<p align="center">
  <a href="https://github.com/captf-io/cluster-api-provider-terraform/actions/workflows/ci.yaml"><img
    src="https://img.shields.io/github/actions/workflow/status/captf-io/cluster-api-provider-terraform/ci.yaml?branch=main&amp;label=build&amp;labelColor=161B3A&amp;style=flat-square"
    alt="build"></a>
  <a href="https://captf.io/docs/module-author/contract/index.html"><img
    src="https://img.shields.io/static/v1?label=contract&amp;message=v1alpha1&amp;color=A974FF&amp;labelColor=161B3A&amp;style=flat-square"
    alt="contract v1alpha1"></a>
  <a href="https://captf.io/docs/"><img
    src="https://img.shields.io/static/v1?label=docs&amp;message=captf.io&amp;color=5B8CFF&amp;labelColor=161B3A&amp;style=flat-square"
    alt="docs captf.io"></a>
  <a href="LICENSE.md"><img
    src="https://img.shields.io/static/v1?label=license&amp;message=Apache-2.0&amp;color=FFD84D&amp;labelColor=161B3A&amp;style=flat-square"
    alt="license Apache-2.0"></a>
</p>
<!-- /captf:header -->

<!-- captf:status -->
> [!NOTE]
> **Pre-release.** CAPTF is `v1alpha1`: its API and its
> [module contract](https://captf.io/docs/module-author/contract/index.html)
> may still change before the first release.
<!-- /captf:status -->

CAPTF is a [Cluster API](https://cluster-api.sigs.k8s.io/) infrastructure
provider that uses Terraform/OpenTofu modules as its infrastructure: one for
the cluster, and one for each machine or machine pool. Each module ships as an
OCI image and runs as a Kubernetes Job, so CAPTF works with any platform a
Terraform provider covers, without a Go SDK integration. This repository holds
the provider itself: the manager, the in-Job runner and the `tfcapi-lint`
linter. It is pre-alpha and nothing has been released yet, so the
`infrastructure.cluster.x-k8s.io/v1alpha1` API and the module contract will
change. Unit tests cover the controllers, runner, webhooks and linter; there is
no end-to-end run against a live management cluster yet. See
[Project status](https://captf.io/docs/introduction.html#project-status) for
what is implemented.

## Using it

```sh
CLUSTER_TOPOLOGY=true clusterctl init --config clusterctl.yaml --infrastructure terraform
clusterctl generate yaml --from templates/identity.yaml | kubectl apply -f -
tfcapi-lint module . --role machine --strict
clusterctl generate cluster my-cluster --infrastructure terraform --target-namespace team-a | kubectl apply -f -
```

The [quick start](https://captf.io/docs/getting-started/quick-start.html)
walks through each step, including building and pushing module images.

## Images

The manager image (manager and runner) is published to
`ghcr.io/captf-io/cluster-api-provider-terraform` for linux/amd64 and
linux/arm64 by [publish.yaml](.github/workflows/publish.yaml): `:edge` and
`:sha-<commit>` on every push to `main`, and `:vX.Y.Z` on every release tag,
whose GitHub Release carries the clusterctl assets and the `tfcapi-lint`
binaries. Each image is signed with keyless cosign and carries SLSA build
provenance and an SPDX SBOM attestation; each release asset carries
provenance:

```sh
cosign verify ghcr.io/captf-io/cluster-api-provider-terraform:edge \
  --certificate-identity-regexp '^https://github.com/captf-io/cluster-api-provider-terraform/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
gh attestation verify oci://ghcr.io/captf-io/cluster-api-provider-terraform:edge \
  -R captf-io/cluster-api-provider-terraform
gh attestation verify infrastructure-components.yaml -R captf-io/cluster-api-provider-terraform
```

## Documentation

The book at [captf.io/docs](https://captf.io/docs/) (source:
[`docs/docs/`](https://github.com/captf-io/captf-io.github.io/tree/main/docs/docs)
in captf-io/captf-io.github.io) covers everything below in depth:

| Section | Covers |
| --- | --- |
| [Getting started](https://captf.io/docs/getting-started/quick-start.html) | Install, identities, first cluster |
| [Concepts](https://captf.io/docs/concepts/architecture.html) | Architecture, kinds, the reconcile lifecycle, state, drift, security model |
| [User guide](https://captf.io/docs/user-guide/identities.html) | Identities, module variables, drift, plan approval, remediation, Job tuning |
| [Operator guide](https://captf.io/docs/operator-guide/installation.html) | Installation, configuration, RBAC, secrets, observability, runbooks |
| [Module author](https://captf.io/docs/module-author/contract/README.html) | The module and image contracts, control-plane integration, `tfcapi-lint` |
| [Reference](https://captf.io/docs/reference/api.html) | API types, metrics, events, conditions, manager flags |

## Developing

```sh
make help    # list every target
make verify  # generated code, doc links, templates, schemas and more
make test    # unit tests in every Go module, with -race
make testenv-up  # a kind management cluster with CAPTF built from the tree (test/README.md)
make e2e-foundation  # opt-in e2e: build and green-light the captf-test-e2e cluster (test/README.md, "E2E tiers")
```

See [`CONTRIBUTING.md`](CONTRIBUTING.md) and the
[contributing guide](https://captf.io/docs/developer-guide/contributing.html)
for the rest of the workflow.

## Releasing

A release needs a clean tree with `HEAD` tagged `vX.Y.Z` (or `vX.Y.Z-rc.N`).
Then:

```sh
make release VERSION=vX.Y.Z
```

This pushes the manager image and builds every release asset into
`out/release`. The [releasing guide](https://captf.io/docs/developer-guide/releasing.html)
has the checklist.

## Security

A module image runs as a Job with the identity's cloud credentials and can
read every Secret in its namespace; referencing an image grants its publisher
that access. See [the security model](https://captf.io/docs/concepts/security-model.html)
before deciding who may set `spec.source.image`. Report vulnerabilities
privately through
[GitHub security advisories](https://github.com/captf-io/cluster-api-provider-terraform/security/advisories/new);
see [`SECURITY.md`](SECURITY.md) for the policy. Maintainers are listed in
[`SECURITY_CONTACTS`](SECURITY_CONTACTS).

<!-- captf:footer -->
<br>
<p align="center">
  <img
    src="https://raw.githubusercontent.com/captf-io/.github/refs/heads/main/readme/assets/divider.svg"
    width="100%" height="4" alt="">
</p>
<p align="center">
  <a href="https://captf.io/"><img
    src="https://raw.githubusercontent.com/captf-io/.github/refs/heads/main/readme/assets/mark.svg"
    width="40" height="40" alt="CAPTF"></a>
  <br>
  <a href="https://captf.io/docs/"
    ><b>Documentation</b></a> ·
  <a href="https://captf.io/docs/getting-started/quick-start.html"
    ><b>Quick start</b></a> ·
  <a href="https://github.com/captf-io/.github/blob/main/CONTRIBUTING.md"
    ><b>Contributing</b></a> ·
  <a href="https://github.com/captf-io/.github/blob/main/SECURITY.md"
    ><b>Security</b></a>
  <br>
  <sub>Built for
    <a href="https://cluster-api.sigs.k8s.io/">Cluster API</a>.
    <a href="LICENSE.md">Apache 2.0</a>.</sub>
</p>
<!-- /captf:footer -->
