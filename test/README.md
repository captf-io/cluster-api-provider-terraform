# CAPTF test environment

`test/` is a separate Go module holding the foundation that CAPTF's
container-based test suites run on: a reproducible management cluster,
brought up with one command.

The environment is a kind cluster on rootless podman (or docker) with:

- cert-manager v1.21.1;
- cluster-api v1.14.2: core, kubeadm bootstrap and kubeadm control plane;
- CAPTF, with a manager image built from your working tree.

The module provides the framework, the lifecycle entry point, the e2e
foundation suite and the noop data-flow suite. See [E2E tiers](#e2e-tiers).

## Quick start

```sh
make testenv-up        # build, create, install, wait (~6 min cold, ~15 s reused)
source bin/testenv/captf-test-dev/env.sh
kubectl get pods -A    # KUBECONFIG now points at the test cluster
make testenv-reload    # after changing the manager: rebuild and roll out
make testenv-down      # delete the cluster; artifacts are kept
```

## Make targets

| Target | What it does |
| --- | --- |
| `testenv-up` | Brings up the whole environment: builds and smoke-checks the manager image, creates the kind cluster, side-loads the manager image, pulls the noop images inside every node, installs the providers, waits for readiness, and writes `state.json` and `env.sh`. |
| `testenv-status` | Shows whether the cluster exists, its nodes, the non-ready provider pods and a summary of `state.json`. |
| `testenv-logs` | Writes a diagnostics bundle into `bin/testenv/<name>/artifacts/<timestamp>/`. |
| `testenv-reload` | Rebuilds the manager image from the tree, side-loads it and rolls the `captf-controller-manager` Deployment over to it. |
| `testenv-down` | Deletes the cluster and removes its work directory, except `artifacts/`. |

Each target runs one test of `test/env/lifecycle`:

```sh
go test -tags=e2e -count=1 -timeout 45m -run '^TestUp$' ./env/lifecycle -v
```

The command runs from `test/`, and the pinned `clusterctl` and `kustomize` are
passed by absolute path. The entry point is a `go test -tags=e2e` process
because that is the process the development host lets drive podman and kind.
Every file of the package carries the `e2e` build tag, so `make test` and a
plain `go vet ./...` never compile it; `make vet` and `make lint` check it in
a separate `-tags e2e` pass. Its `TestMain` refuses to run
unless `-run` selects one operation.

The orchestrator is `test/framework/env`. It builds on `engine`,
`kindcluster`, `images`, `providers`, `wait` and `diag`, each unit-tested with
fakes.

## Variables

| Variable | Default | Meaning |
| --- | --- | --- |
| `TESTENV_NAME` | `captf-test-dev` | The cluster name. It must start with `captf-test-`. |
| `TESTENV_ENGINE` | auto | `podman` or `docker`. The make variable sets `CAPTF_TESTENV_ENGINE`. |
| `TESTENV_WORKERS` | `0` | The number of kind worker nodes, from 0 to 5. |
| `CAPTF_TESTENV_REUSE` | off | Makes `testenv-up` reuse an existing cluster whose `state.json` matches the pins, the engine and the worker count. |
| `TESTENV_ALL` | off | Makes `testenv-down` delete every `captf-test-*` cluster. |

For example: `make testenv-up TESTENV_NAME=captf-test-pool TESTENV_WORKERS=1`.

## What it writes

Everything for cluster `<name>` lives in `bin/testenv/<name>/`, which is
ignored by git:

| Path | Contents |
| --- | --- |
| `kubeconfig` | The cluster's external kubeconfig. |
| `env.sh` | Exports `KUBECONFIG`, plus `TERRAFORM_CLUSTER_IMAGE` and `TERRAFORM_MACHINE_IMAGE` for the cluster templates. Both are set to the Terraform noop images the nodes pulled, as `repository:tag@digest`. |
| `state.json` | The engine, the pins, the manager and noop image references, what every node's containerd holds for each image, the step timings and the kubeconfig path. |
| `captf/`, `repo/`, `home/` | The rendered CAPTF provider, the clusterctl local repository, and clusterctl's isolated `HOME`. |
| `artifacts/<timestamp>/` | Diagnostics bundles. `testenv-down` keeps them. |

Pinned downloads are cached in `~/.cache/captf-testenv/` and checked against
their sha256 pins. Network access is needed only on the first run.

## Safety rules

- **Name prefix.** Cluster names must start with `captf-test-`. Anything
  else is refused, and the guard runs before every create, delete, node and
  log operation. `testenv-down` only ever deletes `captf-test-*` clusters.
- **Protected cluster.** `kube-php-client` is refused by name as well.
- **Dedicated network.** The nodes join the `captf-test` network, never kind's
  default `kind` network. kind creates `captf-test` but never deletes it, so
  `testenv-down` removes it once no `captf-test-*` cluster is left.
- **Explicit kubeconfig.** The orchestrator never reads `~/.kube/config`.
  Every child process gets `KUBECONFIG=bin/testenv/<name>/kubeconfig`, and
  clusterctl also gets an isolated `HOME` and `XDG_CONFIG_HOME`. kind is
  always given the explicit kubeconfig path, including on delete.
- **No host changes.** Host sysctls and limits (inotify included) are never
  changed. If kind reports "too many open files", look for leftover
  `captf-test` clusters or containers first:
  `podman ps -a --filter name=captf-test`.
- **Nothing is deleted implicitly.** `testenv-up` refuses an existing
  cluster unless `CAPTF_TESTENV_REUSE=1` is set, and refuses to reuse one
  whose pins differ. Either way it tells you to run `testenv-down`.

## Pins

Every version, download and image is pinned in
[`framework/versions.go`](framework/versions.go):

- kind v0.33.0 and its node image for Kubernetes v1.36.4, by digest;
- the cluster-api v1.14.2 release assets, by sha256;
- the cert-manager v1.21.1 manifest, by sha256;
- the six published noop module images (`ghcr.io/captf-io/module-images/noop-{cluster,machine,machinepool}`
  for Terraform and OpenTofu), by index digest.

To bump a pin, follow the procedure in the comment at the top of
`versions.go`. Never pin a tag alone. `versions_test.go` checks that every pin
is well formed. After a bump, an existing environment no longer matches its
`state.json`: run `make testenv-down`, then `make testenv-up`.

## The manager image

`testenv-up` and `testenv-reload` build the manager image on every run:

1. They run `make docker-build`, tagged `localhost/captf/manager:<tree-id>`.
   The tree ID is the short HEAD sha, plus `-dirty-<hash>` for uncommitted
   changes. The tag is never `latest`.
2. They smoke-check the image: `/manager --help` and `/runner --help` must
   both exit 0.

The build keeps the Makefile's `VERSION`, which `hack/version.sh` always
makes a valid semantic version (`v0.0.0-dev.g<commit>[.dirty]` before the
first release tag). The manager parses it at startup and once panicked on a
bare commit hash; the smoke check caught that on the first run.

**`DATE`** is pinned to HEAD's commit time, so an unchanged tree rebuilds as
a full layer-cache hit with the same image ID, in about 6 s.

## Inner loop: reload

`make testenv-reload` runs these steps:

1. It rebuilds and smoke-checks the image for the current tree.
2. It side-loads the image into every node.
3. It patches the `captf-controller-manager` Deployment in `captf-system`:
   the container image and `CAPTF_MANAGER_IMAGE`, which sets the runner image,
   and `replicas: 1`. The default install runs two replicas; the suites assert
   one manager pod and its stable identity, so the test environment runs one.
4. It waits for the rollout, then updates `state.json`.

If you run it on an unchanged tree, it rebuilds the same tag with the same
image ID. It finds the Deployment already at that tag, reports "unchanged",
and restarts nothing. To force a restart, change the tree or delete the pod.

## Artifacts

`make testenv-logs` writes a bundle on demand. Any `testenv-up` or
`testenv-reload` failure that happens once the cluster exists also writes one,
and prints its path. A bundle holds:

- the pod logs of the provider namespaces and kube-system;
- the events in those namespaces;
- the CAPI, CAPTF and Job objects;
- the nodes;
- kind's node logs.

Secrets are never collected. The layout is described in
[`framework/diag/doc.go`](framework/diag/doc.go).

## Docker caveat

On the development host, `docker` is the Docker CLI talking to the podman
socket. kind's Docker provider rejects that setup: the podman compat API
reports `CPUShares: false`. Auto-detection tries `podman info` first, so it
picks podman there.

The docker path (`TESTENV_ENGINE=docker`) needs a real Docker daemon, and it
has not been verified yet. A CI job on a Docker runner is the planned proof.

## Measurements (2026-10-04, bertha, rootless podman 5.8.4)

| Run | Wall time |
| --- | --- |
| Cold `testenv-up`, single node | 5 min 38 s |
| `CAPTF_TESTENV_REUSE=1 make testenv-up`, manager current | 16 s |
| `testenv-reload`, tree changed | 54 s, rollout 12 s |
| `testenv-reload`, unchanged tree | 20 s, "unchanged" |
| `testenv-down` | 19 s, then no `captf-test` containers or networks are left |

The cold bring-up breaks down as follows, re-measured on 2026-10-04 with
the noop images pulled inside the nodes. The numbers are the steps
`env.UpCluster` and `env.InstallProviders` logged during
`make e2e-foundation`, which runs the same code as `testenv-up`:

| Step | Time |
| --- | --- |
| Build and smoke-check the manager image | 26 s |
| Create the kind cluster | 49 s |
| Side-load the manager image | 9 s |
| Pull the six noop images inside the node (`crictl pull`) | 36 s |
| `clusterctl init` | 39 s |
| Readiness waits | 0.2 s (clusterctl already waited) |

The old host pull plus side-load of all seven images took 3 min 14 s, since
each noop image is about 330 MiB.

## Finding: side-loaded images lose their repo digests

Side-loading keeps tags and image IDs but drops repo digests. That made it
unusable for the noop module images, so only the manager image is
side-loaded now. The module images are pulled inside every node with
`crictl pull <repository>@<digest>`, so each node holds the registry's real
repo digest and CAPTF's digest pinning works for them. The manager image
is never digest-pinned, so a side-loaded copy with no real repo digest is
fine.

The motivating CAPTF behavior: `jobs.ImageDigest` keeps only the digest of
the pod's status `imageID` and joins it to the repository in the spec. A
pod running a side-loaded image reports the synthetic `import-<date>`
digest (below), so the pinned digest it computes never matches the
published one. A node that pulled the image reports the published digest.

`state.json` records `crictl inspecti` for the manager and noop images on
every node, and `testenv-status` prints the verdict for the noop images
(`kept`, `dropped` or `partial`); it is now `kept` after a normal `up`.
Foundation stage 1 asserts it: every node's repo digests for each noop
image must contain exactly `<repository>@<pinned digest>`.

What a side-load does to an image pulled by digest, saved with `podman save
--format docker-archive` and loaded with `ctr images import --digests`
(measured before the change): the node's containerd keeps:

- **The tag.** `ghcr.io/captf-io/noop-cluster:edge-terraform` is present.
- **The image ID.** It is the config digest, the same ID podman reports.

It loses:

- **The pinned repo digest.** The index digest
  `ghcr.io/captf-io/noop-cluster@sha256:25663d…` is gone, and so is podman's
  manifest digest.
- **The digest's repository.** The only repo digest left is a synthetic
  `docker.io/library/import-<date>@sha256:…`, the digest of the re-serialized
  archive manifest.

Consequences of that, which the in-node pull avoids for module images:

- A pod that references an image by `repository@sha256:<index digest>`
  does not match any local image record, so the node would pull it from the
  registry instead of using the side-loaded copy.
- Anything that reads a digest back from a pod's status `imageID` sees the
  synthetic `import-<date>` digest, not the published one.

## E2E tiers

Tests in this module run in one of two tiers:

| Tier | What | How it runs |
| --- | --- | --- |
| **Unit** (default) | The framework's packages, tested with fakes. Nothing spawns a process or touches podman. | `make test`, `make test-cover` and CI, always. It stays fast, forever. |
| **E2E** (opt-in) | `test/e2e/...` and `test/env/lifecycle`, against a real kind cluster on podman. | Only through their make targets, or `go test -tags=e2e` with `-run`. CI does not run them yet: that needs a real Docker host. |

Three guards keep the tiers apart:

- **The build tag.** Every file under `test/e2e/` and `test/env/lifecycle/`
  starts with `//go:build e2e`, so the default `go test ./...` never compiles
  them. `make verify-test-tiers`, which `make verify` and CI run, fails if one
  lacks the tag, or if any other `.go` file in the repository uses it.
- **`-run`.** Each e2e package's `TestMain` refuses to run unless `-run`
  selects a test.
- **Checked without running.** `make vet` and `make lint` add a `-tags e2e`
  pass over this module, so tagged code cannot rot unnoticed.

### The foundation suite

`make e2e-foundation` runs `TestFoundation` in `test/e2e/foundation`:

```sh
cd test && go test -tags=e2e -count=1 -timeout 60m -run '^TestFoundation$' ./e2e/foundation -v
```

It builds the `captf-test-e2e` cluster through `test/framework/env`, then
proves in six ordered stages that harder tests can rely on it. Each stage is
a subtest, and a failed stage stops the suite.

| Stage | What it proves |
| --- | --- |
| 1 `cluster-build` | `env.UpCluster` builds the manager image, creates the cluster, side-loads the manager image and pulls the noop images inside every node. The cluster exists with 1 + workers nodes, and the explicit kubeconfig reaches an API server at `framework.KubernetesVersion`. Every node container runs the pinned node image (same image ID as on the host) and holds the manager image with the host's image ID. Each node also holds all six noop images, and each one's repo digests contain exactly `<repository>@<pinned digest>`. The manager image is side-loaded and never pinned, so no repo digest is expected of it. |
| 2 `base-components` | The API server's `/readyz` and `/livez` pass. The nodes are Ready with no pressure. The etcd, kube-apiserver, kube-controller-manager and kube-scheduler static pods are Running, and CoreDNS, kindnet, kube-proxy and local-path-provisioner are ready. Every pod in `kube-system` and `local-path-storage` is healthy with 0 restarts, and they hold still for 30 s. |
| 3 `captf-install` | `env.InstallProviders` installs cert-manager, the CAPI core and kubeadm providers and CAPTF, waits for readiness and writes `state.json`. On a reused cluster it only points the manager at the freshly built image. |
| 4 `captf-components` | **cert-manager:** its 3 Deployments are Available with healthy pods, and a self-signed Issuer and Certificate in a throwaway `e2e-<random>` namespace become Ready (then the namespace is deleted). **CAPI:** each provider's Deployment and pods are healthy, its CRDs (found by the `cluster.x-k8s.io/provider` label) are Established, and every admission webhook calling its namespace carries a caBundle and has a ready Service endpoint. **clusterctl inventory:** exactly the Provider objects `capi-system/cluster-api`, `capi-kubeadm-bootstrap-system/bootstrap-kubeadm`, `capi-kubeadm-control-plane-system/control-plane-kubeadm` and `captf-system/infrastructure-terraform`, at the pinned versions. **CAPTF:** one Ready manager pod with 0 restarts runs the image built from your tree (the test environment runs one replica; the default install runs two). The 7 CRDs are Established, and `captf-serving-cert` is Ready. Every webhook in `captf-validating-webhook-configuration` carries exactly the serving Secret's `ca.crt`. The webhook Service has endpoints, and the webhook rejects an invalid object. `:9440/healthz` and `/readyz` answer `ok` through the pod proxy. The manager pod holds the leader Lease, and its log has no E, F or panic line. |
| 5 `together` | Its three parts run in order, and the first failing one stops the rest. **A real reconcile:** a throwaway Secret, then a TerraformClusterIdentity referencing it, which reaches `Ready=True` with reason `SecretFound`. Both are deleted and confirmed gone. This exercises the CRD, the webhook and its SubjectAccessReview, the manager, RBAC and the status subresource. **Stability:** for `CAPTF_E2E_STABILITY`, no pod in any namespace restarts, is recreated, deleted, added or goes not-ready. Every 15 s the manager pod still holds the leader Lease, it renews, and the API server is ready. Afterwards no new Warning event may appear in the provider namespaces. **Logs:** CAPTF's manager must have no E, F or panic line. cert-manager, CAPI and kube-system fail only on panic, `fatal error:` or klog F lines. Their other E lines are listed in the output without failing, except the allowlisted ones below. |
| 6 `green-light` | Writes `bin/testenv/<name>/greenlight.json` (cluster, tree ID, manager image, pins, Kubernetes version, each stage's duration, stability window, timestamp), then runs `greenlight.Require` against it as a later test would. |

**Lifecycle:**

- **Fresh by default.** The suite fails at once if the cluster already
  exists, unless `CAPTF_E2E_REUSE=1`.
- **The green light is removed first.** Each run deletes any previous
  `greenlight.json` before it starts, so a failed run never leaves an old
  green light behind.
- **On failure** it writes a diagnostics bundle into
  `bin/testenv/<name>/artifacts/<timestamp>/`, prints the path and keeps
  the cluster for inspection.
- **On success** it keeps the cluster, green-lit for later tests, unless
  `CAPTF_E2E_TEARDOWN=1`.
- **Cleanup.** `make e2e-down` deletes the cluster and keeps the artifacts.
- **Throwaway objects.** The throwaway namespace, Secret and identity are
  named `e2e-<random>` and always removed in `t.Cleanup`.

Every error says what was expected, what was observed and what to inspect,
usually as a `KUBECONFIG=… kubectl …` command line.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CAPTF_E2E_CLUSTER` | `captf-test-e2e` | The cluster name. It must start with `captf-test-`. |
| `CAPTF_E2E_REUSE` | off | Run against an existing cluster whose `state.json` matches the pins. The manager is rebuilt and rolled out if the tree changed. |
| `CAPTF_E2E_TEARDOWN` | off | Delete the cluster after a passing run (for CI). |
| `CAPTF_E2E_STABILITY` | `2m` | The stage 5 stability window, a Go duration. |
| `CAPTF_E2E_WORKERS` | `0` | The number of kind worker nodes. |
| `CAPTF_E2E_GREENLIGHT_MAX_AGE` | `24h` | How old a green light may be before `greenlight.Require` rejects it. |
| `TESTENV_ENGINE` | auto | As for the `testenv-*` targets. |

`TESTENV_NAME` and `CAPTF_TESTENV_REUSE` never steer the suite, so the
development cluster `captf-test-dev` is left alone.

### The noop data-flow suite

Run `make e2e-foundation` first: the suite needs the green-lit cluster.
Then `make e2e-noop` runs `TestNoop` in `test/e2e/noop`:

```sh
cd test && go test -tags=e2e -count=1 -timeout 45m -run '^TestNoop$' ./e2e/noop -v
```

`TestNoop` drives the published noop modules through real Cluster API
objects and proves, with no cloud, that data flows end to end:

- the CAPI spec into the module inputs;
- the module outputs into CAPTF status, and on into CAPI;
- the cluster's exports into the machine and pool inputs;
- the pinned digests into every later Job;
- deletion into a full cleanup.

`TestNoop` fails at once, and never skips, unless `greenlight.Require`
passes. Each run uses a fresh namespace `e2e-noop-<6 hex>` and its own
cluster-scoped TerraformClusterIdentity of the same name. Object names
are short (`c1`, `c2`, `ma`, `mb`, `mp`, `boot`), since Job names are
hashed past 57 characters.

**Runtimes and images.** The cluster and machine A run Terraform; machine
B and the pool run OpenTofu. Each `spec.source.image` is
`<repository>:<framework.NoopVersion>-<runtime>@<pinned digest>`, so the expected
`status.source.imageDigest` and the durable Secret's
`captf.io/image-digest` are exactly `framework.NoopImage.Pinned()`
(`<repository>@<digest>`). Every Terraform* object sets
`spec.jobs.activeDeadlineSeconds: 600`.

| Stage | What it proves (exact values) |
| --- | --- |
| 1 `setup` | Records the manager pod (UID, restarts). Creates the namespace, the identity's Secret in `captf-system`, and the identity, with `allowedNamespaces.list` naming only the run's namespace; the identity reaches `Ready=True` (`SecretFound`). Creates the bootstrap Secret `boot` (`value` = a fixed cloud-config text), then starts tracking every CAPTF Job pod in the namespace. |
| 2 `cluster` | Cluster `c1` (no control plane) and TerraformCluster `c1` (`drift.intervalSeconds: 60`, action `Report`). **Job:** an apply Job completes and runs the spec's image. **TerraformCluster:** `Ready`, `ApplyJobSucceeded` (`ApplySucceeded`), `OutputsValid` and `InfrastructureHealthy` all True. `spec.controlPlaneEndpoint` = `{host: noop-c1.invalid, port: 6443}`. `status.failureDomains` = `[{name: fd1, controlPlane: true}]`. `status.initialization.provisioned` = true. `status.source.image` = the spec's image, `imageDigest` = the pin, `runtimeVersion` = `1.16.4`. `status.stateSecretSuffix` = `tfstate.SuffixFor`. **Events:** JobCreated, JobSucceeded, DigestPinned, StateBackedUp, Provisioned, ControlPlaneEndpointSet, FailureDomainsChanged. **CAPI Cluster:** `status.initialization.infrastructureProvisioned` = true, `spec.controlPlaneEndpoint` and `status.failureDomains` copied, and the `InfrastructureReady` condition True. **Inputs** (`captf-inputs-c-c1`): `captf.io/image-digest` = the pin; `captf_cluster` = `{name: c1, namespace: <ns>}`; `cluster_network` = `{pods: [10.244.0.0/16], services: [10.96.0.0/12], api_server_port: 6443, service_domain: null}`; `control_plane_initialized` = false. **State:** output `control_plane_endpoint` as above and `exports.backend_id` = `noop-backend-<uuid>`, plus a `captf-state-backup-<suffix>-<serial>` Secret. **Access:** the mirror `captf-creds-<identity>` and the `captf-runner` ServiceAccount and RoleBinding exist in the namespace. |
| 3 `machines` | Machine `ma` (failure domain `fd1`, Terraform) and `mb` (OpenTofu), both bootstrapped from `boot`. For each: the apply Job completes on the spec's image. `spec.providerID` = `noop:///<ns>/<machine>`. `status.addresses` = `[{type: InternalIP, address: 10.0.0.1}]`. Provisioned and `status.interruptible` = false; `ma`'s `status.failureDomain` = `fd1`. `imageDigest` = each machine's own pin, `runtimeVersion` = `1.16.4` and `1.12.6`. Events JobCreated, JobSucceeded, DigestPinned, Provisioned and ProviderIDSet. **CAPI Machine:** `spec.providerID` and `status.addresses` copied, `status.initialization.infrastructureProvisioned` = true, `InfrastructureReady` True. **Export flow** (`captf-inputs-m-<machine>`): `captf_cluster_outputs.backend_id` = the cluster state's `exports.backend_id`; `machine_name` = the Machine's name; `bootstrap_data` = base64 of `boot`'s value; `control_plane` = false; `failure_domain` = `fd1` or null. |
| 4 `pool` | MachinePool `mp` (3 replicas) and TerraformMachinePool `mp` (OpenTofu). The apply completes. `spec.providerIDList` = `noop:///<ns>/mp/{0,1,2}` (sorted), `status.replicas` = 3, `status.instances` = those IDs with `state: running`, `status.ready` = true, provisioned; `spec.providerID` = `noop-group:///<ns>/mp`; `runtimeVersion` = `1.12.6`. Inputs: `replicas` = 3 and the cluster's `backend_id`. CAPI MachinePool: `infrastructureProvisioned` = true. **Scale:** patching the MachinePool to 2 runs a new apply Job; then 2 IDs and 2 replicas. **The pin is pullable:** the next membership refresh Job runs `<repository>@<digest>` and completes. |
| 5 `drift` | **Drift:** the next drift Job of `c1` runs the pinned reference and completes; then `DriftJobSucceeded=True` (`DriftChecked`), `DriftDetected=False` (`NoDrift`), and `lastDriftCheck` at or after that Job's creation. **Failure:** Cluster and TerraformCluster `c2` with `spec.variables: {e2e_unknown: x}`, which the module does not declare: `ApplyJobSucceeded=False` (`ApplyFailed`), `lastRun` = operation apply, error kind `step`, step `init`, summary containing `Extraneous JSON object property`; not provisioned; a Warning JobFailed Event. **Recovery:** removing `spec.variables` runs a new apply Job that completes; `ApplyJobSucceeded=True` names it, `c2` is provisioned with endpoint `noop-c2.invalid:6443`. |
| 6 `teardown` | **Machines:** deleting both Machines destroys both TerraformMachines: each destroy pod ran the machine's pin, a JobSucceeded Event reads `destroy Job <that Job> succeeded …`, and the state and `captf-inputs-m-*` Secrets are gone. **Blocked cluster:** TerraformCluster `c1`, deleted while the pool carries its cluster-name label, reports `DeletionBlocked=True` (`DependentsExist`) and runs no destroy Job. **Pool:** deleting the MachinePool destroys `mp` the same way; then `c1`'s destroy runs and it goes, and Cluster `c1` is deleted. Deleting Cluster `c2` destroys its TerraformCluster. **Sweep:** no `tfstate=true` Secret, no `captf-*` Secret (inputs, run, plan key, backup, mirror), no Lease and no `captf-runner` ServiceAccount or RoleBinding remain. **Identity:** with no users left, the identity can be deleted and goes. |
| 7 `health` | The manager pod is the one stage 1 saw, with no new restart. The manager log since the suite started has no panic, `fatal error:` or klog E/F line. One line is excused: client-go's events broadcaster logging `Server rejected event (will not retry!)` with `namespaces "e2e-noop-…" not found` for an earlier run's namespace. The broadcaster flushes an Event series minutes after its last Event, after that run's cleanup deleted the namespace; the run's own namespace is never excused. Every CAPTF Job pod the tracker saw ended Succeeded, or was last seen running and its Job has a JobSucceeded Event. The exception is `c2`'s apply pods from before its variable was removed. |

**Failure and cleanup.** A failed stage stops the suite. It runs
`env.Collect`, adds the run namespace's pods, logs and events under
`noop-namespace/` in the same bundle, and prints the path. `t.Cleanup`
then deletes what remains, in dependency order: Machines and
MachinePools, then Clusters, then any Terraform* object. Then it deletes
the namespace, unless CAPTF objects are stuck in it: a terminating
namespace refuses the Jobs and Secrets their destroy needs, so the
namespace is kept and the message says what to delete. Last come the
identity and its Secret. The cluster is never changed otherwise:
`greenlight.json` is not rewritten, and the cluster stays green-lit.

| Variable | Default | Meaning |
| --- | --- | --- |
| `CAPTF_E2E_CLUSTER` | `captf-test-e2e` | The green-lit cluster. |
| `CAPTF_E2E_GREENLIGHT_MAX_AGE` | `24h` | As for `greenlight.Require`. |
| `CAPTF_E2E_NOOP_BAD_DIGEST` | off | The negative check only: machine B's image gets the digest `sha256:` plus 64 zeros, so stage 3 must fail on its image pull. |

**Plan versus reality.** These held on the live cluster
(Cluster API v1.14.2), and the suite asserts what is true:

- `status.source.runtimeVersion` is the bare version
  (`terraform version -json`): `1.16.4` for Terraform and `1.12.6` for
  OpenTofu, the versions the pinned images carry. The version is what
  tells the runtimes apart.
- No refresh follows a machine's or pool's apply: the apply's own healthy
  outputs advance `lastRefresh`, so `RefreshAfterApply` has nothing to do.
  The pinned reference is proven by the pool's membership refresh (every
  60 s plus jitter; the first one 60 to 120 s after the apply), the
  cluster's drift Jobs, and every destroy Job.
- Cluster API copies a MachinePool's `spec.providerIDList` and
  `status.replicas` only after it reaches the workload cluster through its
  ClusterCache, and `noop-c1.invalid` never resolves, so the MachinePool
  only reads `infrastructureProvisioned`. A Machine's `spec.providerID`
  and `status.addresses` are copied before that point, so both are
  asserted.
- On the CAPI Cluster, the paths are
  `status.initialization.infrastructureProvisioned`,
  `spec.controlPlaneEndpoint` and the list `status.failureDomains`. Cluster
  and Machine both carry an `InfrastructureReady` condition. A
  TerraformMachinePool has `status.ready`.
- An undeclared module argument fails at `init` with Terraform's JSON-syntax
  diagnostic `Extraneous JSON object property`, not `Unsupported argument`,
  because CAPTF renders the root module as `main.tf.json`. The recovery
  apply Job started about a minute after the variable was removed.
- Deleting a Cluster never reaches a blocked TerraformCluster: Cluster API
  deletes the Cluster's MachinePools and Machines before its
  infrastructure. Stage 6 therefore deletes TerraformCluster `c1` directly
  to see `DeletionBlocked`.
- A destroy Job and its pod are deleted with their object about 8 s after
  they start, sometimes before a poll sees the pod Succeeded. Polling for
  the Job missed it outright when both machines were destroyed at once. So
  stage 6 reads the pod tracker (polled every second) for the image, and
  the JobSucceeded Event for the outcome.
- A Job pod is deleted with its object, so stage 7 reads the tracker's
  record of every pod rather than the pods still present. It scans the
  manager log from the suite's start, so a deliberate failure in an
  earlier run cannot fail a later one.

**Timings** (2026-10-04, bertha, two consecutive green runs on the
green-lit single-node cluster):

| Stage | Run 1 | Run 2 | What takes the time |
| --- | --- | --- | --- |
| 1 `setup` | 2 s | 2 s | The identity reaching Ready. |
| 2 `cluster` | 14 s | 14 s | One apply Job (about 7 s). |
| 3 `machines` | 16 s | 16 s | Two apply Jobs in parallel. |
| 4 `pool` | 1 min 31 s | 1 min 29 s | Two applies, then waiting for the first membership refresh (60 s plus jitter). |
| 5 `drift` | 1 min 53 s | 1 min 59 s | The next drift Job (60 s interval plus jitter), the failed apply, and the recovery apply about a minute after the fix. |
| 6 `teardown` | 38 s | 46 s | Five destroy Jobs, partly serial (machines, pool, then the cluster). |
| 7 `health` | 0.1 s | 0.1 s | |
| **Total** | **4 min 35 s** | **4 min 47 s** | `TestNoop` took 275 s and 287 s, cleanup included. |

**Negative check.** `make e2e-noop CAPTF_E2E_NOOP_BAD_DIGEST=1` points
machine B at a digest no registry holds. Stage 3 fails once the apply
pod has reported the pull failure for 45 s, well inside the 600 s
deadline that would otherwise end the Job. Measured on 2026-10-04,
stage 3 failed after 61 s and 71 s in two runs, with:

```text
TerraformMachine mb provisioned: expected apply Job pod captf-m-mb-apply-a1-…
to run, observed container "source" unable to pull image
ghcr.io/captf-io/module-images/noop-machine:v0.1.0-opentofu@sha256:000…000 for 46s:
ErrImagePull: rpc error: code = NotFound desc = failed to pull and unpack
image "ghcr.io/captf-io/module-images/noop-machine@sha256:000…000": … not found
(the Job's 600s deadline would end it as ImagePullFailed)
```

The run then writes the diagnostics bundle and cleans up. CAPTF deletes
a TerraformMachine only after its running apply Job ends, and a Job
stuck pulling ends only at its deadline. So the cleanup waits up to the
deadline plus a minute for each group. The negative run's cleanup took
9 min, leaving the namespace and identity gone (`TestNoop` 638 s in
all). At the deadline CAPTF reported the Job as `JobDeadlineExceeded`,
not `ImagePullFailed`. The likely cause is that the pod whose waiting
reason would say so is already being removed with the Job. This is a
product follow-up, not yet investigated.

### The green-light contract for later tests

A harder e2e test must not assume the cluster is sound: it requires the
green light. `greenlight.Require` fails the test (it never skips) unless
`greenlight.json` exists, its cluster still exists, its pins equal this
build's, its manager image equals the one in `state.json`, every stage
passed, and it is younger than `CAPTF_E2E_GREENLIGHT_MAX_AGE`. The failure
says to run `make e2e-foundation`.

```go
//go:build e2e

package harder

func TestSomethingHard(t *testing.T) {
	ctx := context.Background()
	cfg, err := env.ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	cfg.Name = "captf-test-e2e" // the green-lit cluster, not TESTENV_NAME's
	eng, err := engine.Detect(ctx, cfg.EngineOverride, engine.ExecRunner())
	if err != nil {
		t.Fatal(err)
	}
	kind, err := kindcluster.New(eng)
	if err != nil {
		t.Fatal(err)
	}
	check, err := greenlight.CheckFromState(cfg.WorkDir(), kind.Exists)
	if err != nil {
		t.Fatal(err)
	}
	greenlight.Require(t, greenlight.Path(cfg.WorkDir()), check)
	// The cluster is green-lit: use cfg.Kubeconfig().
}
```

Call it at the top of each test, or once in `TestMain` with a
`testing.TB` of your own.

### Allowlists

The allowlists live in `test/e2e/foundation/allowlists_test.go`, each entry
with its reason. Keep them short: an entry needs evidence that the line is
expected on a healthy cluster.

E-level lines that cert-manager, CAPI and kube-system print on a healthy
cluster. They never fail the suite, which fails those components only on
panic and fatal lines. The allowlist only keeps them out of the
non-failing report:

| Pattern | Component | Why it is benign |
| --- | --- | --- |
| `unable to fetch associated secret` | cert-manager cainjector | At install, a Certificate's Secret is not issued yet. cainjector retries and injects the CA once it exists. |
| `re-queuing item due to error processing` | cert-manager controller | A transient conflict while the first Certificates are issued. The item is retried and becomes Ready. |
| `unable to fetch certificate that owns the secret` | cert-manager cainjector | Deleting stage 4's throwaway namespace removes its Certificate before its Secret, and the indexer logs the orphan once. |
| `nodePortAddresses is unset` | kube-proxy | kind leaves it unset. The line is advice. |
| `The manifest file is empty, ignoring` | kube-apiserver, kube-controller-manager, kube-scheduler | At startup, kubeadm passes an empty optional manifest file. |
| `system:kube-(scheduler\|controller-manager)… cannot (get\|list\|watch)` | kube-scheduler, kube-controller-manager | In their first seconds, their informers start before the API server has created the bootstrap RBAC policy. They retry. |
| `is not as new as written version` | kube-controller-manager | Its informer cache briefly lags a write it made. The sync is retried. |

Warning events in the provider namespaces: none are allowlisted. A healthy
cluster emits none there once the providers are installed.

CAPTF's manager log has no allowlist. A healthy manager logs no E line.

### Measurements (2026-10-04, bertha, rootless podman 5.8.4)

| Run | Wall time |
| --- | --- |
| `make e2e-foundation` from scratch, noop images pulled in the nodes (re-measured 2026-10-04) | suite 5 min 36 s |
| `CAPTF_E2E_REUSE=1`, manager rebuilt and rolled out | 3 min 36 s (suite 3 min 29 s: cluster-build 23 s, captf-install 12 s) |
| Negative check: manager pod deleted 10 s into a 3 min window | stage 5 failed after 10 s; diagnostics collected, cluster kept |

The from-scratch run breaks down as follows:

| Stage | Time |
| --- | --- |
| 1 `cluster-build` (the in-node noop pulls take 36 s of it; the old host pull and side-load took 4 min 19 s in all) | 2 min 4 s |
| 2 `base-components` (the 30 s stability window) | 30 s |
| 3 `captf-install` | 39 s |
| 4 `captf-components` | 12 s |
| 5 `together` (the 2 min stability window) | 2 min 10 s |
| 6 `green-light` | 0.2 s |
