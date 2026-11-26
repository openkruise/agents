FROM golang:1.25-bookworm@sha256:3b4a11519ad929d1e1d261a12cff056f0c85b735253d7d861346b9c6f8b36437 AS builder

WORKDIR /build
COPY test/e2b/assets/jwt-oidc-provider/main.go .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o jwt-oidc-provider main.go

FROM scratch
COPY --from=builder /build/jwt-oidc-provider /jwt-oidc-provider
ENTRYPOINT ["/jwt-oidc-provider"]
