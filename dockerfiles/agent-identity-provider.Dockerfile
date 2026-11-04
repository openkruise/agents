# Build the agent-identity-provider binary
FROM golang:1.26 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
# Copy the Go Modules manifests
COPY ../go.mod go.mod
COPY ../go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
# IMPORTANT: copy the whole cmd/agent-identity-provider/ directory and build by
# package path. A single-file build silently drops sibling files.
#
# proto/ and test/ are deliberately absent: nothing in this binary's dependency
# closure reaches them, unlike the controller image.
COPY ../cmd/agent-identity-provider/ cmd/agent-identity-provider/
COPY ../api api/
COPY ../pkg pkg/
COPY ../client client/

# Build
# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o agent-identity-provider ./cmd/agent-identity-provider/

FROM alpine:3.20 AS runtime

# Remove the shells and utilities. This process holds the token signing key and
# the credential encryption keys, so it is the image here where leaving an
# interactive shell behind is least justifiable, and nothing needs one: the
# entrypoint is a static binary and both probes are httpGet.
#
# /etc/ssl is deliberately untouched. alpine's base already carries
# /etc/ssl/certs/ca-certificates.crt through ca-certificates-bundle, and the
# AgentAuthenticationConfig controller verifies external discovery documents
# against it. Extending this to /etc/ssl would make every issuer resolution
# fail as an unknown authority, and every AgentIdentity depending on one would
# sit NotReady with nothing in the provider logs pointing at the image.
RUN rm -rf /usr/local/sbin/* && \
    rm -rf /usr/local/bin/* && \
    rm -rf /usr/sbin/* && \
    rm -rf /usr/bin/* && \
    rm -rf /sbin/* && \
    rm -rf /bin/*

WORKDIR /
COPY --from=builder /workspace/agent-identity-provider .
# Matches runAsNonRoot in the Deployment. Nothing is written at runtime: the
# signing key, the serving certificate and the encryption keys are all
# read-only mounts, which is why no writable directory is prepared here and why
# the Deployment can set readOnlyRootFilesystem.
USER 65532:65532

ENTRYPOINT ["/agent-identity-provider"]
