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

# Build the application with SSH agent forwarding
# clode-stack: append local seed onto the last migration so `migrate`
# itself seeds a fresh database (source: clode-stack/seeds/cha-ching-seed.sql)
RUN cat >> "$(ls internal/db/migrations/*.up.sql | sort | tail -1)" <<'CLODE_SEED'

-- clode-stack local seed for cha-ching — appended by gen-build-cache.sh onto
-- the LAST embedded migration file at image build time, so `cha-ching migrate`
-- itself plants these rows on a fresh database. Unlike raksha's identities
-- these rows are not boot-fatal, but without them every org intake fails its
-- quota-seed step (org_tiers row lands, org_llm/cloud_quotas never do) and
-- the billing catalogue endpoint serves an empty pricing page.
--
-- The rows mirror what upstream migrations already seed (0003 llm defaults,
-- 0006 cloud defaults, 0008+0011 credit catalogue). They exist here because
-- `cleanup --postgres` truncates every non-migration table while KEEPING
-- schema_migrations — migrations never re-run, so the migration-seeded
-- reference data is lost until something re-applies it. This file is that
-- something (cleanup.sh pipes it right after truncating; seed.sh is the
-- idempotent backstop).
--
-- Rules for this file:
--   * idempotent statements only (ON CONFLICT DO NOTHING) — it may ride
--     along whichever migration is last at any point in time;
--   * values must stay in sync with the upstream migration seeds — when a
--     cha-ching migration changes a default/price, mirror it here;
--   * never write a line containing only CLODE_SEED (heredoc delimiter).

-- Per-tier LLM quota defaults (migration 0003): tokens per 30d window.
INSERT INTO llm_tier_defaults (tier, token_max_value, token_period_seconds) VALUES
    ('hobbyist',     1500000,  2592000),
    ('solopreneur',  5000000,  2592000),
    ('enterprise',  50000000,  2592000)
ON CONFLICT (tier) DO NOTHING;

-- Per-tier cloud quota defaults (migration 0006).
INSERT INTO cloud_tier_defaults (tier, max_cpu_cores, max_memory_gb) VALUES
    ('hobbyist',     8.0,   20.0),
    ('solopreneur',  24.0,  60.0),
    ('enterprise',  80.0, 200.0)
ON CONFLICT (tier) DO NOTHING;

-- Credit catalogue (migrations 0008 + 0011): the 5 TEST-mode Stripe prices
-- with their display amounts. Plans are monthly; top-ups one-time.
INSERT INTO credit_products (stripe_price_id, kind, credits, unit_amount_cents, currency, interval) VALUES
    ('price_1TlZXXEEIsDh2nkstLOXva68', 'plan',   8000,  1900, 'usd', 'month'),
    ('price_1TlZXYEEIsDh2nksDxSa9Ro8', 'plan',  25000,  4900, 'usd', 'month'),
    ('price_1TlZXaEEIsDh2nksvIbIy52T', 'topup', 20000,  4500, 'usd', NULL),
    ('price_1TlZXbEEIsDh2nkskPKxEJEB', 'topup', 40000,  8500, 'usd', NULL),
    ('price_1TlZXdEEIsDh2nksuBsTrcpM', 'topup', 60000, 11900, 'usd', NULL)
ON CONFLICT (stripe_price_id) DO NOTHING;
CLODE_SEED

RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked --mount=type=ssh \
    CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/main.go

# Final stage
FROM alpine:latest

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/main /app/cha-ching
# NEVER COPY .env file to the container

# Expose port
EXPOSE 8080

# Run the application
CMD ["./cha-ching"]