# cluster-api-provider-terraform (CAPTF)
#
# Every tool is installed at a pinned version into hack/tools/bin as
# <name>-<version> plus an unversioned symlink, so bumping a version
# re-downloads.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

ROOT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST))))
GO_MODULES := . api test

## --------------------------------------
## Versions
## --------------------------------------

GO_VERSION ?= 1.26
CONTROLLER_GEN_VER := v0.21.0
KUSTOMIZE_VER := v5.7.0
GOLANGCI_LINT_VER := v2.13.1
# kube-api-linter publishes no tags; this is the newest pseudo-version on
# proxy.golang.org at the time of pinning.
KUBE_API_LINTER_VER := v0.0.0-20260716143926-092fe0c72997
CLUSTERCTL_VER := v1.14.2
# promtool only: the latest Prometheus release on 2026-09-26.
PROMTOOL_VER := v3.15.0
# Highest release whose go directive builds on Go 1.26.x (v2.18.x needs 1.27).
GORELEASER_VER := v2.17.1
# golang.org/x/tools as in the CAPI v1.14.2 module graph.
GOIMPORTS_VER := v0.48.0
# gotestsum wraps `go test` for JUnit output and CI-friendly formatting.
GOTESTSUM_VER := v1.13.0

IMG ?= ghcr.io/captf-io/cluster-api-provider-terraform:dev
CONTAINER_TOOL ?= podman

MODULE := github.com/captf-io/cluster-api-provider-terraform
# Always a valid semantic version (hack/version.sh): the binaries parse it at
# startup, and a bare commit hash from an untagged clone made them panic.
VERSION ?= $(shell hack/version.sh 2>/dev/null || echo v0.0.0-dev)
COMMIT ?= $(shell git rev-parse HEAD 2>/dev/null || echo unknown)
TREE_STATE ?= $(shell git rev-parse --git-dir >/dev/null 2>&1 && { git diff --quiet HEAD 2>/dev/null && echo clean || echo dirty; })
DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Version stamping for every binary, into k8s.io/component-base/version as
# Kubernetes does (read by --version and the captf_build_info metric);
# passed to the image build as a build arg. .goreleaser.yaml mirrors the -X
# list.
VERSION_PKG := k8s.io/component-base/version
LDFLAGS := -s -w \
	-X $(VERSION_PKG).gitVersion=$(VERSION) \
	-X $(VERSION_PKG).gitCommit=$(COMMIT) \
	-X $(VERSION_PKG).gitTreeState=$(TREE_STATE) \
	-X $(VERSION_PKG).buildDate=$(DATE)

## --------------------------------------
## Tool binaries
## --------------------------------------

TOOLS_DIR := $(ROOT_DIR)/hack/tools
TOOLS_BIN := $(TOOLS_DIR)/bin

CONTROLLER_GEN := $(TOOLS_BIN)/controller-gen-$(CONTROLLER_GEN_VER)
KUSTOMIZE := $(TOOLS_BIN)/kustomize-$(KUSTOMIZE_VER)
GOLANGCI_LINT := $(TOOLS_BIN)/golangci-lint-$(GOLANGCI_LINT_VER)
GOLANGCI_LINT_KAL := $(TOOLS_BIN)/golangci-lint-kube-api-linter-$(GOLANGCI_LINT_VER)-$(KUBE_API_LINTER_VER)
CLUSTERCTL := $(TOOLS_BIN)/clusterctl-$(CLUSTERCTL_VER)
PROMTOOL := $(TOOLS_BIN)/promtool-$(PROMTOOL_VER)
GORELEASER := $(TOOLS_BIN)/goreleaser-$(GORELEASER_VER)
GOIMPORTS := $(TOOLS_BIN)/goimports-$(GOIMPORTS_VER)
GOTESTSUM := $(TOOLS_BIN)/gotestsum-$(GOTESTSUM_VER)

TOOLS := $(CONTROLLER_GEN) $(KUSTOMIZE) $(GOLANGCI_LINT) \
	$(GOLANGCI_LINT_KAL) $(CLUSTERCTL) $(GORELEASER) $(GOIMPORTS) $(PROMTOOL) \
	$(GOTESTSUM)

