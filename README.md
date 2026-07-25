# clode-stack

Local docker-compose for the whole clode platform. Service sources are sibling
directories (`../raksha`, `../jumbo`, …); this folder is the only place
compose, env, and seed data live.

## Quick start (≤5 min)

```bash
# 1. Make sure the prereqs are installed (see "Prereqs" below).
# 2. Clone every required sibling repo into the same parent dir as clode-stack.
# 3. Drop your Cloudflare tunnel creds into ~/.cloudflared/  (see "Cloudflared" below).
# 4. Put provider tokens in ../mang-proxy/.env  (CLAUDE_CODE_OAUTH_TOKEN, etc.).
# 5. Generate raksha's JWT signing keys (see "Raksha JWT keys" below).
mkdir -p keys/raksha
openssl genrsa -out keys/raksha/raksha-private.pem 2048
openssl rsa -in keys/raksha/raksha-private.pem -pubout -out keys/raksha/raksha.pub
# 6. Bring it up.
./stack.sh up
```

That's it. `up` runs build → start → seed and is idempotent — re-running
re-confirms healthy and re-seeds. After `==> stack ready`, every service is
reachable on its host port (see "Services" below).

## Prereqs

| Tool | Why | Install |
|---|---|---|
| Docker Engine + Compose v2 | Runs everything. Buildkit required. | https://docs.docker.com/engine/install/ |
| `jq` | Parses compose JSON in `up.sh` / `seed.sh`. | `apt install jq` |
| `python3` | `seed-skills-from-local.py` emits the skills seed SQL. | usually preinstalled |
| `git` + working `ssh-add` (`ssh: [default]` in builds) | BuildKit forwards your SSH agent so private Go modules resolve during build. | `eval $(ssh-agent) && ssh-add ~/.ssh/id_*` |
| `bash` + `bash-completion` | Tab-completion for `stack` is shell-level. | macOS: `brew install bash-completion`; Debian: `apt install bash-completion` |
| `~/.cloudflared/cert.pem` + tunnel JSON | Cloudflared tunnel needs creds. | See [docs/cloudflared-setup.md](./docs/cloudflared-setup.md) |

## Folder layout it expects

Every service's source must live as a sibling of `clode-stack/`:

```
parent/
├── clode-stack/       ← you are here
├── raksha/
├── jumbo/
├── brahmi/
├── pool-manager/
├── chil/
├── cha-ching/
├── toolkit-proxy/
├── mang-proxy/
├── skills-registry/
├── gitana/
├── intervix/
├── vova/
├── ikki/
├── benji/             ← kairo agent image is built from here
└── narnia/, narnia-workers/   ← only needed for the `deploy` profile
```

Each repo must carry its own `.env` (or `.env.example` for cha-ching). The
compose layers stack-topology overrides on top.

## Raksha JWT keys

raksha signs session JWTs with an RSA keypair mounted read-only at
`keys/raksha/` (→ `/app/keys` in the container, per `../raksha/.env`'s
`JWT_PRIVATE_KEY_PATH` / `JWT_PUBLIC_KEY_PATH`). These are **not** committed
(`keys/` is gitignored), so generate them once per checkout before the first
`up`:

```bash
mkdir -p keys/raksha
# Private key — PKCS#1 ("BEGIN RSA PRIVATE KEY"), what raksha's loader expects.
openssl genrsa -out keys/raksha/raksha-private.pem 2048
# Public key — PKIX ("BEGIN PUBLIC KEY").
openssl rsa -in keys/raksha/raksha-private.pem -pubout -out keys/raksha/raksha.pub
```

The pair survives `./stack.sh wipe` and `cleanup` (host-side, mounted `:ro`) —
raksha reuses the same `kid`/JWKS across restarts and only regenerates when a
file is missing. Deleting them and re-running the commands rotates the keys
(invalidates every existing token).

## Services

Default `./stack.sh up` brings up the unprofiled set. Profiled services are
opt-in via `--profile`.

| Service | Profile | Host URL | Source |
|---|---|---|---|
| postgres | — | `localhost:15432` | — |
| redis | — | `localhost:16379` (`requirepass clode-redis-local`) | — |
| minio | — | S3 `http://localhost:19000`, console `http://localhost:19001` (`minioadmin`/`minioadmin`) | — |
| databend | — | internal only | — |
| raksha | — | http://localhost:8081 | `../raksha` |
| jumbo | — | http://localhost:8082 | `../jumbo` |
| brahmi | — | http://localhost:9000 | `../brahmi` |
| pool-manager | `pool` | http://localhost:8083 | `../pool-manager` |
| cha-ching | — | http://localhost:8086 | `../cha-ching` |
| mang-proxy | — | http://localhost:8090 | `../mang-proxy` |
| skills-registry | `skills` | http://localhost:8087 | `../skills-registry` |
| chil | `org` | http://localhost:8084 | `../chil` |
| toolkit-proxy | `tools` | http://localhost:8085 | `../toolkit-proxy` |
| gitana | `tools` | http://localhost:8088 | `../gitana` |
| intervix | `voice` | http://localhost:8092 | `../intervix` |
| vova | `voice` | http://localhost:8093 | `../vova` |
| ikki | `browser` | http://localhost:8089 | `../ikki` |
| narnia | `deploy` | http://localhost:8091 | `../narnia` |
| narnia-workers | `deploy` | — (worker only) | `../narnia-workers` |
| cloudflared | — | exposes services at `*.srclode.online` | — |

