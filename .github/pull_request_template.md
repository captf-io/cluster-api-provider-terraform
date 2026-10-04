## What this changes

<!-- What was wrong or missing, and why this is the fix. Link the issue. -->

## How it was tested

<!-- Unit tests added or changed; anything checked by hand. -->

## Checklist

- [ ] `make test` and `make lint` pass; new code keeps its package at or
      above its coverage floor (`make test-cover cover-check`)
- [ ] `make verify` passes (generated files, godoc, manifests)
- [ ] Each commit is one logical change, with a `<subsystem>: <summary>`
      subject of 50 characters or fewer
- [ ] User-visible behavior, conditions, events, flags or the module
      contract changed: the book in
      [captf-io/docs](https://github.com/captf-io/docs) is updated by
      hand
- [ ] API change: CRDs regenerated (`make manifests`) and the change is
      backward compatible, or the PR says why not
