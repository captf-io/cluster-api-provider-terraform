# Copyright 2026 The CAPTF Authors.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Two images, one target each:
#
#   manager      (the default, last stage) /manager (controller-manager) and
#                /runner (injected into Job pods by an init container running
#                `/runner copy`). One image, one digest: the runner in a Job
#                is always the build of the manager that created it.
#   tfcapi-lint  /tfcapi-lint alone, for module authors' CI; the
#                actions/tfcapi-lint GitHub Action runs it.
#
# Build with podman: `make docker-build` / `make docker-build-lint` (single
# arch) or `make docker-buildx` (linux/amd64 + linux/arm64 manifest list).
# The docker CLI talking to the podman socket works too:
# `docker build --target tfcapi-lint -t <img> .`.

# Base images are pinned by the digest of their multi-arch index, so a moved
# tag cannot change what is built. The tag stays in the reference for
# readers; the digest wins. Bump GO_VERSION (also in the Makefile) and
# GO_DIGEST together:
#   skopeo inspect --format '{{.Digest}}' docker://docker.io/library/golang:<GO_VERSION>
ARG GO_VERSION=1.26
ARG GO_DIGEST=sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9

# The source and build stages run on the build host's platform and
# cross-compile, so a multi-arch build needs no emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}@${GO_DIGEST} AS source
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /workspace

# go.work lists ./api and ./test, so all three go.mod/go.sum pairs are needed
# to resolve the workspace (see .dockerignore). They are copied alone and
# the modules downloaded before the sources, so a source-only change reuses
# this layer (from the layer cache locally, from the buildx cache in CI).
# The download has no cache mount on purpose: a cache mount's content is
# not part of the layer, so there would be nothing to cache.
COPY go.work go.mod go.sum ./
COPY api/go.mod api/go.sum ./api/
COPY test/go.mod test/go.sum ./test/
RUN go mod download

COPY . .

# One build stage per image, so a target compiles only its own binaries
# (BuildKit skips the stages a target does not need).
#
# Project-scoped cache id: an id-less cache mount is shared with every other
# build on the host that mounts the same target, and a corrupt object
# written by another project breaks this one. The module cache needs no
# mount: the layer above holds it.
FROM source AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG LDFLAGS=""
RUN --mount=type=cache,id=captf-gobuild,sharing=locked,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="${LDFLAGS}" -o /out/manager ./cmd/manager && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="${LDFLAGS}" -o /out/runner ./cmd/runner && \
    ./hack/verify-static.sh /out/manager /out/runner

FROM source AS build-lint
ARG TARGETOS=linux
ARG TARGETARCH
ARG LDFLAGS=""
RUN --mount=type=cache,id=captf-gobuild,sharing=locked,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="${LDFLAGS}" -o /out/tfcapi-lint ./cmd/tfcapi-lint && \
    ./hack/verify-static.sh /out/tfcapi-lint

# distroless static: no shell, no libc, runs as nonroot, and carries the CA
# bundle that `tfcapi-lint image` needs to pull from a registry. Pinned by
# index digest (both images use the same one); to bump:
#   skopeo inspect --format '{{.Digest}}' docker://gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS tfcapi-lint
COPY --from=build-lint /out/tfcapi-lint /tfcapi-lint
USER 65532:65532
ENTRYPOINT ["/tfcapi-lint"]

# The manager stays the last stage: a build without --target builds it.
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3 AS manager
COPY --from=build /out/manager /manager
COPY --from=build /out/runner /runner
USER 65532:65532
ENTRYPOINT ["/manager"]
