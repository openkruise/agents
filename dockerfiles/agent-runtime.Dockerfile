FROM golang:1.26-alpine@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS builder
ARG TARGETOS
ARG TARGETARCH

# Install dependencies
RUN apk update && apk add --no-cache git curl bash

RUN cd src && git clone https://github.com/e2b-dev/infra -b 2025.33
RUN cd src/infra/packages/envd && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o bin/envd

FROM alpine:3.21@sha256:bc21487101ae47172509130ee1092cc273a831968d64b617776d1f69c81e42ab AS runtime
WORKDIR /workspace
COPY --from=builder /go/src/infra/packages/envd/bin/envd /workspace/envd
COPY "../cmd/agent-runtime/entrypoint.sh" /workspace/entrypoint.sh
COPY ../pkg/agent-runtime/envd-run.sh /workspace/envd-run.sh
# Staged into the envd volume by the envd-scripts init container for
# startupMode=Deferred sandboxes; the entrypoint does not publish it.
COPY ../pkg/agent-runtime/run_with_envs.sh /workspace/run_with_envs.sh
# Ship each artifact's digest next to it. The entrypoint then verifies a
# surviving destination by hashing only the destination, without reading the
# source copy.
RUN md5sum /workspace/envd > /workspace/envd.md5 \
    && md5sum /workspace/envd-run.sh > /workspace/envd-run.sh.md5
ENTRYPOINT ["sh", "/workspace/entrypoint.sh"]
