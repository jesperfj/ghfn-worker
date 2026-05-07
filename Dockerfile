# syntax=docker/dockerfile:1.7
#
# Build context: this directory. Deps are fetched from the public Go module
# proxy at build time (no vendor/ in the repo).
#
# Multi-arch: TARGETARCH is provided by buildx. Plain `docker build` defaults
# it to the host arch on recent Docker; the yq download falls back to amd64.

###############
# Build stage #
###############
FROM golang:1.25-alpine AS builder
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY main.go ./
COPY internal/ ./internal/

ARG TARGETARCH
ENV CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH:-amd64}
RUN go build -ldflags="-s -w" -o /out/worker .

#################
# Runtime stage #
#################
FROM alpine:3.20

# Tools the bash/python function authors are likely to reach for. yq comes
# from mikefarah/yq (Go binary, statically linked); the apk repo's `yq` is a
# different (Python) tool, so we install ours from a release asset.
RUN apk add --no-cache \
        bash \
        coreutils \
        ca-certificates \
        curl \
        git \
        jq \
        python3 \
        py3-pip \
        tini \
    && ARCH=$(uname -m) \
    && case "$ARCH" in \
        x86_64)  YQ_ARCH=amd64 ;; \
        aarch64) YQ_ARCH=arm64 ;; \
        *)       YQ_ARCH=amd64 ;; \
       esac \
    && curl -fsSL -o /usr/local/bin/yq \
        "https://github.com/mikefarah/yq/releases/download/v4.44.3/yq_linux_${YQ_ARCH}" \
    && chmod +x /usr/local/bin/yq \
    && yq --version

# Non-root user. Numeric IDs so Pod fsGroup can grant volume access.
RUN addgroup -g 1000 worker \
    && adduser -u 1000 -G worker -s /bin/bash -D worker

# Default mount point for the function repo. The Deployment mounts an
# emptyDir (or hostPath) here, and the worker scans /repo/functions.
RUN mkdir -p /repo && chown 1000:1000 /repo

COPY --from=builder /out/worker /usr/local/bin/worker

USER 1000:1000
WORKDIR /home/worker

# Mode (local hostPath vs git-pulled) is selected by the deployment, not the
# image: set LOCAL_REPO_PATH=/repo + mount a hostPath there for local-dev mode,
# or set REPO_URL + REPO_BRANCH (+ optional GITHUB_TOKEN) for git mode.

# tini reaps zombies left behind by short-lived bash/python children.
ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/worker"]
