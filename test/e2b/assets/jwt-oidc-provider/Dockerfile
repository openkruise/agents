FROM golang:1.25-bookworm AS builder

WORKDIR /build
COPY test/e2b/assets/jwt-oidc-provider/main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o jwt-oidc-provider main.go

FROM scratch
COPY --from=builder /build/jwt-oidc-provider /jwt-oidc-provider
ENTRYPOINT ["/jwt-oidc-provider"]
