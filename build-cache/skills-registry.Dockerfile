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
    GIT_TRACE=1 \
    GIT_CURL_VERBOSE=1 \
    GIT_SSH_COMMAND="ssh -v" \
    go mod download

# Copy source code
COPY . .

# Build the server binary with SSH agent forwarding
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/main.go

# Build aramb-skills client binary (independent — failure does not block the server image)
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked CGO_ENABLED=0 GOOS=linux go build -o aramb-skills ./cmd/aramb-skills || true

# Final stage
FROM alpine:latest

WORKDIR /app

# Install Node.js and npm (required for Claude Code CLI)
RUN apk add --no-cache nodejs npm

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Copy binaries from builder
COPY --from=builder /app/main /app/skills-registry
COPY --from=builder /app/aramb-skills* /app/
# NEVER COPY .env file to the container

# Expose port
EXPOSE 8080

# Run the application
CMD ["./skills-registry"]