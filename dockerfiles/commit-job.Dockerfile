# Build the commit-job binary
FROM golang:1.25 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY ../go.mod go.mod
COPY ../go.sum go.sum
RUN go mod download

COPY ../cmd/commit-job/ cmd/commit-job/
COPY ../api api/
COPY ../pkg pkg/
COPY ../client client/

# Build
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -o commit-job cmd/commit-job/main.go

# Final image with nerdctl for commit+push operations
FROM alpine:3.20
RUN apk add --no-cache nerdctl
WORKDIR /
COPY --from=builder /workspace/commit-job .

ENTRYPOINT ["/commit-job"]
