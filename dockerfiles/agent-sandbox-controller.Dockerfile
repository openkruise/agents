# Build the manager binary
FROM golang:1.25 AS builder
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
# IMPORTANT: copy the whole cmd/agent-sandbox-controller/ directory and build by
# package path. Single-file builds (go build cmd/main.go) silently drop sibling
# files such as ca_binding.go, leading to "undefined: executeCABindings".
COPY cmd/agent-sandbox-controller/ cmd/agent-sandbox-controller/
COPY api api/
COPY pkg pkg/
COPY client client/
COPY proto proto/
COPY test test/

# Build for Docker's target platform when buildx provides it, and fall back to
# the builder platform for normal docker builds.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -a -o manager ./cmd/agent-sandbox-controller/

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM alpine:3.20
WORKDIR /
RUN mkdir -p /home/nonroot/sandbox-controller-webhook-certs && \
    chmod 777 /home/nonroot/sandbox-controller-webhook-certs && \
    chown 65532:65532 /home/nonroot/sandbox-controller-webhook-certs
COPY --from=builder /workspace/manager .
USER 65532:65532

ENTRYPOINT ["/manager"]
