# Cluster API Provider Terraform

<div align="center">

**Run Cluster API on any infrastructure you can build with Terraform or OpenTofu.**

CAPTF is a [Cluster API](https://cluster-api.sigs.k8s.io/) infrastructure provider
that uses Terraform/OpenTofu modules as its infrastructure: one for the cluster,
and one for each machine or machine pool. Each module ships as an OCI image and
runs as a Kubernetes Job, so CAPTF works with any platform a Terraform provider
covers, without a Go SDK integration.

[![Status: pre-alpha](https://img.shields.io/badge/status-pre--alpha-orange)](#status)
[![Cluster API contract: v1beta2](https://img.shields.io/badge/Cluster_API-v1beta2-326CE5?logo=kubernetes&logoColor=white)](https://cluster-api.sigs.k8s.io/)
[![Runtime: Terraform or OpenTofu](https://img.shields.io/badge/runtime-Terraform_%7C_OpenTofu-7B42BC?logo=terraform&logoColor=white)](https://captf.io/docs/module-author/image-contract.html)
[![Go 1.26](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: Apache 2.0](https://img.shields.io/badge/license-Apache_2.0-blue)](LICENSE.md)

[![ci](https://github.com/captf-io/cluster-api-provider-terraform/actions/workflows/ci.yaml/badge.svg?branch=main)](https://github.com/captf-io/cluster-api-provider-terraform/actions/workflows/ci.yaml)
[![security](https://github.com/captf-io/cluster-api-provider-terraform/actions/workflows/security.yaml/badge.svg?branch=main)](https://github.com/captf-io/cluster-api-provider-terraform/actions/workflows/security.yaml)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/captf-io/cluster-api-provider-terraform/badge)](https://scorecard.dev/viewer/?uri=github.com/captf-io/cluster-api-provider-terraform)

</div>

## Status

Pre-alpha: nothing has been released yet, and the `infrastructure.cluster.x-k8s.io/v1alpha1`
API and module contract will change. Unit tests cover the controllers, runner,
webhooks and linter; there is no end-to-end run against a live management
cluster yet. See [Project status](https://captf.io/docs/introduction.html#project-status)
for what's implemented.

## Try it

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
[captf-io/docs](https://github.com/captf-io/docs)) covers everything below in
depth:

| Section | Covers |
| --- | --- |
| [Getting started](https://captf.io/docs/getting-started/quick-start.html) | Install, identities, first cluster |
| [Concepts](https://captf.io/docs/concepts/architecture.html) | Architecture, kinds, the reconcile lifecycle, state, drift, security model |
| [User guide](https://captf.io/docs/user-guide/identities.html) | Identities, module variables, drift, plan approval, remediation, Job tuning |
| [Operator guide](https://captf.io/docs/operator-guide/installation.html) | Installation, configuration, RBAC, secrets, observability, runbooks |
| [Module author](https://captf.io/docs/module-author/contract/README.html) | The module and image contracts, control-plane integration, `tfcapi-lint` |
| [Reference](https://captf.io/docs/reference/api.html) | API types, metrics, events, conditions, manager flags |

## Development

```sh
make help    # list every target
make verify  # generated code, doc links, templates, schemas and more
make test    # unit tests in every Go module, with -race
make testenv-up  # a kind management cluster with CAPTF built from the tree (test/README.md)
make e2e-foundation  # opt-in e2e: build and green-light the captf-test-e2e cluster (test/README.md, "E2E tiers")
```

See [Contributing](https://captf.io/docs/developer-guide/contributing.html) for
the rest of the workflow.

## Security

A module image runs as a Job with the identity's cloud credentials and can
read every Secret in its namespace; referencing an image grants its publisher
that access. See [the security model](https://captf.io/docs/concepts/security-model.html)
before deciding who may set `spec.source.image`. Report vulnerabilities
privately through
[GitHub security advisories](https://github.com/captf-io/cluster-api-provider-terraform/security/advisories/new);
maintainers are listed in [`SECURITY_CONTACTS`](SECURITY_CONTACTS).

## License

Licensed under the [Apache License 2.0](LICENSE.md).
