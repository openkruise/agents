FROM golang:1.24-bookworm AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o sandbox-gateway.so ./cmd/sandbox-gateway/.

FROM envoyproxy/envoy:contrib-v1.37.1

COPY --from=builder /build/sandbox-gateway.so /etc/envoy/sandbox-gateway.so
# COPY envoy.yaml /etc/envoy/envoy.yaml

ENTRYPOINT ["envoy", "-c", "/etc/envoy/envoy.yaml"]
