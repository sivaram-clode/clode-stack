# Build stage
FROM golang:1.25-alpine AS builder

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

# Build brahmi (always required)
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -o brahmi ./cmd/brahmi

# Build kairo (independent — failure here doesn't block brahmi)
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -o kairo ./cmd/kairo || true

# Final stage
FROM alpine:latest

WORKDIR /app

# Copy binaries from builder
COPY --from=builder /app/brahmi /app/brahmi
COPY --from=builder /app/kairo* /app/
# NEVER COPY .env file to the container

# Expose ports
EXPOSE 8080

# Run the application
CMD ["./brahmi"]
