# Build the commit-job binary
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH
ARG VERSION
ARG NERDCTL_BRANCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# Copy the go source
COPY cmd/commit-job/ cmd/commit-job/
COPY api api/
COPY pkg pkg/
COPY client client/

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -ldflags "-X main.version=${VERSION}" -a -o commit-job ./cmd/commit-job

WORKDIR /workspace/nerdctl-builder
RUN git clone -b ${NERDCTL_BRANCH:-v2.0.0} https://github.com/containerd/nerdctl.git
RUN cd nerdctl && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} make

FROM alpine:3.20
WORKDIR /

COPY --from=builder /workspace/commit-job .
COPY --from=builder /workspace/nerdctl-builder/nerdctl/_output/nerdctl /usr/bin/nerdctl

ENTRYPOINT ["/commit-job"]
