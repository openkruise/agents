# Build stage
FROM golang:1.26@sha256:4165ee6bf3b7811fd7b40373290405c83d0167dcb87ee5f29c427cdcef45ff8f AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

# Copy go mod and sum files
COPY ../go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy the source code
COPY ../cmd/sandbox-manager ./cmd/sandbox-manager
COPY ../pkg ./pkg
COPY ../api  ./api
COPY ../client ./client
COPY ../proto ./proto
COPY ../test ./test

# Build the binary
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -a -installsuffix cgo -o sandbox-manager ./cmd/sandbox-manager

# Final stage
FROM alpine:3.20@sha256:d9e853e87e55526f6b2917df91a2115c36dd7c696a35be12163d44e6e2a4b6bc AS runtime

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    apk --no-cache add ca-certificates && \
    rm -rf /usr/local/sbin/* && \
    rm -rf /usr/local/bin/* && \
    rm -rf /usr/sbin/* && \
    rm -rf /usr/bin/* && \
    rm -rf /sbin/* && \
    rm -rf /bin/*

WORKDIR /
# Copy the binary from builder stage
COPY --from=builder /app/sandbox-manager .
USER 65532:65532

# Expose port
EXPOSE 8080

# Run the binary
CMD ["/sandbox-manager"]
