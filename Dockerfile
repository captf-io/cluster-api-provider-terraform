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

# Manager image: /manager (controller-manager) and /runner (injected into Job
# pods by an init container running `/runner copy`). One image, one digest:
# the runner in a Job is always the build of the manager that created it.
#
# Build with podman: `make docker-build` (single arch) or `make docker-buildx`
# (linux/amd64 + linux/arm64 manifest list). The docker CLI talking to the
# podman socket works too: `docker build -t <img> .`.

# Base images are pinned by the digest of their multi-arch index, so a moved
# tag cannot change what is built. The tag stays in the reference for
# readers; the digest wins. Bump GO_VERSION (also in the Makefile) and
# GO_DIGEST together:
#   skopeo inspect --format '{{.Digest}}' docker://docker.io/library/golang:<GO_VERSION>
ARG GO_VERSION=1.26
ARG GO_DIGEST=sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9

# The build stage runs on the build host's platform and cross-compiles, so a
# multi-arch build needs no emulation.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:${GO_VERSION}@${GO_DIGEST} AS build
ARG TARGETOS=linux
ARG TARGETARCH
ARG LDFLAGS=""
ENV CGO_ENABLED=0 GOTOOLCHAIN=local GOFLAGS=-trimpath
WORKDIR /workspace

# go.work lists ./api, so its go.mod/go.sum are part of the context (see
# .dockerignore).
COPY . .

# Project-scoped cache ids: an id-less cache mount is shared with every other
# build on the host that mounts the same target, and a corrupt module
# extracted by another project breaks this one.
RUN --mount=type=cache,id=captf-gomod,sharing=locked,target=/go/pkg/mod \
    --mount=type=cache,id=captf-gobuild,sharing=locked,target=/root/.cache/go-build \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="${LDFLAGS}" -o /out/manager ./cmd/manager && \
    GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -ldflags="${LDFLAGS}" -o /out/runner ./cmd/runner && \
    ./hack/verify-static.sh /out/manager /out/runner

# distroless static: no shell, no libc, runs as nonroot. Pinned by index
# digest; to bump:
#   skopeo inspect --format '{{.Digest}}' docker://gcr.io/distroless/static:nonroot
FROM gcr.io/distroless/static:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/manager /manager
COPY --from=build /out/runner /runner
USER 65532:65532
ENTRYPOINT ["/manager"]
