# Build stage
FROM golang:1.25-alpine AS builder

# Install git and openssh-client for SSH access to private repos
RUN apk add --no-cache git openssh-client && \
    mkdir -p ~/.ssh && \
    ssh-keyscan -H github.com >> ~/.ssh/known_hosts

RUN git config --global url."ssh://git@github.com/clode-labs".insteadOf "https://github.com/clode-labs" && \
    go env -w GOPRIVATE="github.com/clode-labs/*"

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies with SSH agent forwarding
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh go mod download

# Copy source code
COPY . .

# Build all workers with SSH agent forwarding
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o narnia-workers ./cmd/workers/narnia-workers

# Runtime stage - use railpack-runtime for build worker language support
FROM ghcr.io/railwayapp/railpack-runtime:latest

# Install additional runtime dependencies
RUN apt-get update && apt-get install -y ca-certificates curl git && apt-get clean && rm -rf /var/lib/apt/lists/*

# Install Node.js 22 — required by FrontendDeployActivity when it receives a
# `clode.artifact-type=frontend-source` OCI artifact and runs `npm ci && npm
# run build` inline (see pkg/activities/frontend_source_build.go). Matches the
# NodeSource setup used in benji/Dockerfile so we're on the same Node 22 line
# across the workspace.
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && apt-get clean && rm -rf /var/lib/apt/lists/* \
    && node --version && npm --version

# Install aramb-cli (deployment tool) — pinned to a specific release.
# Bump ARAMB_CLI_VERSION to upgrade; the new value changes the layer hash
# so BuildKit re-downloads instead of reusing the stale `latest` blob from
# its cache. CI can also override with --build-arg ARAMB_CLI_VERSION=…
ARG ARAMB_CLI_VERSION=v1.0.0-beta12
RUN ARCH=$(uname -m) && \
    if [ "$ARCH" = "x86_64" ]; then ARCH="amd64"; elif [ "$ARCH" = "aarch64" ]; then ARCH="arm64"; fi && \
    curl -fsSL -o /usr/local/bin/aramb \
      "https://github.com/aramb-ai/release-beta/releases/download/${ARAMB_CLI_VERSION}/aramb-linux-${ARCH}" && \
    chmod +x /usr/local/bin/aramb && \
    /usr/local/bin/aramb --version

# Create non-root user (Debian syntax)
RUN useradd -m -s /bin/bash narnia

# Set working directory
WORKDIR /app

# Copy workers from builder stage with narnia ownership applied in the same
# layer — using `RUN chown -R …` afterwards rewrites every file into a fresh
# layer and effectively doubles the binary's contribution to image size
# (138 MB binary + 138 MB chown'd copy = 276 MB).
COPY --from=builder --chown=narnia:narnia /app/narnia-workers .

# Switch to non-root user
USER narnia

# Default command (can be overridden)
CMD ["./narnia-workers"]
