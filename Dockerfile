# Build the manager binary
FROM registry.access.redhat.com/ubi9/go-toolset:1.26.7-1787774815 AS builder

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
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o manager cmd/main.go
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o wait-for ./cmd/wait-for/

# Runtime is UBI Micro: Red Hat-signed RHEL content, no package manager, no shell.
# Smaller TCB than ubi-minimal; same errata/scan stream as OpenShift.
# Defaults to root user.
FROM registry.access.redhat.com/ubi9/ubi-micro:9.8-1787778798
WORKDIR /
COPY --from=builder /workspace/manager /manager
COPY --from=builder /workspace/wait-for /wait-for
USER 65532:65532

ENTRYPOINT ["/manager"]
