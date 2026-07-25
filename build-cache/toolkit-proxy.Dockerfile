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

# Download dependencies — result cached as a Docker layer by GHA cache
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    go mod download

# Copy source code
COPY . .

# Build — compiled artifact cache persists on the Blacksmith runner
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/main.go

# Final stage
FROM alpine:3.21

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/main /app/toolkit-proxy
# NEVER COPY .env file to the container

# Expose port
EXPOSE 8080

# Run the application
CMD ["./toolkit-proxy"]