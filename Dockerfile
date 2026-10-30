# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.26.10-1792592174 AS builder

# Pin the validated native Go module and retain the Red Hat toolchain. The
# no_openssl build tag below selects native crypto instead of the legacy backend.
# Both main packages bake in fips140=auto; strict mode rejects disabling it on
# a FIPS host. CGO is unnecessary for the native module.
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
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -tags=no_openssl,nativefips -o manager ./cmd
RUN GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -tags=no_openssl,nativefips -o wait-for ./cmd/wait-for/
RUN go version -m manager wait-for

# Use Red Hat UBI Micro as the minimal supported runtime image.
FROM registry.access.redhat.com/ubi9/ubi-micro:9.8-1791895366

WORKDIR /
COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/wait-for /wait-for
USER 65532:65532

ENTRYPOINT ["/manager"]
