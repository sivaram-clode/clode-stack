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

# Build the application with SSH agent forwarding
# clode-stack: append local seed onto the last migration so `migrate`
# itself seeds a fresh database (source: clode-stack/seeds/jumbo-seed.sql)
RUN cat >> "$(ls internal/db/migrations/*.up.sql | sort | tail -1)" <<'CLODE_SEED'

-- clode-stack local seed for jumbo: the pool project + application + draft
-- canvas the local pool uses. owner id == org id for the local pool (org_id is
-- a plain UUID, no FK); the UUIDs are the x-admin-ids constants in
-- docker-compose.yml — keep them in sync if that anchor rotates. Idempotent.
-- gen-build-cache appends this onto jumbo's last migration, so `jumbo migrate`
-- seeds a fresh DB itself (baseline + fresh forks alike).

INSERT INTO projects (id, org_id, name, slug, created_by_member_id, is_default)
VALUES ('e26e56c1-7fd0-458c-a611-584d174651ef', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894',
        'Pool Project', 'pool-project', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894', true)
ON CONFLICT (id) DO NOTHING;

INSERT INTO applications (id, project_id, org_id, name, slug, created_by_member_id)
VALUES ('ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c', 'e26e56c1-7fd0-458c-a611-584d174651ef',
        'b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'Pool Application', 'pool-application',
        'b2290247-c2af-44c0-9b2d-1e5c5a6a4894')
ON CONFLICT (id) DO NOTHING;

INSERT INTO canvas (application_id, org_id, body, is_draft, created_by_member_id, nodes, edges, viewport)
SELECT 'ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894',
       '{}'::jsonb, true, 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894',
       '[]'::jsonb, '[]'::jsonb, '{"x": 0, "y": 0, "zoom": 1}'::jsonb
WHERE NOT EXISTS (
  SELECT 1 FROM canvas
  WHERE application_id = 'ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c' AND is_draft = true AND is_deleted = false
);
CLODE_SEED

RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/main.go

# Final stage
FROM alpine:latest

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/main /app/jumbo
# NEVER COPY .env file to the container

# Expose port
EXPOSE 8080

# Run the application
CMD ["./jumbo"]