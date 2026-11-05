# Build the node checkpoint agent binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY ../go.mod go.mod
COPY ../go.sum go.sum
RUN go mod download

COPY ../cmd/checkpoint-agent/ cmd/checkpoint-agent/
COPY ../api api/
COPY ../pkg pkg/
COPY ../client client/
COPY ../proto proto/

RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o checkpoint-agent ./cmd/checkpoint-agent/

FROM alpine:3.20
WORKDIR /
COPY --from=builder /workspace/checkpoint-agent .
# Runs as root on purpose: the agent talks to the CRI socket and owns a
# directory under the host's /var/lib, both of which are root-only.
USER 0:0

ENTRYPOINT ["/checkpoint-agent"]
