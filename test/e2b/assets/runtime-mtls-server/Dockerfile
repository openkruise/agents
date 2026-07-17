FROM golang:1.25-bookworm AS builder

WORKDIR /build
COPY test/e2b/assets/runtime-mtls-server/main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o runtime-mtls-server main.go

FROM scratch
COPY --from=builder /build/runtime-mtls-server /runtime-mtls-server
ENTRYPOINT ["/runtime-mtls-server"]
