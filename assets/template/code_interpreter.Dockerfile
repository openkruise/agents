FROM golang:1.24-alpine AS builder

# Install dependencies
RUN apk update && apk add --no-cache git curl bash

RUN cd src && git clone https://github.com/e2b-dev/infra -b 2025.33
COPY authenticate.go.patch src/infra/packages/envd/internal/permissions/authenticate.go
RUN cd src/infra/packages/envd && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -a -o bin/envd

FROM e2bdev/code-interpreter:latest AS runtime

COPY --from=builder /go/src/infra/packages/envd/bin/envd /usr/local/bin/envd
ARG S6_OVERLAY_VERSION=3.2.1.0

ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-noarch.tar.xz /tmp
RUN tar -C / -Jxpf /tmp/s6-overlay-noarch.tar.xz
ADD https://github.com/just-containers/s6-overlay/releases/download/v${S6_OVERLAY_VERSION}/s6-overlay-x86_64.tar.xz /tmp
RUN tar -C / -Jxpf /tmp/s6-overlay-x86_64.tar.xz

COPY envd-run.sh /etc/services.d/envd/run
COPY code-interpreter-run.sh /etc/services.d/code-interpreter/run

# Create default user
RUN useradd -ms /bin/bash user && \
    usermod -aG sudo user && \
    passwd -d -q user && \
    echo "user ALL=(ALL:ALL) NOPASSWD: ALL" >>/etc/sudoers && \
    mkdir -p /home/user && \
    chmod 777 -R /home/user && \
    chown -R user:user /home/user

ENTRYPOINT ["/init"]
