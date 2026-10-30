FROM golang:1.25-alpine AS builder
ARG TARGETOS
ARG TARGETARCH

# Install dependencies
RUN apk update && apk add --no-cache git curl bash

RUN cd src && git clone https://github.com/e2b-dev/infra -b 2025.33
RUN cd src/infra/packages/envd && CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o bin/envd

FROM golang:1.26-alpine AS compatibility-builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
COPY proto/envd ./proto/envd
COPY pkg/runtime/envd ./pkg/runtime/envd
COPY pkg/agent-runtime/opensandbox ./pkg/agent-runtime/opensandbox
COPY cmd/opensandbox-runtime ./cmd/opensandbox-runtime
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -mod=readonly -trimpath -o /opensandbox-runtime ./cmd/opensandbox-runtime

FROM alpine:3.21 AS runtime
WORKDIR /workspace
COPY --from=builder /go/src/infra/packages/envd/bin/envd /workspace/envd
COPY --from=compatibility-builder /opensandbox-runtime /workspace/opensandbox-runtime
COPY "../cmd/agent-runtime/entrypoint.sh" /workspace/entrypoint.sh
COPY ../pkg/agent-runtime/envd-run.sh /workspace/envd-run.sh
ENTRYPOINT ["sh", "/workspace/entrypoint.sh"]
