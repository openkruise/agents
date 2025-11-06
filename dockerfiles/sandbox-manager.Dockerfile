# Build stage
FROM registry.cn-hangzhou.aliyuncs.com/airanthem/mirrors:golang-1.24.9-alpine AS builder

WORKDIR /app

ENV GOPROXY https://goproxy.cn,direct

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
FROM registry.cn-hangzhou.aliyuncs.com/airanthem/mirrors:alpine-3.21 as runtime

RUN sed -i 's/dl-cdn.alpinelinux.org/mirrors.aliyun.com/g' /etc/apk/repositories && \
    apk --no-cache add ca-certificates

WORKDIR /root/

# Copy the binary from builder stage
COPY --from=builder /app/sandbox-manager .
COPY ../assets/template/builtin_templates /root/builtin_templates

# Expose port
EXPOSE 8080

# Run the binary
CMD ["./sandbox-manager"]
