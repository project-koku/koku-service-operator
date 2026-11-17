# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.26.10-1792592174 AS builder

# Pin the validated native Go module and retain the Red Hat toolchain. The
# no_openssl build tag below selects native crypto instead of the legacy backend.
# Both main packages bake in fips140=auto; strict mode rejects disabling it on
# a FIPS host. CGO is unnecessary for the native module.
# Write outputs under /tmp so rootless Podman / OpenShift random-UID builders
# can create the binaries (WORKDIR may not be writable).
ENV CGO_ENABLED=0 \
    GOFIPS140=v1.0.0 \
    GOEXPERIMENT=strictfipsruntime \
    GOTOOLCHAIN=local

ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build operator manager and wait-for helper (both shipped in the same image).
# wait-for is used by init containers in managed pods so they don't need a
# separate image — they reuse the operator image itself.
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -tags=no_openssl,nativefips -o /tmp/manager ./cmd
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -tags=no_openssl,nativefips -o /tmp/wait-for ./cmd/wait-for/
RUN go version -m /tmp/manager /tmp/wait-for

# Build rbac-mini at a pinned commit for spec.rbac.backend: go (COST-8610).
# Same FIPS contract as manager/wait-for (RH go-toolset 1.26 + GOTOOLCHAIN=local).
# Mini go.mod must be <= builder Go (1.26.x) and include native FIPS
# (cmd/rbac-go/fips.go). How to bump: docs/development/rbac-backend-go.md.
#
# Optional local override: `make docker-build RBAC_MINI_LOCAL_DIR=../rbac-mini`
# stages sources into rbac-mini-src/ and skips git clone.
FROM registry.access.redhat.com/ubi9/go-toolset:1.26.10-1792592174 AS rbac-mini-build

ENV CGO_ENABLED=0 \
    GOFIPS140=v1.0.0 \
    GOEXPERIMENT=strictfipsruntime \
    GOTOOLCHAIN=local

ARG TARGETOS
ARG TARGETARCH
ARG RBAC_MINI_REPO=https://github.com/project-koku/rbac-mini
# Pin: rbac-mini main after FIPS + go 1.26.5 (PR #2 / COST-8610).
ARG RBAC_MINI_REF=e7db2608f79be2576c66651198eccf29d51ddd83

# Makefile always creates rbac-mini-src/ (local tree or .gitkeep placeholder).
# Local tree has go.mod → build that. Otherwise clone the pin into a fresh dir
# (placeholder .gitkeep would make `git clone .` fail).
COPY rbac-mini-src/ /tmp/rbac-mini-context/
# -buildvcs=false: tree may lack a usable .git identity inside the builder.
# Outputs stay under /tmp (writable by the go-toolset non-root UID).
RUN if [ -f /tmp/rbac-mini-context/go.mod ]; then \
      echo "Building rbac-mini from build-context rbac-mini-src/" \
      && SRC=/tmp/rbac-mini-context; \
    else \
      echo "Cloning rbac-mini @$RBAC_MINI_REF" \
      && git clone "$RBAC_MINI_REPO" /tmp/rbac-mini-src \
      && git -C /tmp/rbac-mini-src checkout --detach "$RBAC_MINI_REF" \
      && SRC=/tmp/rbac-mini-src; \
    fi \
 && cd "$SRC" \
 && GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -buildvcs=false -a -tags=no_openssl,nativefips -o /tmp/rbac-mini ./cmd/rbac-go \
 && go version -m /tmp/rbac-mini

# Use Red Hat UBI Micro as the minimal supported runtime image.
FROM registry.access.redhat.com/ubi9/ubi-micro:9.8-1791895366

WORKDIR /
COPY --from=builder /tmp/manager /manager
COPY --from=builder /tmp/wait-for /wait-for
COPY --from=rbac-mini-build /tmp/rbac-mini /usr/local/bin/rbac-mini
USER 65532:65532

ENTRYPOINT ["/manager"]
