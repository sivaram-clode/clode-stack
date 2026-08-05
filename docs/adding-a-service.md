# Adding a new service to clode-stack

Walk-through for wiring a sibling repo (`../<svc>`) into the local stack.
Assumes the service is a Glafa-shaped Go binary that takes `migrate` and
`serve` subcommands, listens on internal port `8080`, and has a sibling
`.env` (`cp .env.example .env` first).

Replace `<svc>` and `<db>` (postgres DB name) below. There is no
host-published port to pick — HTTP routes through the traefik ingress at
`http://<svc>.localhost:8080`, declared by labels on the service itself.

## Guiding principle — convention over editing scripts

Two runtime conventions mean a new service almost never touches a script:

- **`DB_NAME`** in the compose block: the seeder (`scripts/seed.py`) reads live
  compose state — it iterates `docker compose ps` and picks up each service's
  postgres DB from its container's `DB_NAME` — so it discovers and backfills your
  DB with no edit.
- **`seeds/<svc>-seed.sql`** (optional): drop this file and the seed is applied
  two ways automatically — `gen-build-cache` appends it onto your service's last
  migration (so `<svc> migrate` seeds a fresh DB, including a `wfork` clone), and
  `seed.py`'s uniform loop re-applies it as the reseed backstop. No script edit.

Only step 5 touches `seed.py`, and only for *dynamic* seeds (a value resolved at
up-time — e.g. a locally-built image tag — or a post-boot API call) that can't be
baked into a static SQL file.

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

## 3. Database + seed (usually no script edit)

- `db/init-multiple-dbs.sql` — `CREATE DATABASE <db>;` (runs only on first
  postgres-volume creation). On an existing volume `seed.py` backfills a missing
  `<db>` from the running container's `DB_NAME` and bounces the service, so it
  Just Works either way.
- **Seed rows?** Drop `seeds/<svc>-seed.sql` (idempotent SQL, `ON CONFLICT`). It
  is embedded onto your last migration (so `<svc> migrate` seeds a fresh DB and a
  `wfork` clone) *and* applied by `seed.py`'s uniform loop as the reseed backstop
  — no `seed.py` edit. Keep it single-DB and self-contained (literal values, not
  cross-DB references), so order and isolation never matter.

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

## 5. Custom seed logic (only for DYNAMIC seeds)

Static seed rows go in `seeds/<svc>-seed.sql` (step 3) — no `seed.py` edit. Touch
`seed.py` **only** when the seed can't be a static file:

- a value resolved at up-time (e.g. a locally-built image tag — see the
  pool-manager `svc_configs` step), or
- a post-boot API call / filesystem import (e.g. skills-registry importing
  `../aramb-skills`).

Add one block in `seed.py`'s `main()`, gated with `in_scope`:

```python
if in_scope("<svc>"):                     # + and in_scope("<dep>") if you call one
    say("<svc>: <what this seeds>")
    # s.psql(SVC_DB["<svc>"], sql=...) for DB writes; s.http(...) for APIs;
    # RAKSHA_URL for auth JWTs
    ok("<svc> seeded")
```

Rules:

- **Self-gate** (`in_scope("<svc>")`) is mandatory — runtime discovery keeps the
  seeder profile-native.
- **Dep-gate** (`and in_scope("<dep>")`) for any service you call (mint a JWT off
  raksha → gate on raksha), so profile combinations stay honest.
- If the step hits an API, add a `wait_healthy(...)` in the pre-flight block,
  also gated on `in_scope`.
- **Make it fork-safe** when it matters: expose the logic as a function + a
  `seed.py <mode> <db>` CLI so `wfork` can run it against a fork's fresh DB — see
  `seed_pool_manager_svc_configs` + the `svc-configs` CLI (wfork calls it for a
  forked pool-manager).

Patterns to crib: the pool-manager `svc_configs` step (dynamic image tag,
fork-aware) and the skills-registry import (filesystem).

## 6. Bring up

```bash
./stack.sh up <svc>            # partial; seeder skipped (per stack.sh policy)
./stack.sh up                  # full bring-up; runs seed against live containers
./stack.sh up --profile <name> # if <svc> was assigned a profile
```

Seed is skipped on partial bring-ups; if you added custom seed logic, run
`./stack.sh seed` once the full stack is healthy to fire it manually.
