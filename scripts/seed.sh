#!/usr/bin/env bash
# clode-stack/seed.sh — unified one-shot seeder for the local stack.
#
# Compose is the source of truth. This script discovers which services to
# seed by asking `docker compose ps` what's actually running under the
# current project — not from a hardcoded list in this file. Two consequences:
#
#   1. Profile-native. `./stack.sh up --profile skills` brings up
#      skills-registry, this script sees it, seeds it. `./stack.sh up`
#      (base only) leaves it out of scope, this script skips it silently.
#   2. Invocation-independent. `./stack.sh seed` run bare (no --profile
#      flag on the shell) still sees the same containers that are up, so
#      re-seeding after a wipe/config change works either way.
#
# Per-service DB names come from the running container's `DB_NAME` env var
# (docker inspect). To add a new service, set `DB_NAME` on it in compose —
# this script will create + backfill its DB with no code change here.
#
# What each in-scope service gets seeded with:
#   raksha           admin/pool-owner user + default service account
#   cha-ching        llm/cloud tier defaults + credit catalogue
#   jumbo            pool project + application + draft canvas          (needs raksha)
#   pool-manager     kairo svc_configs row(s) from data/*.json
#   skills-registry  admin JWT + skills import + workflow templates     (needs raksha)
#
# Idempotency: SQL uses ON CONFLICT DO NOTHING/UPDATE; skills-registry
# workflow-template POSTs skip on 409; skills import is UPSERT by full_id.
#
# Provider tokens (CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY, OPENAI_API_KEY,
# CODEX_OAUTH_TOKEN+REFRESH) are mang-proxy's concern — set them in
# ../mang-proxy/.env. The stack does not load or forward them.
#
# Run via `stack.sh up` (preferred) or directly after `docker compose up -d`.

set -euo pipefail

cd "$(dirname "$0")/.."

# ── identity ──────────────────────────────────────────────────────────
# Must mirror x-admin-ids in docker-compose.yml exactly.
ADMIN_USER_ID="b2290247-c2af-44c0-9b2d-1e5c5a6a4894"
POOL_PROJECT_ID="e26e56c1-7fd0-458c-a611-584d174651ef"
POOL_APPLICATION_ID="ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c"
# Dedicated identities raksha uses for outbound calls to its upstream
# clients. Each row seeds a raksha users + organizations pair with
# id == same UUID, linked via org_members(owner) — see step 1b/1c below.
# Must match the matching *_ADMIN_ORG_ID / *_ADMIN_USER_ID in
# docker-compose.yml's x-admin-ids anchor (both share the UUID).
NOTIFY_ADMIN_ORG_ID="2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c"
CHACHING_ADMIN_ORG_ID="0d44278f-d900-4b9d-bdc2-a8a64e91d422"

# All HTTP goes through the traefik ingress on host 8080 — *.localhost
# resolves to loopback natively (systemd-resolved / RFC 6761).
RAKSHA_URL="http://raksha.localhost:8080"
SKILLS_URL="http://skills-registry.localhost:8080"
EC2MOCK_URL="http://ec2mock.localhost:8080"

PSQL='docker compose exec -T -e PGPASSWORD=postgres db psql -U postgres -v ON_ERROR_STOP=1'

say() { printf '\n\033[1;36m[seed]\033[0m %s\n' "$*"; }
ok()  { printf '  \033[32m✓\033[0m %s\n' "$*"; }
skip(){ printf '  \033[33m·\033[0m %s\n' "$*"; }
warn(){ printf '  \033[33m!\033[0m %s\n' "$*" >&2; }

# ── discovery ─────────────────────────────────────────────────────────
# Populate three arrays ONCE from the live compose project. Every gate
# below reads from these — no hardcoded service lists, no db_to_svc map.
say "Discovery"

# RUNNING[svc]=1 for every service with a live container. `--status` is a
# stringArray in `docker compose ps` — it MUST be repeated per value, NOT
# comma-separated. Compose v5.x silently returns zero rows for the CSV
# form (matches nothing). Boot-time restart-loopers (waiting on their DB
# to be backfilled below) show up in `restarting`; freshly-`docker
# compose up`'d services may briefly be `created` before their process
# starts. Cleanly-exited one-shots like minio-setup are excluded — they
# don't need seeding.
declare -A RUNNING=()
while IFS= read -r svc; do
  [[ -n "$svc" ]] && RUNNING[$svc]=1
done < <(
  docker compose ps --services \
    --status running \
    --status restarting \
    --status created 2>/dev/null || true
)

