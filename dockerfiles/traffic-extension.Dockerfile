# Build the traffic-extension binary
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
COPY cmd/traffic-extension cmd/traffic-extension
COPY api api/
COPY pkg pkg/

# Build for Docker's target platform when buildx provides it, and fall back to
# the builder platform for normal docker builds.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -a -o traffic-extension ./cmd/traffic-extension

# Use alpine as a minimal base image to package the binary.
FROM alpine:3.20
WORKDIR /
COPY --from=builder /workspace/traffic-extension .
USER 65532:65532

ENTRYPOINT ["/traffic-extension"]
