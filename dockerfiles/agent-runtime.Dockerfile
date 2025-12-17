FROM golang:1.24-alpine AS builder

# Install dependencies
RUN apk update && apk add --no-cache git curl bash

RUN cd src && git clone https://github.com/e2b-dev/infra -b 2025.33
RUN cd src/infra/packages/envd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o bin/envd

FROM alpine:3.21 AS runtime
WORKDIR /workspace
COPY --from=builder /go/src/infra/packages/envd/bin/envd /workspace/envd
COPY "../cmd/agent-runtime/entrypoint.sh" /workspace/entrypoint.sh
COPY ../pkg/agent-runtime/envd-run.sh /workspace/envd-run.sh
ENTRYPOINT ["sh", "/workspace/entrypoint.sh"]