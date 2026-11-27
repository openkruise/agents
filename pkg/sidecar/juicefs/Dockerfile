# Stage 1: build mount-proxy-server from alibaba-cloud-csi-driver source
# (the customfuse feature on PR #1722). The repository is cloned inside the
# build instead of relying on a local --build-context; override
# CSI_DRIVER_REPO with a mirror when GitHub is not directly reachable.
# IMAGE_PREFIX overrides the Docker Hub prefix for environments without
# direct Docker Hub access (e.g. docker.1panel.live).
ARG IMAGE_PREFIX=docker.io
ARG CSI_DRIVER_REPO=https://github.com/kubernetes-sigs/alibaba-cloud-csi-driver.git
# The ref to fetch from CSI_DRIVER_REPO. pull/1722/head is GitHub's PR ref;
# when overriding CSI_DRIVER_REPO with a mirror, also override this: most
# mirrors do not expose PR refs (and the PR's source branch lives on
# the author's fork), so use a branch or commit the mirror carries.
ARG CSI_DRIVER_REF=pull/1722/head
# CSI_DRIVER_SHA pins pr-1722's head; override with --build-arg when the
# branch advances (after verifying compatibility). The original pin
# (d45ddfa) was force-pushed out of pr-1722's history, so the build fails
# loudly instead of trusting the moving branch.
ARG CSI_DRIVER_SHA=baf1dca5269965c1ee04ec900a0ba5de4f310f8e
# GOPROXY defaults to a mainland mirror; override with --build-arg
# for environments with other reachable proxies.
ARG GOPROXY=https://goproxy.cn,direct
# pr-1722's go.mod requires go >= 1.26.0; the sidecar stage below uses
# 1.26 as well because this repo's go.mod requires >= 1.25.9.
FROM ${IMAGE_PREFIX}/golang:1.26 AS builder-proxy
ARG CSI_DRIVER_REPO
ARG CSI_DRIVER_REF
ARG CSI_DRIVER_SHA
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
WORKDIR /src
# Fetch PR #1722's head and verify the pinned commit; the build fails
# loudly if the branch has advanced (the pin must be re-verified and
# updated deliberately).
RUN git init . &&     git remote add origin "${CSI_DRIVER_REPO}" &&     git fetch --depth 1 origin "${CSI_DRIVER_REF}" &&     git checkout FETCH_HEAD &&     test "$(git rev-parse HEAD)" = "${CSI_DRIVER_SHA}" ||         { echo "ERROR: checkout does not match CSI_DRIVER_SHA (did the pinned ref advance?)" >&2; exit 1; } &&     CGO_ENABLED=0 go build -o /out/csi-mount-proxy-server ./cmd/mount-proxy-server

# Stage 2: build csi-sidecar-customfuse (CSI node server) from agents source.
# Build context must be the repository root.
# !!! Run the build from the repository root, e.g.:
# !!! docker buildx build -f pkg/sidecar/juicefs/Dockerfile -t <image> .
# !!! (Stage 1 clones the pinned csi-driver commit from GitHub itself.)
# go.mod requires go >= 1.25.9; the 1.26 image satisfies it and matches the
# builder-proxy stage above.
FROM ${IMAGE_PREFIX}/golang:1.26 AS builder-sidecar
ARG GOPROXY
ENV GOPROXY=${GOPROXY}
COPY go.mod go.sum ./
COPY cmd/csi-sidecar-customfuse ./cmd/csi-sidecar-customfuse
COPY pkg/agent-runtime/customfusesidecar ./pkg/agent-runtime/customfusesidecar
# customfusesidecar imports customfusevalidate; omit it and the build
# fails. Keep this COPY set in sync with the packages customfusesidecar
# imports whenever a new in-repo dependency is added.
COPY pkg/agent-runtime/customfusevalidate ./pkg/agent-runtime/customfusevalidate
RUN CGO_ENABLED=0 go build -o /out/csi-sidecar-customfuse ./cmd/csi-sidecar-customfuse

# Stage 3: JuiceFS client + both binaries + entrypoint
# Pinned by digest (Debian 12, bash 5.2.15 — start.sh requires bash >= 5.1
# for wait -n -p). Do not float on :latest: the build must stay
# reproducible and the upstream image updates JuiceFS client versions.
FROM ${IMAGE_PREFIX}/juicedata/mount@sha256:ee34645eae22378bfa7e9728210a243b32962971942ce52011cd08497cf38927

COPY --from=builder-proxy /out/csi-mount-proxy-server /usr/local/bin/
COPY --from=builder-sidecar /out/csi-sidecar-customfuse /usr/local/bin/
# go build outputs already carry the executable bit; the explicit chmod is
# belt-and-braces for COPY paths that lose file modes.
RUN chmod +x /usr/local/bin/csi-mount-proxy-server /usr/local/bin/csi-sidecar-customfuse

# Copy entrypoint (must be /entrypoint.sh — mount-proxy looks for this path)
COPY pkg/sidecar/juicefs/entrypoint-juicefs.sh /entrypoint.sh
# start.sh runs both processes: mount-proxy-server (mounter.sock) and the
# CSI node server (csi.sock).
COPY pkg/sidecar/start.sh /start.sh
RUN chmod +x /entrypoint.sh /start.sh

ENTRYPOINT ["/start.sh"]
