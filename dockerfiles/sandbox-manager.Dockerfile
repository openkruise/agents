# Build stage
FROM golang:1.24 AS builder

WORKDIR /app

# Copy go mod and sum files
COPY ../go.mod go.sum ./

# Copy the source code
COPY ../cmd/sandbox-manager ./cmd/sandbox-manager
COPY ../pkg ./pkg
COPY ../api  ./api
COPY ../client ./client
COPY ../proto ./proto
# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o sandbox-manager ./cmd/sandbox-manager

# Final stage
FROM alpine:3.20 AS runtime

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