Every service is also reachable on its public Cloudflared hostname
(`<service>.srclode.online`) via the tunnel ingress in
[cloudflared-config.yml](./cloudflared-config.yml).

## Profiles

```bash
./stack.sh up                                  # base stack only
./stack.sh up --profile voice                  # base + intervix + vova
./stack.sh up --profile browser,tools          # CSV — base + ikki + toolkit-proxy + gitana
./stack.sh up --profile voice --profile org    # repeatable — base + voice + chil
./stack.sh up --profile deploy                 # base + narnia + narnia-workers
```

Profile names tab-complete after `--profile`/`--profile=`.

| Profile | Services |
|---|---|
| `org` | chil |
| `tools` | toolkit-proxy, gitana |
| `voice` | intervix, vova |
| `browser` | ikki |
| `skills` | skills-registry |
| `deploy` | narnia, narnia-workers |
| `email` | notify |
| `pool` | pool-manager |

## Seeder

`scripts/seed.sh` is compose-driven — every run it queries
`docker compose ps` for what's actually up and derives each service's
postgres DB from the running container's `DB_NAME` env. Consequences:

- **Profile-native.** `./stack.sh up --profile skills` brings up
  skills-registry, the seeder sees it and seeds skills + workflow
  templates. `./stack.sh up` on its own leaves it out of scope; the
  seeder skips it silently — no warning, no error.
- **Invocation-independent.** `./stack.sh seed` invoked bare (no
  `--profile` flag on the shell) sees the same containers that are up,
  so re-seeding after `wipe`/config changes works whether or not the
  original profile flags are re-passed.
- **Adding a service auto-onboards it into the seeder.** As long as the
  new service has `DB_NAME` in its compose `environment:`, the seeder
  creates its DB, bounces it so `migrate && serve` completes, and moves
  on — no seed.sh edit. Full walk-through in
  [docs/adding-a-service.md](./docs/adding-a-service.md).

If a step's payload is custom (raksha admin row, jumbo project row,
skills import) it stays hand-written in seed.sh under
`if in_scope <svc>; then …; fi` gates. Cross-step deps (e.g. skills →
raksha for JWT mint) are explicit: `if in_scope skills-registry && in_scope raksha`.

## Commands

| Command | What it does |
|---|---|
| `./stack.sh up [svc...]` | Build (batched — default 2 parallel, `--batch N` up to 6) + `compose up -d` + tail logs + seed. With a service subset, seeder is skipped. |
| `./stack.sh down` | `compose down`; preserves volumes. |
| `./stack.sh wipe [--yes\|-y]` | `compose down -v` + drop images + prune BuildKit cache + `docker rm -f` kairo-pmlocal-* agents. Prompts unless `-y`. |
| `./stack.sh seed` | Re-run the idempotent post-boot seeder. |
| `./stack.sh cleanup [flags]` | Truncate data in place without dropping volumes. See `./stack.sh cleanup -h` for the full source/modifier matrix (`--postgres`, `--redis`, `--redis-mang`, `--databend`, `--pmlocal`, `--reseed`, `--dry-run`). |
| `./stack.sh tail-logs [svc...]` | (Re-)start per-service log tailers into `./logs/service/`. |
| `./stack.sh build-cache` | Regenerate the cache-mount Dockerfiles + overlay. |

Optional knobs:

- `BUILD_BATCH_SIZE=N ./stack.sh up` — same as `--batch N`. Default 2, max 6.
- Provider tokens (`CLAUDE_CODE_OAUTH_TOKEN`, `ANTHROPIC_API_KEY`,
  `OPENAI_API_KEY`, `CODEX_OAUTH_TOKEN`+`CODEX_OAUTH_REFRESH_TOKEN`) live in
  `../mang-proxy/.env`. The stack does not load or forward them.

## Tab completion

Source the completion once from your shell rc:

```bash
echo "source $(pwd)/scripts/_stack-completion.bash" >> ~/.bashrc
```

Then `stack <Tab>` lists subcommands, `stack up <Tab>` lists services parsed
live from `docker-compose.yml`, and `stack up --profile <Tab>` lists profiles.

## Cloudflared

The `cloudflared` service publishes every backend at `<svc>.srclode.online`
via Cloudflare Tunnel. First-time setup, rotation, and the wildcard-DNS
recovery procedure live in [docs/cloudflared-setup.md](./docs/cloudflared-setup.md).

## Adding a new service

Walk-through in [docs/adding-a-service.md](./docs/adding-a-service.md) —
covers the compose entry, env wiring, postgres DB creation, healthcheck,
cloudflared ingress, and (only if the service needs custom post-boot
setup beyond DB creation) a seed hook. The guiding principle is that
setting `DB_NAME` in the compose block is enough for the seeder to pick
up a new service without any script edit.

## Gotchas

The full operator notes (postgres-init-first-boot-only, BuildKit cache
pruning blast radius, the cloudflared Accept-Encoding SigV4 quirk, …)
live in [CLAUDE.md](./CLAUDE.md#gotchas). Read them before debugging an
unexpected behaviour.

## Cross-references

- web-app-v2 local-stack env: `../web-app-v2/.env.local`
- Pool-manager local-mode runtime: `../brahmi/CLAUDE.md` (search "Pool-manager runs LOCAL_MODE")
- mang-proxy provider-key storage internals: `../mang-proxy/internal/loadkeys/loadkeys.go`
