# Security Policy

## Reporting a vulnerability

Report vulnerabilities privately through GitHub's private vulnerability
reporting. **Do not open a public issue, pull request or discussion.**

1. Open the **Security** tab of the affected repository, for example
   [cluster-api-provider-terraform](https://github.com/captf-io/cluster-api-provider-terraform/security)
   or [opentofu-base](https://github.com/captf-io/opentofu-base/security).
   If you are not sure which repository is affected, use
   [captf-io/.github](https://github.com/captf-io/.github/security).
2. Select **Report a vulnerability** and fill in the form.

Only the maintainers can see the report. Include:

- The affected component (manager, runner, `tfcapi-lint`, a base image, a
  reference module or a template) and the version or image digest.
- What an attacker needs in order to exploit it: for example, `create` on a
  `Terraform*` kind in a namespace, control of a module image, or network
  access to the webhook.
- Steps to reproduce, and what they gain.

Never include real credentials, state Secrets or runner environment dumps.
Redact them, or reproduce the issue with throwaway ones.

## What to expect

CAPTF is maintained by a small team with no guaranteed response time. We aim
to acknowledge a report within seven days, agree on a fix and a disclosure
date with you, and publish a GitHub security advisory crediting you (unless
you prefer not to be named) once a fixed release is out.

## Scope

CAPTF runs whatever module image a `Terraform*` object names, with that
namespace's runner access and the resolved identity's cloud credentials.
That is by design: the image is the trust boundary, as the
[Security Model](https://captf.io/docs/concepts/security-model.html)
explains. A module doing what its code says is not a CAPTF vulnerability.
Ways to exceed what that page says an object grants are, for example:

- Reading Secrets or credentials outside the namespace or identity the page
  allows.
- Leaking credentials or state into status, events or logs.
- Bypassing a webhook or owner-reference check.

Vulnerabilities in Terraform, OpenTofu, providers or the Ubuntu base belong
upstream. Report them to us only if CAPTF's use of them makes things worse.

## Supported versions

CAPTF is pre-1.0. Fixes land on `main` and in the next release; there are no
backports to earlier releases. The base images are rebuilt weekly, so use a
current tag.
