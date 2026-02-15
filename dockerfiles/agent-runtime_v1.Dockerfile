# Build stage
FROM golang:1.24 AS builder

WORKDIR /app

# Copy go mod and sum files
COPY ../go.mod go.sum ./

# Copy the source code
COPY ../cmd/agent-runtime ./cmd/agent-runtime
COPY ../pkg ./pkg
COPY ../api  ./api
COPY ../client ./client

# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o sandbox-agent-runtime ./cmd/agent-runtime

# Final stage
FROM alpine:3.20 as runtime

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
COPY --from=builder /app/sandbox-agent-runtime .
USER 65532:65532

# Expose port
EXPOSE 49983

# Run the binary
CMD ["/sandbox-agent-runtime"]
