# Build stage
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Copy go mod and sum files
COPY go.mod ./

# Download dependencies
RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked go mod download

# Copy source code
COPY . .

# Build the application
# clode-stack: append local seed onto the last migration so `migrate`
# itself seeds a fresh database (source: clode-stack/seeds/raksha-seed.sql)
RUN cat >> "$(ls db/migrations/*.up.sql | sort | tail -1)" <<'CLODE_SEED'

-- clode-stack local seed for raksha — appended by gen-build-cache.sh onto
-- the LAST embedded migration file at image build time, so `raksha migrate`
-- itself plants these rows on a fresh database BEFORE `serve` runs its
-- boot validation (missing NOTIFY/CHACHING admin identities = fatal
-- crashloop — the old fresh-stack deadlock).
--
-- Rules for this file:
--   * idempotent statements only (ON CONFLICT DO NOTHING) — it may ride
--     along whichever migration is last at any point in time;
--   * UUIDs must mirror docker-compose.yml's x-admin-ids anchor (migrations
--     cannot read env vars);
--   * never write a line containing only CLODE_SEED (heredoc delimiter).
--
-- Databases that already applied the last migration don't re-run it —
-- they were seeded by scripts/seed.sh, which stays the idempotent backstop
-- for cleanup/reseed flows.

-- Admin/pool-owner + bot identities raksha mints outbound SA tokens for.
-- Convention: USER_ID == ORG_ID per identity, linked via org_members(owner).
INSERT INTO users (id, email, name) VALUES
  ('b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'admin@local',           'Admin / Pool Owner'),
  ('2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c', 'raksha-notify@local',   'Raksha Notify'),
  ('0d44278f-d900-4b9d-bdc2-a8a64e91d422', 'raksha-chaching@local', 'Raksha ChaChing')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name) VALUES
  ('b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'Admin Org'),
  ('2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c', 'Raksha Notify Org'),
  ('0d44278f-d900-4b9d-bdc2-a8a64e91d422', 'Raksha ChaChing Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role) VALUES
  ('b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'owner'),
  ('2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c', '2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c', 'owner'),
  ('0d44278f-d900-4b9d-bdc2-a8a64e91d422', '0d44278f-d900-4b9d-bdc2-a8a64e91d422', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default) VALUES
  ('b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'pool-manager-default',    true),
  ('2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c', 'raksha-notify-default',   true),
  ('0d44278f-d900-4b9d-bdc2-a8a64e91d422', 'raksha-chaching-default', true)
ON CONFLICT DO NOTHING;

-- intervix OAuth 2.1 client (code+PKCE). Prod uses RFC 7591 dynamic
-- registration; locally a known client_id is pinned so the compose env can
-- hardcode INTERVIX_OAUTH_CLIENT_ID. redirect_uri targets intervix-web's
-- dev server (:3001) at the SPA's /auth/callback route.
INSERT INTO oauth_clients (
  client_id, client_name, redirect_uris, grant_types,
  token_endpoint_auth_method, client_metadata
)
VALUES (
  'intervix-local',
  'intervix (local)',
  ARRAY['http://localhost:3001/auth/callback']::text[],
  ARRAY['authorization_code','refresh_token']::text[],
  'none',
  jsonb_build_object(
    'client_uri', 'http://localhost:3001',
    'scope',      'aramb',
    'contacts',   jsonb_build_array('ops@clode.space')
  )
)
ON CONFLICT (client_id) DO NOTHING;
CLODE_SEED

RUN --mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked --mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked CGO_ENABLED=0 GOOS=linux go build -o main ./cmd/main.go

# Final stage
FROM alpine:latest

WORKDIR /app

# Copy the binary from builder
COPY --from=builder /app/main /app/raksha
# NEVER COPY .env file to the container

# Expose port
EXPOSE 8080

# Run the application
CMD ["./raksha"]