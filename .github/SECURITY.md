# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately through
[GitHub security advisories](https://github.com/captf-io/cluster-api-provider-terraform/security/advisories/new),
not in a public issue or pull request. The maintainers listed in
[`SECURITY_CONTACTS`](../SECURITY_CONTACTS) are notified and will
acknowledge the report.

Include what you found, how to reproduce it, the CAPTF version or commit,
and the impact you expect. Credit is given in the advisory unless you ask
otherwise.

## Supported versions

CAPTF is pre-alpha and has no releases yet. Fixes land on `main`; once
releases start, the latest minor release receives security fixes.

## Scope

CAPTF runs Terraform or OpenTofu module images as Kubernetes Jobs with an
identity's cloud credentials. A module image's publisher is trusted with
those credentials by design; see
[the security model](https://captf.io/docs/concepts/security-model.html)
for what is and is not a vulnerability in CAPTF itself.