if (( ${#RUNNING[@]} == 0 )); then
  warn "no compose services running under this project — nothing to seed"
  exit 0
fi

ok "in scope (${#RUNNING[@]}): $(printf '%s ' "${!RUNNING[@]}")"

is_running() { [[ -n "${RUNNING[$1]:-}" ]]; }
# Alias — used at the seed-step gates so intent reads naturally.
in_scope()   { is_running "$1"; }

# SVC_DB[svc]=<db-name> for every in-scope service whose container has
# DB_NAME set. Reading from `docker inspect` is deliberately authoritative:
# it survives however seed.sh was invoked (up.sh vs bare) and however the
# compose env was merged (anchor merge, env_file, environment: override).
declare -A SVC_DB=()
for svc in "${!RUNNING[@]}"; do
  cid=$(docker compose ps -q "$svc" 2>/dev/null | head -1)
  [[ -z "$cid" ]] && continue
  db=$(docker inspect "$cid" \
         --format '{{range .Config.Env}}{{println .}}{{end}}' 2>/dev/null \
       | awk -F= '$1=="DB_NAME"{print $2; exit}')
  [[ -n "$db" ]] && SVC_DB[$svc]=$db
done

if (( ${#SVC_DB[@]} > 0 )); then
  ok "db-owning services: $(for s in "${!SVC_DB[@]}"; do printf '%s→%s ' "$s" "${SVC_DB[$s]}"; done)"
else
  skip "no db-owning services in scope"
fi

# ── pre-flight ────────────────────────────────────────────────────────
# Wait for the services the later steps actually depend on. Others fall
# out of scope automatically.
say "Pre-flight"

wait_healthy() {
  local name=$1 url=$2 i
  for i in $(seq 1 60); do
    if curl -sf "$url/health" >/dev/null 2>&1; then ok "$name reachable at $url"; return; fi
    sleep 1
  done
  warn "$name not reachable at $url after 60s — continuing anyway"
}
in_scope raksha          && wait_healthy raksha          "$RAKSHA_URL"
in_scope skills-registry && wait_healthy skills-registry "$SKILLS_URL"

# ── 0. ensure every per-service DB exists ─────────────────────────────
# The postgres init script (db/init-multiple-dbs.sql) only runs the first
# time the postgres volume is created; new services (or restoring against
# a stale volume) will crash-loop in `migrate && serve` with "database
# does not exist". This step backfills any missing DB, but ONLY for
# services currently in scope — so removed/profile-gated services don't
# leave dead DBs behind. Owner is `postgres` in both create paths.
say "postgres: ensuring per-service DBs exist for in-scope services"
created_dbs=()
for svc in "${!SVC_DB[@]}"; do
  dbname="${SVC_DB[$svc]}"
  exists=$($PSQL -d postgres -tAc "SELECT 1 FROM pg_database WHERE datname='${dbname}'" | tr -d '[:space:]' || true)
  if [[ "$exists" == "1" ]]; then
    skip "${dbname}: present (${svc})"
  else
    # CREATE DATABASE can't run inside a transaction; the double-quoted
    # identifier is safe for dashes (pool-manager) and underscores alike.
    $PSQL -d postgres -c "CREATE DATABASE \"${dbname}\""
    ok "${dbname}: created (${svc})"
    created_dbs+=("$dbname")
  fi
done

# If any DB was created above, the owning service crash-looped through
# `migrate && serve` while the DB was missing. Bounce it so `migrate`
# runs against the now-present DB before any downstream step talks to
# it. Idempotent — bouncing a healthy container just cycles it briefly.
if (( ${#created_dbs[@]} > 0 )); then
  to_restart=()
  for db in "${created_dbs[@]}"; do
    for svc in "${!SVC_DB[@]}"; do
      if [[ "${SVC_DB[$svc]}" == "$db" ]]; then
        to_restart+=("$svc")
        break
      fi
    done
  done
  if (( ${#to_restart[@]} > 0 )); then
    say "kicking services whose DB was just created: ${to_restart[*]}"
    docker compose restart "${to_restart[@]}" >/dev/null
    # Re-wait for the services later steps depend on.
    for svc in "${to_restart[@]}"; do
      case "$svc" in
        raksha)          wait_healthy raksha          "$RAKSHA_URL" ;;
        skills-registry) wait_healthy skills-registry "$SKILLS_URL" ;;
      esac
    done
  fi
fi

# ── 1. raksha admin user + org + service account ─────────────────────
# Post-slack-native-foundation schema: every resource is org-owned. The
# admin org gets the same UUID as the admin user so pool-owner identity
# is unambiguous — `service_accounts.org_id` FKs to `organizations(id)`
# and `org_members` records the ownership (UNIQUE WHERE role='owner').
if in_scope raksha; then
  say "raksha: admin user + org + default service account"
  $PSQL -d raksha -v owner_id="$ADMIN_USER_ID" <<'SQL'
INSERT INTO users (id, email, name)
VALUES (:'owner_id', 'admin@local', 'Admin / Pool Owner')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name)
VALUES (:'owner_id', 'Admin Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
VALUES (:'owner_id', :'owner_id', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default)
VALUES (:'owner_id', 'pool-manager-default', true)
ON CONFLICT DO NOTHING;
SQL
  ok "raksha seeded"

  # ── 1b. raksha-notify identity ──────────────────────────────────────
  # Separate org so raksha's outbound token to notify (organization_id
  # claim) is attributable in notify's emails.created_by. Same
  # user_id == org_id convention as the admin identity above.
  #
  # Seeded even when notify itself isn't running: raksha's NOTIFY_ADMIN_*
  # env vars are always set (from x-admin-ids), and raksha validates the
  # user exists in `users` at boot — no row = fatal crashloop, regardless
  # of whether the notify service ever comes up.
  say "raksha: raksha-notify user + org + default service account"
  $PSQL -d raksha -v owner_id="$NOTIFY_ADMIN_ORG_ID" <<'SQL'
INSERT INTO users (id, email, name)
VALUES (:'owner_id', 'raksha-notify@local', 'Raksha Notify')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name)
VALUES (:'owner_id', 'Raksha Notify Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
VALUES (:'owner_id', :'owner_id', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default)
VALUES (:'owner_id', 'raksha-notify-default', true)
ON CONFLICT DO NOTHING;
SQL
  ok "raksha-notify identity seeded"

  # ── 1c. raksha-chaching identity ────────────────────────────────────
  # Mirrors 1b for the cha-ching upstream. Same rationale: raksha reads
  # CHACHING_ADMIN_ORG_ID + CHACHING_ADMIN_USER_ID at boot and validates
  # the user row exists (raksha/cmd/main.go:294) — no row = crashloop.
  # Seed unconditionally, since env vars are always set on raksha.
  say "raksha: raksha-chaching user + org + default service account"
  $PSQL -d raksha -v owner_id="$CHACHING_ADMIN_ORG_ID" <<'SQL'
INSERT INTO users (id, email, name)
VALUES (:'owner_id', 'raksha-chaching@local', 'Raksha ChaChing')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name)
VALUES (:'owner_id', 'Raksha ChaChing Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
VALUES (:'owner_id', :'owner_id', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default)
VALUES (:'owner_id', 'raksha-chaching-default', true)
ON CONFLICT DO NOTHING;
SQL
  ok "raksha-chaching identity seeded"

  # ── 1d. intervix OAuth 2.1 client (RFC 7591) ────────────────────────
  # Intervix drives its SPA sign-in through raksha's OAuth 2.1 code+PKCE
  # flow: intervix-web hits intervix's `/api/v1/auth/start`, intervix
  # composes a raksha /authorize URL using INTERVIX_OAUTH_CLIENT_ID +
  # INTERVIX_OAUTH_REDIRECT_URI, and raksha 302s the browser back to
  # the registered redirect_uri after consent. Silent refresh
  # (/api/v1/auth/refresh) also needs the client_id — with none set,
  # intervix short-circuits refresh with 502 (client_id must be
  # configured) and the SPA bounces to /login on the next expiry.
  #
  # Prod uses RFC 7591 Dynamic Client Registration (intervix/scripts/
  # register-oauth-client.sh POST /auth/oauth/register → random
  # client_id). For local dev we skip DCR and pin a known client_id
  # into the row directly so the compose env can hardcode it too. The
  # redirect_uri points at intervix-web's dev server (:3001) at the
  # /auth/callback route the SPA's AppRouter mounts.
  say "raksha: intervix OAuth client (client_id=intervix-local → http://localhost:3001/auth/callback)"
  $PSQL -d raksha <<'SQL'
INSERT INTO oauth_clients (
  client_id,
  client_name,
  redirect_uris,
  grant_types,
  token_endpoint_auth_method,
  client_metadata
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
ON CONFLICT (client_id) DO UPDATE
  SET client_name                = EXCLUDED.client_name,
      redirect_uris              = EXCLUDED.redirect_uris,
      grant_types                = EXCLUDED.grant_types,
      token_endpoint_auth_method = EXCLUDED.token_endpoint_auth_method,
      client_metadata            = EXCLUDED.client_metadata;
SQL
  ok "intervix OAuth client seeded"

  # Kick raksha so it re-boots against the freshly-seeded user rows.
  # Its NOTIFY_ADMIN_USER_ID / CHACHING_ADMIN_USER_ID validation at
  # boot is what was crashlooping the container.
  say "raksha: restarting to pick up freshly-seeded identity rows"
  docker compose restart raksha >/dev/null
  wait_healthy raksha "$RAKSHA_URL"
else
  skip "raksha not in scope — skipping admin/org/service-account seed"
fi

# ── 1d. cha-ching tier defaults + credit catalogue ────────────────────
# Migration-seeded reference data (llm/cloud tier defaults, credit
# products) lives in regular tables, so `cleanup --postgres` wipes it
# while schema_migrations survives and migrations never re-run. Without
# these rows every org intake half-fails: the org_tiers row lands but
# the quota-seed step errors (no defaults row for DEFAULT_TIER), so no
# cap ever reaches mang-proxy/jumbo. Same idempotent SQL the image
# build appends to the last cha-ching migration.
if in_scope cha-ching; then
  say "cha-ching: tier defaults + credit catalogue"
  $PSQL -d chaching < seeds/cha-ching-seed.sql >/dev/null
  ok "cha-ching reference data seeded"
else
  skip "cha-ching not in scope — skipping tier-defaults seed"
fi

# ── 2. jumbo project + application + draft canvas ─────────────────────
# Schema post-mig-000032: polymorphic owner_type was DROPPED, every
# resource is org-owned via a plain org_id column, and `created_by` was
# renamed `created_by_member_id`. For the LOCAL pool project we treat
# the admin user id as its own org id (no real raksha org needed — org_id
# is just a UUID with no FK). Depends on raksha having seeded the admin.
if in_scope jumbo && in_scope raksha; then
  say "jumbo: pool project + application + draft canvas"
  $PSQL -d jumbo \
    -v owner_id="$ADMIN_USER_ID" \
    -v project_id="$POOL_PROJECT_ID" \
    -v application_id="$POOL_APPLICATION_ID" <<'SQL'
INSERT INTO projects (id, org_id, name, slug, created_by_member_id, is_default)
VALUES (:'project_id', :'owner_id', 'Pool Project', 'pool-project', :'owner_id', true)
ON CONFLICT (id) DO NOTHING;

INSERT INTO applications (id, project_id, org_id, name, slug, created_by_member_id)
VALUES (:'application_id', :'project_id', :'owner_id', 'Pool Application', 'pool-application', :'owner_id')
ON CONFLICT (id) DO NOTHING;

INSERT INTO canvas (application_id, org_id, body, is_draft, created_by_member_id, nodes, edges, viewport)
SELECT :'application_id', :'owner_id', '{}'::jsonb, true, :'owner_id',
       '[]'::jsonb, '[]'::jsonb, '{"x": 0, "y": 0, "zoom": 1}'::jsonb
WHERE NOT EXISTS (
  SELECT 1 FROM canvas
  WHERE application_id = :'application_id' AND is_draft = true AND is_deleted = false
);
SQL
  ok "jumbo seeded"
elif in_scope jumbo; then
  skip "jumbo in scope but raksha isn't — skipping (admin identity would be dangling)"
fi

# ── 3. pool-manager svc_configs ───────────────────────────────────────
# settings + vars come straight from production (metabase DB 12 →
# clode.svc_configs WHERE service_type='kairo') and live in
# data/pool-manager-svc-configs.json so the blob is auditable. The file
# holds a `configs` array — one entry per service_type. Iterate + upsert.
#
# Some keys (settings.publicNet, settings.volumeMounts, settings.workspaceSize)
# are k8s-only and pool-manager's DockerDeployer ignores them; they stay
# in the row as-is for parity with prod.
if in_scope pool-manager; then
  KAIRO_JSON=data/pool-manager-svc-configs.json
  # up.sh's --agent flag exports BENJI_IMAGE=clode-stack/benji:<mode>.
  # When set, that value overrides the JSON's settings.image on every row
  # so the pool-manager svc_configs match the actually-built image tag.
  # Unset → JSON default (`clode-stack/benji:vm`) wins.
  KAIRO_JQ_FILTER='.configs[]'
  if [[ -n "${BENJI_IMAGE:-}" ]]; then
    say "pool-manager: overriding svc_configs image with BENJI_IMAGE=${BENJI_IMAGE} (--agent-built)"
    KAIRO_JQ_FILTER='.configs[] | .settings.image = env.BENJI_IMAGE'
  fi
  while IFS= read -r cfg; do
    st=$(jq -r '.service_type'              <<<"$cfg")
    settings_json=$(jq -c '.settings'        <<<"$cfg")
    vars_json=$(jq -c '.vars'                <<<"$cfg")
    hot=$(jq      '.hot_count'                  <<<"$cfg")
    cold=$(jq     '.cold_count'                 <<<"$cfg")
    maxc=$(jq     '.max_concurrent_deployments' <<<"$cfg")
    ena=$(jq      '.enabled'                    <<<"$cfg")
    say "pool-manager: ${st} svc_configs (image=$(jq -r '.settings.image' <<<"$cfg"), hot=${hot}, cold=${cold}, max=${maxc})"
    $PSQL -d "pool-manager" \
      -v st="$st" \
      -v settings="$settings_json" \
      -v vars_json="$vars_json" \
      -v hot="$hot" \
      -v cold="$cold" \
      -v maxc="$maxc" \
      -v ena="$ena" <<'SQL'
INSERT INTO svc_configs (service_type, settings, vars, config_hash, hot_count, cold_count, max_concurrent_deployments, enabled)
VALUES (
  :'st',
  :'settings'::jsonb,
  :'vars_json'::jsonb,
  '',
  :hot, :cold, :maxc, :ena
)
ON CONFLICT (service_type) DO UPDATE
  SET settings = EXCLUDED.settings,
      vars = EXCLUDED.vars,
      hot_count = EXCLUDED.hot_count,
      cold_count = EXCLUDED.cold_count,
      max_concurrent_deployments = EXCLUDED.max_concurrent_deployments,
      enabled = EXCLUDED.enabled,
      updated_at = now();
SQL
  done < <(BENJI_IMAGE="${BENJI_IMAGE:-}" jq -c "$KAIRO_JQ_FILTER" "$KAIRO_JSON")
  ok "pool-manager seeded"
else
  skip "pool-manager not in scope — skipping svc_configs seed"
fi

# ── 3b. ec2mock default image ────────────────────────────────────────
# Push the kairo image from data/pool-manager-svc-configs.json to the
# mock's admin API. Same source of truth as pool-manager's svc_configs
# above — the two stay in lockstep because both derive from the same
# jq path. The mock uses this as its RunInstances default: brahmi's
# aramb-vm path sends a placeholder AGENT_VM_AMI_ID and the mock
# substitutes the real docker image at launch. Gated on ec2mock being
# in scope, so a stack running without it just skips this step.
if in_scope ec2mock; then
  KAIRO_JSON=${KAIRO_JSON:-data/pool-manager-svc-configs.json}
  KAIRO_IMG=$(jq -r '.configs[] | select(.service_type=="kairo") | .settings.image' "$KAIRO_JSON")
  if [[ -z "$KAIRO_IMG" || "$KAIRO_IMG" == "null" ]]; then
    warn "no kairo image found in $KAIRO_JSON — ec2mock default_image not seeded"
  else
    say "ec2mock: pushing default_image=${KAIRO_IMG}"
    http=$(curl -s -o /tmp/ec2mock.$$ -w '%{http_code}' \
      -X PUT "${EC2MOCK_URL}/_admin/config/default-image" \
      -H "Content-Type: application/json" \
      -d "{\"image\":\"${KAIRO_IMG}\"}")
    case "$http" in
      200|204) ok "ec2mock default_image=${KAIRO_IMG}" ;;
      *) warn "ec2mock PUT default-image: HTTP ${http} — $(cat /tmp/ec2mock.$$ 2>/dev/null)" ;;
    esac
    rm -f /tmp/ec2mock.$$
  fi
else
  skip "ec2mock not in scope — skipping default_image seed"
fi

# ── 4. skills-registry — admin JWT + import + workflow templates ──────
# Gate on both self and raksha: JWT is minted from raksha's dev endpoint,
# and workflow-template POSTs are authenticated with that JWT. Without
# raksha there's no auth path here.
if in_scope skills-registry && in_scope raksha; then
  say "skills-registry: minting admin JWT from raksha"
  ADMIN_JWT=$(curl -s "${RAKSHA_URL}/generate-dev-jwt-access-token?sub=${ADMIN_USER_ID}" | jq -r '.OK.Jwt')
  if [[ -z "${ADMIN_JWT:-}" || "$ADMIN_JWT" == "null" ]]; then
    warn "could not mint admin JWT from raksha — skills-registry seed skipped"
  else
    ok "JWT minted (sub=${ADMIN_USER_ID})"

    # 4a. skills — parse SKILL.md files under ../aramb-skills locally and
    # UPSERT repos + skills + skill_versions directly into skills_registry.
    # Bypasses /api/v1/me/import (which walks GitHub anonymously and gets
    # rate-limited at 60 req/hr). Idempotent via ON CONFLICT DO UPDATE.
    # Column overrides for category/tags/etc. live in data/skill-overrides.json.
    say "skills-registry: seeding skills from ../aramb-skills (direct DB)"
    if [[ ! -d ../aramb-skills ]]; then
      warn "../aramb-skills not found — skipping skills seed"
    elif ! command -v python3 >/dev/null; then
      warn "python3 not on PATH — skipping skills seed"
    else
      sk_branch=$(git -C ../aramb-skills rev-parse --abbrev-ref HEAD 2>/dev/null || echo "?")
      sk_commit=$(git -C ../aramb-skills rev-parse --short HEAD 2>/dev/null || echo "?")
      if [[ "$sk_branch" != "main" ]]; then
        warn "../aramb-skills is on '${sk_branch}', not main — seeding HEAD=${sk_commit} as-is"
      fi
      if ADMIN_USER_ID="$ADMIN_USER_ID" ./scripts/seed-skills-from-local.py \
           | $PSQL -d skills_registry > /tmp/sk-seed.$$ 2>&1; then
        # `|| true` keeps a no-match grep from tripping `set -e` inside $(...).
        summary=$(grep -E "^-- summary" /tmp/sk-seed.$$ | head -1 || true)
        ok "${summary:-seeded skills from ../aramb-skills @${sk_commit}}"
      else
        warn "skills seed failed — output below"
        tail -30 /tmp/sk-seed.$$ | sed 's/^/    /'
      fi
      rm -f /tmp/sk-seed.$$
    fi

    # 4b. workflow templates — POST each entry from data/workflow-templates.json.
    # 409 means slug already exists (idempotent skip); anything else is logged.
    say "skills-registry: workflow templates (20 entries)"
    total=$(jq 'length' data/workflow-templates.json)
    created=0; skipped=0; failed=0
    for i in $(seq 0 $((total - 1))); do
      slug=$(jq -r ".[$i].slug" data/workflow-templates.json)
      payload=$(jq ".[$i]" data/workflow-templates.json)
      http=$(curl -s -o /tmp/wt.$$.body -w '%{http_code}' \
        -X POST "${SKILLS_URL}/api/v1/workflow-templates" \
        -H "Authorization: Bearer ${ADMIN_JWT}" \
        -H "Content-Type: application/json" \
        -d "$payload")
      case "$http" in
        201|200) ok    "${slug}: created"; created=$((created+1)) ;;
        409)     skip  "${slug}: exists";  skipped=$((skipped+1)) ;;
        *)       warn "${slug}: HTTP ${http} — $(jq -c . /tmp/wt.$$.body 2>/dev/null || cat /tmp/wt.$$.body)"
                 failed=$((failed+1)) ;;
      esac
    done
    rm -f /tmp/wt.$$.body
    printf '\n  workflow templates: created=%d skipped=%d failed=%d\n' "$created" "$skipped" "$failed"
  fi
elif in_scope skills-registry; then
  skip "skills-registry in scope but raksha isn't — skipping (no JWT auth path)"
fi

# ── 5. nudge restart-loopers ──────────────────────────────────────────
# Services that depend on a seeded raksha row (pool-manager) or a freshly
# backfilled DB (skills-registry) may still be restart-looping from boot.
# They'd recover on their own via restart: unless-stopped, but a nudge
# shaves ~30s off "stack is ready". Only kick services that are actually
# in scope — no dangling references.
kick=()
in_scope pool-manager    && kick+=(pool-manager)
in_scope skills-registry && kick+=(skills-registry)
if (( ${#kick[@]} > 0 )); then
  say "post-seed: kick any restart-loopers (${kick[*]})"
  docker compose restart "${kick[@]}" >/dev/null
  ok "restart issued"
fi

say "done"
