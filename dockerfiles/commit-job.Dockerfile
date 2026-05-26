# Build the commit-job binary
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG NERDCTL_BRANCH=v2.0.3

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/commit-job/ cmd/commit-job/
COPY api api/
COPY pkg pkg/
COPY client client/

# Build commit-job binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o commit-job cmd/commit-job/main.go

# Build nerdctl from source
RUN mkdir nerdctl-builder && cd nerdctl-builder
WORKDIR /workspace/nerdctl-builder
RUN git clone -b ${NERDCTL_BRANCH:-v2.0.3} https://github.com/containerd/nerdctl.git
RUN cd nerdctl && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} make

# Final image
FROM alpine:3.20
WORKDIR /
COPY --from=builder /workspace/commit-job .
COPY --from=builder /workspace/nerdctl-builder/nerdctl/_output/nerdctl /usr/bin/nerdctl

ENTRYPOINT ["/commit-job"]
