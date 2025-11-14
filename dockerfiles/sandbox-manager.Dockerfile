# Build stage
FROM hub.docker.alibaba-inc.com/chorus-ci/golang:1.24 AS builder

WORKDIR /app

# Copy go mod and sum files
COPY ../go.mod go.sum ./

COPY vendor ./vendor

# Copy the source code
COPY ../cmd/sandbox-manager ./cmd/sandbox-manager
COPY ../pkg ./pkg
COPY ../api  ./api
COPY ../client ./client
# Build the binary
RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o sandbox-manager ./cmd/sandbox-manager

# Final stage
FROM hub.docker.alibaba-inc.com/chorus-ci/alpine:3.20 as runtime

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the binary from builder stage
COPY --from=builder /app/sandbox-manager .

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./sandbox-manager"]