# clusterctl cannot be `go install`ed (CAPI's go.mod carries replace
# directives), so the release binary is downloaded and checked against the
# sha256 GitHub reports for each v1.14.2 asset.
HOST_OS := $(shell go env GOOS)
HOST_ARCH := $(shell go env GOARCH)
CLUSTERCTL_SHA256_linux_amd64 := 01122674fd3c47a33206ab1b8b81d437afbcf5dd25d126535564f24a2cdf676e
CLUSTERCTL_SHA256_linux_arm64 := 83976008aa9ddb81dab01443c646aaa125e4993e17bf24e790e29779f712d79d
CLUSTERCTL_SHA256_darwin_amd64 := 07a8c84719e1c9f8a1f4e9c6f398423ac47de8fb0284ac2145acc134e6ffc1bd
CLUSTERCTL_SHA256_darwin_arm64 := ea2285445da861b2ec96e948563cf158f4e0fd89a36238267b62e43a1bb00da8
# promtool comes out of the Prometheus release tarball, checked against the
# release's sha256sums.txt.
PROMTOOL_SHA256_linux_amd64 := 2a542df32eac02ee17b9d844fb2aa1de00dafa5476579ba8a3ba862e9d572ea0
PROMTOOL_SHA256_linux_arm64 := f1f90ec08e849d494ca66c611470afc50192f0355f1a61c33f2cbde02d067823
PROMTOOL_SHA256_darwin_amd64 := 2d79e744c2d7e505db936fbc898e05abc74fcb6e437c25befd26e9c9f00aa58b
PROMTOOL_SHA256_darwin_arm64 := 920df4d17e78b3b0175af144eb318b0c74d1cf7b1d1251b326966f0e81977260

# go-install-tool: $(1) versioned target path, $(2) package, $(3) version,
# $(4) binary name produced by `go install`. GOTOOLCHAIN=local makes a tool
# that needs a newer Go fail instead of silently downloading a toolchain.
define go-install-tool
	@mkdir -p "$(TOOLS_BIN)"
	@echo "Installing $(2)@$(3)"
	@rm -f "$(TOOLS_BIN)/$(4)"
	@GOBIN="$(TOOLS_BIN)" GOTOOLCHAIN=local GOWORK=off go install "$(2)@$(3)"
	@mv "$(TOOLS_BIN)/$(4)" "$(1)"
	@ln -sfn "$(notdir $(1))" "$(TOOLS_BIN)/$(4)"
endef

