# Adding a new service to clode-stack

Walk-through for wiring a sibling repo (`../<svc>`) into the local stack.
Assumes the service is a Glafa-shaped Go binary that takes `migrate` and
`serve` subcommands, listens on internal port `8080`, and has a sibling
`.env` (`cp .env.example .env` first).

Replace `<svc>` and `<db>` (postgres DB name) below. There is no
host-published port to pick — HTTP routes through the traefik ingress at
`http://<svc>.localhost:8080`, declared by labels on the service itself.

## Guiding principle — compose is the source of truth

The seeder (`scripts/seed.sh`) reads live compose state to decide what to
seed: it iterates services from `docker compose ps` and picks up each
service's postgres DB from its container's `DB_NAME` env var. That means
**for the vast majority of new services, seed.sh does not need to change**
— setting `DB_NAME` in the compose block below is enough. Only step 5
(custom seed logic) touches seed.sh, and only for services that need
post-boot API/DB seeding beyond DB creation itself.

## 1. Compose service block

Add under `services:` in `docker-compose.yml`. Copy the `chil` block as a
template — it pulls every shared anchor:

```yaml
<svc>:
  build:
    context: ../<svc>
    ssh: [default]
  command: ["sh", "-c", "/app/<svc> migrate && exec /app/<svc> serve"]
  env_file: [../<svc>/.env]
  environment:
    <<: [*db-common, *service-urls, *allowed-origins]   # + *admin-ids if needed
    ENVIRONMENT: local                                  # Glafa secret-loader requirement
    DB_NAME: <db>                                       # load-bearing — seeder reads this
    PORT: 8080
  labels:
    - traefik.enable=true
    # include the srclode Host ONLY if the service should be reachable in
    # `--public` mode; internal/admin-only services get `.localhost` alone.
    - traefik.http.routers.<svc>.rule=Host(`<svc>.localhost`) || Host(`<svc>.srclode.online`)
    - traefik.http.services.<svc>.loadbalancer.server.port=8080
  depends_on:
    db: { condition: service_healthy }
    raksha: { condition: service_healthy }
  networks: [clode]
  healthcheck:
    test: ["CMD-SHELL", "wget --no-verbose --tries=1 --spider http://127.0.0.1:8080/health || exit 1"]
    interval: 60s
    timeout: 5s
    retries: 5
    start_period: 30s
  restart: unless-stopped
```

If the service belongs behind an opt-in profile, add `profiles: [<name>]`
just after `<svc>:` — the seeder's runtime discovery picks that up
automatically.

## 2. Cross-service URL (only if other services dial this one)

In `docker-compose.yml`, add to the `x-service-urls:` anchor:

```yaml
<SVC>_URL: http://<svc>:8080
```

Match whatever key name the consumer service's config reads (`_BASE_URL`,
`_EXTERNAL_URL`, etc.). The anchor is merged into every consumer's
`environment:`, so this overrides any localhost URL their own `.env` has.

## 3. Database

- `db/init-multiple-dbs.sql` — `CREATE DATABASE <db>;` (runs only on
  first postgres-volume creation, i.e. only for someone bringing up the
  stack from a clean slate).
- `scripts/seed.sh` — **no change required.** The seeder discovers
  `<db>` from the running container's `DB_NAME` env, backfills it if
  postgres doesn't already have it, and bounces the service so its
  `migrate && serve` completes. Adding a new service on an existing
  postgres volume Just Works.

## 4. Public hostname (nothing to do)

Routing is entirely label-driven. If the router rule in step 1 includes
`Host(`<svc>.srclode.online`)`, the service is public whenever the stack
runs with `--public` — cloudflared's config is a single catch-all
(`*.srclode.online → traefik:8080`) and never needs editing; DNS is
covered by the `*.srclode.online` wildcard CNAME.

Two extras only when they apply:

- **Containers dial the public/`.localhost` URL** (S3 signing, OAuth
  token POSTs, agent callbacks): add the hostname(s) to the traefik
  service's `networks.clode.aliases` list so in-network callers resolve
  them.
- **Outward-facing URL env values** should interpolate so `--public`
  flips them:
  `${STACK_SCHEME:-http}://<svc>.${STACK_DOMAIN:-localhost}${STACK_PORT-:8080}`

## 5. Custom seed logic (only if the service needs post-boot API/DB setup)

If DB creation is enough, skip this step. Otherwise append one block to
`scripts/seed.sh`, gated with `in_scope`:

```bash
# ── N. <svc> — post-boot setup ────────────────────────────────────────
if in_scope <svc>; then                              # + && in_scope <dep> if any
  say "<svc>: <what this seeds>"
  # SQL / curl / whatever; use $PSQL for DB writes, $RAKSHA_URL for auth JWTs
  ok "<svc> seeded"
fi
```

Rules for the gate:

- **Self-gate** (`in_scope <svc>`) is mandatory. Runtime discovery is what
  makes the seeder profile-native.
- **Dep-gate** (`&& in_scope <dep>`) for any external service you call. If
  your step mints a JWT off raksha, gate on `in_scope raksha`. If it
  requires jumbo to already have a project row, gate on `in_scope jumbo`.
  Keeps profile combinations honest and prevents half-run state.
- Add a `wait_healthy <svc> "$<SVC>_URL"` call in the pre-flight block,
  also gated on `in_scope`, only if the seed step hits its API.

Existing patterns to crib from: the raksha admin-user block, the jumbo
project/canvas block, the skills-registry JWT-mint-then-import block.

## 6. Bring up

```bash
./stack.sh up <svc>            # partial; seeder skipped (per stack.sh policy)
./stack.sh up                  # full bring-up; runs seed against live containers
./stack.sh up --profile <name> # if <svc> was assigned a profile
```

Seed is skipped on partial bring-ups; if you added custom seed logic, run
`./stack.sh seed` once the full stack is healthy to fire it manually.
