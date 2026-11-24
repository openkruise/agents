FROM golang:1.26-bookworm@sha256:dc40a13b61304647ed7e5d581d246b86413dfc29d63c6d018310feb93cd98fc0 AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o sandbox-gateway.so ./cmd/sandbox-gateway/.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o sandbox-gateway-cert-init ./cmd/sandbox-gateway-cert-init/.

FROM envoyproxy/envoy:contrib-v1.37.3@sha256:8c5cc4d5f72422f8200f5d6c200bfc2d74b761c860b79d28589ecc285790eca6

COPY --from=builder /build/sandbox-gateway.so /etc/envoy/sandbox-gateway.so
COPY --from=builder /build/sandbox-gateway-cert-init /usr/local/bin/sandbox-gateway-cert-init
# COPY envoy.yaml /etc/envoy/envoy.yaml

ENTRYPOINT ["envoy", "-c", "/etc/envoy/envoy.yaml"]