# Go sources outside the tool directory; lint/vet/test/generate skip cleanly
# on an empty tree.
GO_FILES = $(shell find . -path ./hack/tools -prune -o -path ./.claude -prune -o -name '*.go' -type f -print)
API_GO_FILES = $(shell find ./api -name '*.go' -type f -print 2>/dev/null)
# controller-gen roots for manifests: only directories that hold Go files.
MANIFEST_PATHS = $(strip $(foreach d,api internal,$(if $(shell find ./$(d) -name '*.go' -type f -print -quit 2>/dev/null),paths=./$(d)/...)))
BINARIES = $(patsubst cmd/%/main.go,%,$(wildcard cmd/*/main.go))

# for-each-module: run $(1) in every Go module that has at least one package.
# A module without packages is skipped: golangci-lint exits 5 ("no go files
# to analyze") and `go test` has nothing to do there. A `go list` failure
# (broken go.mod, bad import) fails the target instead of skipping.
define for-each-module
for m in $(GO_MODULES); do \
	if ! pkgs="$$(cd "$$m" && go list -f '{{.ImportPath}}' ./... 2>&1)"; then \
		echo "$@: go list failed in module $$m:" >&2; echo "$$pkgs" >&2; exit 1; fi; \
	if [[ -z "$$(grep -v -e '^go: warning: ' <<<"$$pkgs")" ]]; then \
		echo "$@: module $$m has no Go packages, skipping"; continue; fi; \
	echo "$@: $$m"; \
	(cd "$$m" && $(1)); \
done
endef

##@ General

.PHONY: help
help: ## Display this help.
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} /^[a-zA-Z_0-9-]+:.*?##/ { printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2 } /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) } ' $(MAKEFILE_LIST)

# Print a variable's value, e.g. `make -s print-LDFLAGS`: publish.yaml reads
# GO_VERSION and LDFLAGS this way, so the published image is stamped exactly
# as `make docker-build` stamps it. $(info) prints the value verbatim, with
# no shell quoting in the way.
print-%:
	@:$(info $($*))

##@ Development

.PHONY: generate
generate: $(CONTROLLER_GEN) ## Generate deepcopy code (controller-gen object).
	@if [[ -z "$(API_GO_FILES)" ]]; then echo "generate: no Go files under api/, nothing to do"; exit 0; fi; \
	cd api && "$(CONTROLLER_GEN)" object:headerFile="$(ROOT_DIR)/hack/boilerplate.go.txt" paths=./...

.PHONY: manifests
manifests: $(CONTROLLER_GEN) ## Generate CRD, RBAC and webhook manifests into config/.
	@if [[ -z "$(MANIFEST_PATHS)" ]]; then echo "manifests: no Go files under api/ or internal/, nothing to do"; exit 0; fi; \
	"$(CONTROLLER_GEN)" rbac:roleName=manager-role crd webhook \
		$(MANIFEST_PATHS) \
		output:crd:artifacts:config=config/crd/bases \
		output:rbac:artifacts:config=config/rbac \
		output:webhook:artifacts:config=config/webhook


.PHONY: fmt
fmt: $(GOIMPORTS) ## Format Go code (gofmt -s, goimports -local).
	@if [[ -z "$(GO_FILES)" ]]; then echo "fmt: no Go files"; exit 0; fi; \
	gofmt -s -w $(GO_FILES); \
	"$(GOIMPORTS)" -local github.com/captf-io/cluster-api-provider-terraform -w $(GO_FILES)

.PHONY: vet
vet: ## Run go vet in every module, then on the test module's e2e-tagged code.
	@$(call for-each-module,go vet ./...)
	@echo "vet: test (-tags e2e)"
	@cd test && go vet -tags e2e ./...

.PHONY: lint
lint: $(GOLANGCI_LINT) $(GOLANGCI_LINT_KAL) ## Run golangci-lint (.golangci.yml) in every module and on the test module's e2e-tagged code, then kube-api-linter on api/.
	@$(call for-each-module,"$(GOLANGCI_LINT)" run -c "$(ROOT_DIR)/.golangci.yml" $(GOLANGCI_LINT_EXTRA_ARGS) ./...)
	@echo "lint: test (--build-tags e2e)"
	@cd test && "$(GOLANGCI_LINT)" run -c "$(ROOT_DIR)/.golangci.yml" --build-tags e2e $(GOLANGCI_LINT_EXTRA_ARGS) ./...
	@$(MAKE) --no-print-directory lint-api

.PHONY: lint-api
lint-api: $(GOLANGCI_LINT_KAL) ## Run kube-api-linter (.golangci-kal.yml) on the api/ module.
	@if ! pkgs="$$(cd api && go list -f '{{.ImportPath}}' ./... 2>&1)"; then \
		echo "$@: go list failed in module api:" >&2; echo "$$pkgs" >&2; exit 1; fi; \
	if [[ -z "$$(grep -v -e '^go: warning: ' <<<"$$pkgs")" ]]; then \
		echo "$@: module api has no Go packages, skipping"; exit 0; fi; \
	cd api && "$(GOLANGCI_LINT_KAL)" run -c "$(ROOT_DIR)/.golangci-kal.yml" $(GOLANGCI_LINT_EXTRA_ARGS) ./...

.PHONY: lint-fix
lint-fix: ## Run golangci-lint and kube-api-linter with auto-fixers.
	GOLANGCI_LINT_EXTRA_ARGS=--fix $(MAKE) lint

##@ Test

.PHONY: test
test: ## Run unit tests in every module (-race -count=1).
	@$(call for-each-module,go test -race -count=1 ./...)

# GOTESTSUM_FORMAT is the gotestsum output format; CI sets github-actions.
GOTESTSUM_FORMAT ?= pkgname

.PHONY: test-cover
test-cover: $(GOTESTSUM) ## Run unit tests with -race and coverage in every module (profiles and JUnit into bin/).
	@mkdir -p "$(ROOT_DIR)/bin"
	@$(call for-each-module,name="$$([[ "$$m" == . ]] && echo root || echo "$$m")"; "$(GOTESTSUM)" --format $(GOTESTSUM_FORMAT) --junitfile "$(ROOT_DIR)/bin/junit-$$name.xml" -- -race -count=1 -covermode=atomic -coverprofile="$(ROOT_DIR)/bin/cover-$$name.out" ./...)

.PHONY: cover-check
cover-check: ## Check per-package coverage against hack/coverage-floors.txt (after test-cover).
	@go run ./hack/covercheck -floors hack/coverage-floors.txt $(if $(GITHUB_STEP_SUMMARY),-summary "$$GITHUB_STEP_SUMMARY") bin/cover-*.out

##@ Test environment

# The testenv-* targets run one operation of test/framework/env through the
# e2e-tagged test/env/lifecycle package (test/README.md). Empty values take
# the orchestrator's defaults: cluster captf-test-dev, engine auto-detected
# (podman preferred), no workers. TESTENV_ENGINE sets CAPTF_TESTENV_ENGINE.
# CAPTF_TESTENV_REUSE=1 makes testenv-up reuse a matching cluster, and
# TESTENV_ALL=1 makes testenv-down delete every captf-test-* cluster.
TESTENV_NAME ?=
TESTENV_ENGINE ?=
TESTENV_WORKERS ?=
CAPTF_TESTENV_REUSE ?=
TESTENV_ALL ?=

# testenv-run: run the lifecycle test $(1), with the pinned clusterctl and
# kustomize passed by absolute path.
define testenv-run
cd test && TESTENV_NAME="$(TESTENV_NAME)" CAPTF_TESTENV_ENGINE="$(TESTENV_ENGINE)" \
	TESTENV_WORKERS="$(TESTENV_WORKERS)" CAPTF_TESTENV_REUSE="$(CAPTF_TESTENV_REUSE)" \
	TESTENV_ALL="$(TESTENV_ALL)" CLUSTERCTL="$(CLUSTERCTL)" KUSTOMIZE="$(KUSTOMIZE)" \
	go test -tags=e2e -count=1 -timeout 45m -run '^$(1)$$' ./env/lifecycle -v
endef

.PHONY: testenv-up
testenv-up: $(CLUSTERCTL) $(KUSTOMIZE) ## Build the manager image and bring up the kind test environment (bin/testenv/<name>/).
	$(call testenv-run,TestUp)

.PHONY: testenv-down
testenv-down: $(CLUSTERCTL) $(KUSTOMIZE) ## Delete the test environment's cluster (TESTENV_ALL=1: every captf-test-* cluster); artifacts are kept.
	$(call testenv-run,TestDown)

.PHONY: testenv-status
testenv-status: $(CLUSTERCTL) $(KUSTOMIZE) ## Show the test environment's cluster, nodes, non-ready provider pods and state.json.
	$(call testenv-run,TestStatus)

.PHONY: testenv-logs
testenv-logs: $(CLUSTERCTL) $(KUSTOMIZE) ## Collect diagnostics into bin/testenv/<name>/artifacts/<timestamp>/.
	$(call testenv-run,TestCollect)

.PHONY: testenv-reload
testenv-reload: $(CLUSTERCTL) $(KUSTOMIZE) ## Rebuild the manager image from the tree and roll it out into the test environment.
	$(call testenv-run,TestReload)

##@ End-to-end

# The e2e-* targets run the e2e-tagged suites under test/e2e/ (test/README.md,
# "E2E tiers"); make test never compiles them. Empty values take the suite's
# defaults: cluster captf-test-e2e, a fresh cluster (CAPTF_E2E_REUSE=1 reuses
# an existing one), the cluster kept after a passing run
# (CAPTF_E2E_TEARDOWN=1 deletes it), a 2m stability window
# (CAPTF_E2E_STABILITY) and no workers (CAPTF_E2E_WORKERS).
# CAPTF_E2E_GREENLIGHT_MAX_AGE bounds the green light's age (default 24h).
# TESTENV_ENGINE sets CAPTF_TESTENV_ENGINE, as for the testenv-* targets.
CAPTF_E2E_CLUSTER ?=
CAPTF_E2E_REUSE ?=
CAPTF_E2E_TEARDOWN ?=
CAPTF_E2E_STABILITY ?=
CAPTF_E2E_WORKERS ?=
CAPTF_E2E_GREENLIGHT_MAX_AGE ?=
# CAPTF_E2E_NOOP_BAD_DIGEST=1 is e2e-noop's negative check: machine B gets a
# nonexistent digest, so stage 3 must fail on the image pull.
CAPTF_E2E_NOOP_BAD_DIGEST ?=

.PHONY: e2e-foundation
e2e-foundation: $(CLUSTERCTL) $(KUSTOMIZE) ## Run the e2e foundation suite: build, check and green-light the captf-test-e2e cluster.
	cd test && CAPTF_E2E_CLUSTER="$(CAPTF_E2E_CLUSTER)" CAPTF_E2E_REUSE="$(CAPTF_E2E_REUSE)" \
		CAPTF_E2E_TEARDOWN="$(CAPTF_E2E_TEARDOWN)" CAPTF_E2E_STABILITY="$(CAPTF_E2E_STABILITY)" \
		CAPTF_E2E_WORKERS="$(CAPTF_E2E_WORKERS)" CAPTF_E2E_GREENLIGHT_MAX_AGE="$(CAPTF_E2E_GREENLIGHT_MAX_AGE)" \
		CAPTF_TESTENV_ENGINE="$(TESTENV_ENGINE)" CLUSTERCTL="$(CLUSTERCTL)" KUSTOMIZE="$(KUSTOMIZE)" \
		go test -tags=e2e -count=1 -timeout 60m -run '^TestFoundation$$' ./e2e/foundation -v

.PHONY: e2e-noop
e2e-noop: $(CLUSTERCTL) $(KUSTOMIZE) ## Run the noop data-flow suite on the green-lit e2e cluster (run e2e-foundation first).
	cd test && CAPTF_E2E_CLUSTER="$(CAPTF_E2E_CLUSTER)" CAPTF_E2E_GREENLIGHT_MAX_AGE="$(CAPTF_E2E_GREENLIGHT_MAX_AGE)" \
		CAPTF_E2E_NOOP_BAD_DIGEST="$(CAPTF_E2E_NOOP_BAD_DIGEST)" \
		CAPTF_TESTENV_ENGINE="$(TESTENV_ENGINE)" CLUSTERCTL="$(CLUSTERCTL)" KUSTOMIZE="$(KUSTOMIZE)" \
		go test -tags=e2e -count=1 -timeout 45m -run '^TestNoop$$' ./e2e/noop -v

.PHONY: e2e-down
e2e-down: ## Delete the e2e cluster (captf-test-e2e, or CAPTF_E2E_CLUSTER); artifacts are kept.
	@$(MAKE) --no-print-directory testenv-down TESTENV_NAME="$(or $(CAPTF_E2E_CLUSTER),captf-test-e2e)"

##@ Build

.PHONY: build
build: ## Build every cmd/* binary into bin/.
	@if [[ -z "$(BINARIES)" ]]; then echo "build: no cmd/*/main.go yet"; exit 0; fi; \
	for b in $(BINARIES); do \
		echo "go build ./cmd/$$b"; \
		CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o "bin/$$b" "./cmd/$$b"; \
	done

# The docker CLI talking to the podman socket also works:
# `make docker-build CONTAINER_TOOL=docker`. docker-buildx is podman-only.
PLATFORMS ?= linux/amd64,linux/arm64

.PHONY: manager
manager: ## Build the static manager into bin/manager.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/manager ./cmd/manager
	hack/verify-static.sh bin/manager

# `make run` starts bin/manager against the current kubeconfig, outside the
# cluster. Leader election is off: out of cluster controller-runtime cannot
# find its namespace. The webhook server needs a serving certificate before
# /readyz passes, so a self-signed one is generated into WEBHOOK_CERT_DIR
# when missing. The runner image defaults to $(IMG); extra flags go in ARGS.
WEBHOOK_CERT_DIR ?= $(CURDIR)/bin/dev-webhook-certs
RUNNER_IMAGE ?= $(IMG)

.PHONY: run
run: manager ## Run the manager out of cluster against the current kubeconfig (dev).
	hack/dev-webhook-certs.sh "$(WEBHOOK_CERT_DIR)"
	bin/manager --leader-elect=false --webhook-cert-dir="$(WEBHOOK_CERT_DIR)" \
		--runner-image="$(RUNNER_IMAGE)" $(ARGS)

.PHONY: runner
runner: ## Build the static Job runner into bin/runner and check it is static.
	CGO_ENABLED=0 go build -trimpath -ldflags "$(LDFLAGS)" -o bin/runner ./cmd/runner
	hack/verify-static.sh bin/runner

.PHONY: docker-build
docker-build: ## Build the manager/runner image $(IMG) for the host platform.
	hack/ensure-podman.sh "$(CONTAINER_TOOL)"
	$(CONTAINER_TOOL) build --build-arg GO_VERSION="$(GO_VERSION)" --build-arg LDFLAGS="$(LDFLAGS)" \
		-t "$(IMG)" -f Dockerfile .

.PHONY: docker-buildx
docker-buildx: ## Build a multi-arch manifest list $(IMG) for $(PLATFORMS) (podman).
	hack/ensure-podman.sh podman
	if podman manifest exists "$(IMG)"; then podman manifest rm "$(IMG)"; fi
	podman build --platform "$(PLATFORMS)" --manifest "$(IMG)" \
		--build-arg GO_VERSION="$(GO_VERSION)" --build-arg LDFLAGS="$(LDFLAGS)" -f Dockerfile .

.PHONY: docker-push
docker-push: ## Push $(IMG); a manifest list from docker-buildx is pushed with all its images.
	hack/ensure-podman.sh "$(CONTAINER_TOOL)"
	if [[ "$(CONTAINER_TOOL)" == podman ]] && podman manifest exists "$(IMG)"; then \
		podman manifest push --all "$(IMG)" "docker://$(IMG)"; \
	else \
		$(CONTAINER_TOOL) push "$(IMG)"; \
	fi

.PHONY: release-lint-snapshot
release-lint-snapshot: $(GORELEASER) ## Build tfcapi-lint release assets and checksums into dist/ (goreleaser snapshot).
	"$(GORELEASER)" release --snapshot --clean

.PHONY: release-lint-binaries
release-lint-binaries: release-lint-snapshot ## Alias of release-lint-snapshot.

.PHONY: release-lint
release-lint: $(GORELEASER) ## Build tfcapi-lint release assets from the current git tag.
	"$(GORELEASER)" release --clean

# Release manifests: the clusterctl components file with the
# release image, and metadata.yaml, into out/. kustomize edits a temp copy of
# config/, so the tree keeps the :dev image. RELEASE_IMG defaults to the tag;
# `make release` passes the pushed image by digest (RELEASE_REPO@sha256:…),
# so the published components never follow a moved tag. The runner image
# (CAPTF_MANAGER_IMAGE) is copied from it and is pinned the same way.
RELEASE_DIR ?= $(ROOT_DIR)/out
RELEASE_REPO ?= ghcr.io/captf-io/cluster-api-provider-terraform
RELEASE_IMG ?= $(RELEASE_REPO):$(VERSION)
SKOPEO ?= skopeo

.PHONY: manifests-release
manifests-release: $(KUSTOMIZE) ## Build out/infrastructure-components.yaml (RELEASE_IMG); copy metadata.yaml and templates/*.yaml.
	@work="$$(mktemp -d)"; trap 'rm -rf -- "$$work"' EXIT; \
	cp -R config "$$work/config"; \
	(cd "$$work/config/default" && "$(KUSTOMIZE)" edit set image "controller=$(RELEASE_IMG)"); \
	mkdir -p "$(RELEASE_DIR)"; \
	"$(KUSTOMIZE)" build "$$work/config/default" >"$(RELEASE_DIR)/infrastructure-components.yaml"; \
	cp metadata.yaml "$(RELEASE_DIR)/metadata.yaml"; \
	cp templates/*.yaml "$(RELEASE_DIR)/"; \
	echo "$@: $(RELEASE_DIR)/infrastructure-components.yaml ($(RELEASE_IMG))"

# Release (https://captf.io/docs/developer-guide/releasing.html). VERSION must be the tag on a
# clean HEAD; nothing is ever force-pushed and tags never move: a bad rc gets
# a new rc. The assets land in out/release/.
RELEASE_ASSETS := $(RELEASE_DIR)/release

.PHONY: release-preflight
release-preflight: ## Check the tree is clean, HEAD carries tag $(VERSION), and metadata.yaml is append-only.
	@git rev-parse --is-inside-work-tree >/dev/null 2>&1 || { echo "$@: not a git repository" >&2; exit 1; }
	@[[ -z "$$(git status --porcelain)" ]] || { echo "$@: the working tree is not clean" >&2; exit 1; }
	@[[ "$(VERSION)" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$$ ]] || { echo "$@: VERSION $(VERSION) is not vX.Y.Z or vX.Y.Z-rc.N" >&2; exit 1; }
	@git tag --points-at HEAD | grep -qxF -- "$(VERSION)" || { echo "$@: HEAD is not tagged $(VERSION)" >&2; exit 1; }
	@hack/check-metadata.sh

.PHONY: release
release: release-preflight ## Build and push the manager image, and build every asset into out/release (VERSION=vX.Y.Z).
	$(MAKE) docker-build docker-push IMG=$(RELEASE_REPO):$(VERSION)
	@digest="$$($(MAKE) --no-print-directory -s release-image-digest VERSION=$(VERSION))" || exit 1; \
	$(MAKE) release-assets VERSION=$(VERSION) RELEASE_IMG="$(RELEASE_REPO)@$$digest"

# The registry digest of the pushed manager image (the manifest-list digest
# for a multi-arch push); a local image ID would not match what clusters pull.
.PHONY: release-image-digest
release-image-digest: ## Print the registry digest of $(RELEASE_REPO):$(VERSION).
	@digest="$$("$(SKOPEO)" inspect --format '{{.Digest}}' "docker://$(RELEASE_REPO):$(VERSION)")"; \
	[[ "$$digest" =~ ^sha256:[0-9a-f]{64}$$ ]] || { echo "$@: no digest for $(RELEASE_REPO):$(VERSION): '$$digest'" >&2; exit 1; }; \
	echo "$$digest"

# The clusterctl assets (components, metadata, templates) and the tfcapi-lint
# binaries and checksums. GoReleaser keeps the binaries under build-dir
# names; dist/artifacts.json maps each to its release name.
.PHONY: release-assets
release-assets: $(GORELEASER) ## Build every release asset for VERSION into out/release (needs the git tag).
	@rm -rf -- "$(RELEASE_ASSETS)"
	$(MAKE) manifests-release RELEASE_DIR="$(RELEASE_ASSETS)" VERSION=$(VERSION)
	"$(GORELEASER)" release --clean
	@python3 -c 'import json, shutil, sys; [shutil.copy(a["path"], sys.argv[1] + "/" + a["name"]) for a in json.load(open("dist/artifacts.json")) if a["type"] in ("Binary", "Checksum") and a["name"].startswith("tfcapi-lint-")]' "$(RELEASE_ASSETS)"
	@ls -1 "$(RELEASE_ASSETS)"

.PHONY: release-notes
release-notes: ## Write out/release/notes.md from the commits since the previous tag.
	@mkdir -p "$(RELEASE_ASSETS)"
	@prev="$$(git describe --tags --abbrev=0 --match 'v*' "$(VERSION)^" 2>/dev/null || true)"; \
	{ echo "## $(VERSION)"; echo; git log --no-merges --format='- %s' $${prev:+$$prev..}"$(VERSION)"; } >"$(RELEASE_ASSETS)/notes.md"; \
	echo "$@: $(RELEASE_ASSETS)/notes.md (since $${prev:-the first commit})"

.PHONY: release-github
release-github: release-notes ## Create the GitHub release for VERSION from out/release (publishes: operator only).
	gh release create "$(VERSION)" --verify-tag --notes-file "$(RELEASE_ASSETS)/notes.md" \
		$$(find "$(RELEASE_ASSETS)" -maxdepth 1 -type f ! -name notes.md | sort)

##@ Verify

.PHONY: verify
verify: verify-modules verify-schemas verify-components verify-metadata verify-version verify-gen check-licenses verify-templates verify-godoc verify-test-tiers promtool-check promtool-test verify-local-repository ## Run all verifications.

.PHONY: verify-test-tiers
verify-test-tiers: ## Check that e2e code carries the e2e build tag and stays in test/e2e/ and test/env/lifecycle/.
	@hack/verify-test-tiers_test.sh >/dev/null
	@hack/verify-test-tiers.sh

.PHONY: verify-godoc
verify-godoc: ## Check that every declaration, parameter, return value and package is documented (hack/godoccheck).
	go run ./hack/godoccheck .

.PHONY: verify-local-repository
verify-local-repository: $(CLUSTERCTL) $(KUSTOMIZE) ## Generate the provider and both flavors from a clusterctl local repository of the release assets (offline, no cluster).
	@CLUSTERCTL="$(CLUSTERCTL)" hack/verify-local-repository.sh

.PHONY: verify-templates
verify-templates: $(CLUSTERCTL) ## Render templates/ with the pinned clusterctl (hack/verify-templates.sh).
	@CLUSTERCTL="$(CLUSTERCTL)" hack/verify-templates.sh

.PHONY: check-licenses
check-licenses: ## Check that no MPL-2.0 dependency of tfcapi-lint applies Exhibit B (hack/check-licenses.sh).
	@hack/check-licenses.sh

.PHONY: verify-components
verify-components: $(KUSTOMIZE) ## Check the clusterctl components built from config/default.
	KUSTOMIZE="$(KUSTOMIZE)" hack/verify-components.sh

.PHONY: verify-metadata
verify-metadata: ## Validate metadata.yaml and check releaseSeries is append-only against the previous tag.
	@hack/check-metadata_test.sh
	@hack/check-metadata.sh

.PHONY: verify-version
verify-version: ## Check that hack/version.sh prints a valid semantic version for every checkout state.
	@hack/version_test.sh
	@echo "VERSION=$(VERSION)"

.PHONY: verify-modules
verify-modules: ## Check go.work/go.mod pins and that only this repo's own modules are replaced (locally).
	hack/verify-modules.sh

.PHONY: verify-schemas
verify-schemas: ## Validate the contract JSON Schemas and their examples (python3 + jsonschema).
	hack/verify-schemas.sh

.PHONY: verify-gen
verify-gen: generate manifests ## Check generated files are up to date (needs git).
	@if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then \
		echo "verify-gen: not a git repository, skipping diff check"; exit 0; fi; \
	git diff --exit-code

# The opt-in Prometheus component. rules.yaml is a
# PrometheusRule; promtool reads the native rule-file format, so its .spec is
# extracted first. The component cannot be built alone: it is built on top
# of config/default in a scratch overlay. Both targets are part of verify.
PROM_DIR := $(CURDIR)/config/prometheus
define prom-rules
python3 -c 'import sys, yaml; yaml.safe_dump(yaml.safe_load(open(sys.argv[1]))["spec"], open(sys.argv[2], "w"))' "$(PROM_DIR)/rules.yaml" "$$work/rules.yaml"
endef

.PHONY: promtool-check
promtool-check: $(PROMTOOL) $(KUSTOMIZE) ## Check the alert rules with promtool and build the Prometheus component on config/default.
	@work="$$(mktemp -d)"; trap 'rm -rf -- "$$work"' EXIT; \
	$(prom-rules); \
	"$(PROMTOOL)" check rules "$$work/rules.yaml"; \
	printf 'apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\nresources:\n- %s\ncomponents:\n- %s\n' "$$(realpath --relative-to="$$work" "$(CURDIR)/config/default")" "$$(realpath --relative-to="$$work" "$(PROM_DIR)")" > "$$work/kustomization.yaml"; \
	"$(KUSTOMIZE)" build "$$work" > "$$work/out.yaml"; \
	for k in ServiceMonitor PrometheusRule; do grep -q "^kind: $$k$$" "$$work/out.yaml" || { echo "promtool-check: no $$k in the overlay" >&2; exit 1; }; done; \
	echo "promtool-check: rules and component OK"

.PHONY: promtool-test
promtool-test: $(PROMTOOL) ## Unit-test the alert rules (config/prometheus/tests/rules_test.yaml).
	@work="$$(mktemp -d)"; trap 'rm -rf -- "$$work"' EXIT; \
	$(prom-rules); \
	cp "$(PROM_DIR)/tests/rules_test.yaml" "$$work/"; \
	"$(PROMTOOL)" test rules "$$work/rules_test.yaml"

##@ Tools

.PHONY: tools
tools: $(TOOLS) ## Install every pinned tool into hack/tools/bin.

$(CONTROLLER_GEN):
	$(call go-install-tool,$@,sigs.k8s.io/controller-tools/cmd/controller-gen,$(CONTROLLER_GEN_VER),controller-gen)

$(KUSTOMIZE):
	$(call go-install-tool,$@,sigs.k8s.io/kustomize/kustomize/v5,$(KUSTOMIZE_VER),kustomize)

$(GOLANGCI_LINT):
	$(call go-install-tool,$@,github.com/golangci/golangci-lint/v2/cmd/golangci-lint,$(GOLANGCI_LINT_VER),golangci-lint)

$(GORELEASER):
	$(call go-install-tool,$@,github.com/goreleaser/goreleaser/v2,$(GORELEASER_VER),goreleaser)

$(GOIMPORTS):
	$(call go-install-tool,$@,golang.org/x/tools/cmd/goimports,$(GOIMPORTS_VER),goimports)

$(GOTESTSUM):
	$(call go-install-tool,$@,gotest.tools/gotestsum,$(GOTESTSUM_VER),gotestsum)

# golangci-lint with the kube-api-linter module plugin. This performs the
# steps of `golangci-lint custom` (plugins.go import, go get, tidy, build)
# but takes the golangci-lint source from the Go module proxy (checksummed
# via sum.golang.org) instead of `git clone`: `custom` strips GIT_CONFIG_*
# from git's environment, which breaks clones behind an authenticating proxy.
KAL_BUILD_PREFIX := $(TOOLS_BIN)/.kal-build.
$(GOLANGCI_LINT_KAL):
	@mkdir -p "$(TOOLS_BIN)"
	@echo "Building golangci-lint $(GOLANGCI_LINT_VER) with kube-api-linter $(KUBE_API_LINTER_VER)"
	@src="$$(GOWORK=off GOFLAGS= go mod download -json github.com/golangci/golangci-lint/v2@$(GOLANGCI_LINT_VER) | jq -r .Dir)"; \
	work="$$(mktemp -d "$(KAL_BUILD_PREFIX)XXXXXX")"; \
	trap 'if [[ "$$work" == "$(KAL_BUILD_PREFIX)"* ]]; then chmod -R u+w "$$work"; rm -rf -- "$$work"; fi' EXIT; \
	cp -R "$$src/." "$$work/"; \
	chmod -R u+w "$$work"; \
	printf 'package main\n\nimport (\n\t_ "sigs.k8s.io/kube-api-linter"\n)\n' > "$$work/cmd/golangci-lint/plugins.go"; \
	cd "$$work"; \
	export GOTOOLCHAIN=local GOWORK=off GOFLAGS=; \
	go get "sigs.k8s.io/kube-api-linter@$(KUBE_API_LINTER_VER)"; \
	go mod tidy; \
	go build -trimpath -ldflags "-s -w -X 'main.version=$(GOLANGCI_LINT_VER)-kal-$(KUBE_API_LINTER_VER)'" -o "$@" ./cmd/golangci-lint
	@ln -sfn "$(notdir $@)" "$(TOOLS_BIN)/golangci-lint-kube-api-linter"

$(CLUSTERCTL):
	@mkdir -p "$(TOOLS_BIN)"
	@echo "Downloading clusterctl $(CLUSTERCTL_VER) ($(HOST_OS)/$(HOST_ARCH))"
	@want="$(CLUSTERCTL_SHA256_$(HOST_OS)_$(HOST_ARCH))"; \
	if [[ -z "$$want" ]]; then echo "clusterctl: no pinned sha256 for $(HOST_OS)/$(HOST_ARCH)" >&2; exit 1; fi; \
	tmp="$$(mktemp "$(TOOLS_BIN)/.clusterctl.XXXXXX")"; \
	trap 'rm -f "$$tmp"' EXIT; \
	curl -fsSL -o "$$tmp" "https://github.com/kubernetes-sigs/cluster-api/releases/download/$(CLUSTERCTL_VER)/clusterctl-$(HOST_OS)-$(HOST_ARCH)"; \
	echo "$$want  $$tmp" | sha256sum -c --quiet -; \
	chmod +x "$$tmp"; \
	mv "$$tmp" "$@"; \
	ln -sfn "$(notdir $@)" "$(TOOLS_BIN)/clusterctl"

$(PROMTOOL):
	@mkdir -p "$(TOOLS_BIN)"
	@echo "Downloading promtool $(PROMTOOL_VER) ($(HOST_OS)/$(HOST_ARCH))"
	@want="$(PROMTOOL_SHA256_$(HOST_OS)_$(HOST_ARCH))"; \
	if [[ -z "$$want" ]]; then echo "promtool: no pinned sha256 for $(HOST_OS)/$(HOST_ARCH)" >&2; exit 1; fi; \
	dir="prometheus-$(PROMTOOL_VER:v%=%).$(HOST_OS)-$(HOST_ARCH)"; \
	work="$$(mktemp -d "$(TOOLS_BIN)/.promtool.XXXXXX")"; \
	trap 'if [[ "$$work" == "$(TOOLS_BIN)/.promtool."* ]]; then rm -rf -- "$$work"; fi' EXIT; \
	curl -fsSL -o "$$work/p.tar.gz" "https://github.com/prometheus/prometheus/releases/download/$(PROMTOOL_VER)/$$dir.tar.gz"; \
	echo "$$want  $$work/p.tar.gz" | sha256sum -c --quiet -; \
	tar -xzf "$$work/p.tar.gz" -C "$$work" "$$dir/promtool"; \
	mv "$$work/$$dir/promtool" "$@"; \
	ln -sfn "$(notdir $@)" "$(TOOLS_BIN)/promtool"

##@ Cleanup

.PHONY: clean
clean: ## Remove build output (bin/, dist/).
	rm -rf ./bin ./dist
