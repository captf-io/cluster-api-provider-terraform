# Contributing to CAPTF

Thanks for helping. Every `captf-io` repository carries this same file.

## Before you start

- **Questions and bugs:** check the [docs](https://captf.io/docs/) first,
  then open an issue using the matching form.
- **Vulnerabilities:** never in a public issue. See
  [SECURITY.md](SECURITY.md).
- **Larger changes:** open an issue describing the change before writing
  it, especially anything that touches the API, the
  [module contract](https://captf.io/docs/module-author/contract/README.html) or the
  security model. CAPTF is `v1alpha1`, but a contract change still breaks
  every module written against it.

## Making a change

The [Developer Guide](https://captf.io/docs/developer-guide/contributing.html)
covers the provider repository: its layout, prerequisites, the
build/lint/test/verify loop and the conventions the checks enforce. Each
other repository documents its own checks in its `README.md`.

In short:

1. Fork the repository and branch from `main`.
2. Make one logical change per commit, with tests where the repository
   has them.
3. Run the repository's checks:

   | Repository | Checks |
   | --- | --- |
   | `cluster-api-provider-terraform` | `make lint test verify` |
   | `*-modules` (cloud) | `make verify`, then `make test` |
   | `noop-modules`, `*-base` | `make test` |
   | `captf-io.github.io` | `make gen && make build` |

4. Open a pull request against `main` and fill in the template.

## Commit messages

- The subject is imperative, at most about 50 characters, in
  `<subsystem>: <summary>` form, such as `runner: name the exit codes`.
  Check `git log` for the subsystem names already in use.
- A blank line follows, then a body wrapped at about 72 columns that
  explains why the change is needed, not only what it does.
- Keep refactors and features in separate commits.

## Licensing

Every `captf-io` repository is licensed under Apache-2.0. By opening a pull
request you agree that your contribution is licensed under the same terms.

Every source file starts with the Apache-2.0 license header, with the
copyright held by The CAPTF Authors. `make check-headers` checks it, and CI
runs the same check; `make fix-headers` adds the header to new files.
`.licenserc.yaml` lists the files that don't need one.

## Conduct

Everyone taking part is expected to follow the
[Code of Conduct](CODE_OF_CONDUCT.md).
