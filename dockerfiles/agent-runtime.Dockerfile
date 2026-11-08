FROM golang:1.26-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

# Install dependencies
RUN apk update && apk add --no-cache git curl bash

RUN cd src && git clone https://github.com/e2b-dev/infra -b 2025.33
RUN cd src/infra/packages/envd && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o bin/envd

FROM alpine:3.21 AS runtime
WORKDIR /workspace
COPY --from=builder /go/src/infra/packages/envd/bin/envd /workspace/envd
COPY "../cmd/agent-runtime/entrypoint.sh" /workspace/entrypoint.sh
COPY ../pkg/agent-runtime/envd-run.sh /workspace/envd-run.sh
# Ship each artifact's digest next to it. The entrypoint then verifies a
# surviving destination by hashing only the destination, without reading the
# source copy.
RUN md5sum /workspace/envd > /workspace/envd.md5 \
    && md5sum /workspace/envd-run.sh > /workspace/envd-run.sh.md5
ENTRYPOINT ["sh", "/workspace/entrypoint.sh"]
