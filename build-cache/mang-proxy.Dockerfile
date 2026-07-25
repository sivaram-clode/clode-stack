# Build stage
FROM golang:1.24-alpine AS builder

# Install git and openssh-client for SSH access to private repos
RUN apk add --no-cache git openssh-client && \
    mkdir -p ~/.ssh && \
    ssh-keyscan -H github.com >> ~/.ssh/known_hosts

RUN git config --global url."ssh://git@github.com/clode-labs".insteadOf "https://github.com/clode-labs" && \
    export GOPRIVATE=github.com/clode-labs/* && \
    go env -w GOPRIVATE="github.com/clode-labs/*"

WORKDIR /app

# Copy go mod and sum files
COPY go.mod go.sum ./

# Download dependencies with SSH agent forwarding
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    go mod download

# Copy source code
COPY cmd/ ./cmd/
COPY internal/ ./internal/
COPY mang-docs/ ./mang-docs/

# Build the application with SSH agent forwarding
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/main.go

# Final stage
FROM alpine:latest

RUN apk add --no-cache ca-certificates curl

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/main /app/mang-proxy

# Copy entrypoint script for kernel tuning (optional, for high-perf mode)
COPY deploy/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# Expose port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=10s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# Use entrypoint for kernel tuning before starting proxy
ENTRYPOINT ["/app/entrypoint.sh"]
CMD ["serve"]
