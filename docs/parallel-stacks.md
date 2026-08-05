# Parallel stacks — running a feature branch alongside baseline (`wfork`)

Goal: run feature-branch copies of one or more services **concurrently** with the
baseline, for **max isolation at minimum duplication**, **derivable addressing**
(a request's hostname names the service *and* the fork — no port hunting), and
**no application-code changes** (the service repos are sealed; only clode-stack's
own compose/scripts change).

**Baseline is always `main`.** Every service builds from its main sibling
checkout (`../<svc>`); there is no in-place branch swap of the baseline. A feature
branch runs *only* as a fork — which is what makes a fork's `mirror` image
unambiguously the baseline `:main` image.

## The model — `wfork`

A fork is **one container `<svc>-<name>` per listed service, on the existing
`clode` network**, reached at `http://<svc>-<name>.localhost:8080` through the
baseline traefik. It's declared in one reviewable YAML and applied atomically —
the config is the single source of truth (no interleaved build/up commands).

```yaml
# fork.b1.yaml
name: b1
services:
  brahmi:        { branch: feat/x, db: reuse }   # build clode-stack/brahmi:b1 from the worktree
  aramb-gateway: { mirror: true }                # baseline :main image, run as aramb-gateway-b1
console: true                                    # static console-web wired to the forked backends
agents: true                                     # also fork pool-manager so agents call home to brahmi-b1
```

```bash
stack wfork preview --config fork.b1.yaml   # dry-run: routing + ⚠ WRITE-to-baseline warnings — RUN FIRST
stack wfork up      --config fork.b1.yaml   # the only mutating step (atomic)
stack wfork down    --config fork.b1.yaml   # tear down (containers + any fresh DBs)
stack wfork ls                              # list forks
stack wfork prune                           # tear down ALL forks
```

## What `up` wires per service (all from `docker compose config` — no routing layer)

- **image** — `branch: <b>` builds `clode-stack/<svc>:<name>` from that branch's
  git worktree (resolved under `../<svc>`); `mirror: true` (the default when no
  `branch:` is given) reuses the baseline `:main` image, no rebuild.
- **env** — lifted from the baseline's resolved config, then the **host token of
  every forked peer (and self) is rewritten `<peer>` → `<peer>-<name>`**:
  `aramb-gateway-b1` gets `BRAHMI_URL=http://brahmi-b1:8080`, `brahmi-b1` gets
  `CLUSTER_GRPC_ADDR=brahmi-b1:9500`. Unlisted peers fall through to the baseline
  by DNS. The rewrite only touches a `host:port` / `host/path` token, so a bare
  value like `DB_NAME=brahmi` is never corrupted.
- **command + resource caps** are lifted from baseline too (caps from
  `docker-compose.limits.yml`).
- **db** — `reuse` (default) shares the baseline DB (zero setup, no data
  isolation); `fresh` creates an **empty** `<svc>_<name>` DB — the forked
  container's own `<svc> migrate` then builds the schema *and* runs the seed
  appended to its last migration (see "Seed model"), identical to a baseline
  first boot. `down` drops fresh DBs.
- **console** (`console: true`) — a static console-web build with the forked
  backends' `VITE_*` URLs baked in, served at `console-web-<name>.localhost:8080`.

## Agent conversations in a fork (`agents: true`)

Agents are provisioned by **pool-manager**, which bakes a *static* `BRAHMI_URL`
into every agent it deploys — so a forked brahmi gets no agents unless
pool-manager is *also* forked to point at it. `agents: true` handles that: it
**auto-adds `pool-manager` to the fork** (`mirror`, `db: fresh`); the env rewrite
repoints `pool-manager-<name>`'s `BRAHMI_URL` → `http://brahmi-<name>:8080`, so
agents deployed in the fork call home to the fork's brahmi. It requires `brahmi`
in the fork.

pool-manager's `svc_configs` (agent image + pool sizing) is the one **dynamic**
seed — the local image tag is resolved at up-time, so it can't be embedded in a
migration. `wfork up` therefore seeds it explicitly: after the fork is up it waits
for `pool-manager_<name>`'s schema, then runs `seed.py svc-configs
pool-manager_<name>` (with `BENJI_IMAGE`, default `clode-stack/benji:main`). The
agent containers are deployed by the shared baseline mock-services/jumbo, but the
brahmi URL rides in from `pool-manager-<name>`'s env, so they attach to the fork.

## `preview` — read the consequences before `up`

`preview` reads the code-verified service graph (`scripts/lib/service-graph.json`,
also exposed via `stack graph` / `resolve` / `check`) and prints, before anything
runs:

- what routes into the fork (`aramb-gateway → brahmi ✓`),
- every **`⚠ WRITE mutates BASELINE`** edge — a forked brahmi still *write-calls*
  baseline jumbo/raksha/… unless you fork them too,
- baseline callers that won't reach the fork.

The graph's `rw` field (**R** read / **W** write / **mint**) is load-bearing:
falling through to a baseline peer is safe for **R** edges but a **W** edge
mutates baseline state (creates projects/deploys, mints identities, consumes pool
agents). Always `preview` first.

## Seed model (shared with baseline — see `adding-a-service.md`)

`seeds/<svc>-seed.sql` is the single source of a service's seed data. It is:

1. **embedded** by `gen-build-cache` onto that service's last migration (the
   migrations dir is auto-discovered), so `<svc> migrate` seeds a fresh DB itself
   — this is what seeds a `db: fresh` fork (and a baseline first boot); and
2. **applied** by `seed.py`'s one uniform loop as the reseed backstop (after a
   `cleanup` truncate, where `migrate` no-ops on the already-migrated schema).

So a `db: fresh` fork of any service that has a seed file comes up fully seeded
with no extra step. The only exception is the dynamic pool-manager svc_config,
handled by `agents: true` above.

## Resource ceilings

`docker-compose.limits.yml` sets a per-service `mem_limit` + `cpus` **ceiling**
(caps a runaway container — like the usageq hot-loop that once filled the disk —
not tight packing). `up.py` applies it on every bring-up and each fork container
inherits it; `NO_LIMITS=1` skips it.

## Not built — the origin-aware router (north star)

The leaner end of the dial: one shared baseline where a fork runs *only* the
changed subtree and every other call falls through to baseline, decided
per-request by a small router that injects the requesting branch's call-home URL
into claimed agents (so a *single* shared pool-manager serves all forks — no
duplicate pools). True per-request routing needs header baggage the sealed code
can't propagate today, so `wfork` — explicit per-fork containers plus the
`agents: true` pool-manager fork — is the model in use.
