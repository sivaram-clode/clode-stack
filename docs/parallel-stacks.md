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
# benji: {...}   # optional — a fork-specific agent VM image / state (see "Agents in a fork").
                 # Omit it and the fork's agents run the baseline benji image as-is.
```

**Everything is declared, never inferred.** The YAML states *what* the fork is
(which services, which branches, which agent image + state); `wfork` only *builds
and deploys* it — it holds no per-fork flags, relations, or "if agents then also…"
logic. A key that isn't set takes the baseline default unchanged, so the smallest
useful fork is a single `services:` entry.

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

## Agents in a fork (aramb-vm, on-demand)

The default substrate is **aramb-vm**: brahmi provisions **one VM per project, on
demand**, straight through the mock EC2 (`mock-services`) — no warm pool, no
pool-manager. That makes agents in a fork fall out for free. A forked
`brahmi-<name>` already carries its own `BRAHMI_URL` (rewritten to
`http://brahmi-<name>:8080`) and its own `AGENT_VM_IMAGE`; brahmi bakes both into
each VM's cloud-init, and the mock launches exactly that image and points its
call-home at that brahmi. **No pool-manager fork, no `agents:` flag** — just put
`brahmi` in the fork and its agents attach to it.

Two things about the agent are **declared, never inferred** — the config states
them, stack builds + wires them, and omitting either takes the baseline default
as-is.

### A fork-specific agent image — `benji.branch`

Omit `benji` and the fork's VMs run the baseline agent image
(`clode-stack/benji:latest`) unchanged. To pin the fork to its own agent image:

```yaml
name: b1
services:
  brahmi: { branch: feat/x, db: reuse }
benji:
  branch: feat/agent-change     # build clode-stack/benji:b1 from ../benji worktree feat/agent-change
```

`wfork up` builds `clode-stack/benji:b1` from that worktree and sets
`AGENT_VM_IMAGE=clode-stack/benji:b1` on `brahmi-b1`. Because the mock deploys the
**incoming** image (whatever brahmi bakes in — there is no server-side
default-image), the fork's VMs run exactly `:b1` while baseline keeps running
`:latest`. Deterministic end to end: the tag is `:<name>` (derived from the fork
name), the build source is the declared branch — no `--agent` flag, no up-time
image inference.

### A fork-specific agent state — `benji.state`

The benji-state tarball baked into the agent image is declarative too. Omit it and
the image's built-in state flies in unchanged; declare it to bake a fork-specific
state:

```yaml
benji:
  branch: feat/agent-change
  state:
    build: true                 # build state.tar.gz from ../benji-state + ../aramb-skills, bake into clode-stack/benji:b1
    # from: /abs/path/state.tar.gz   # …or bake a prebuilt tarball instead
```

Stack does only the mechanical build + overlay
(`FROM clode-stack/benji:b1` → `COPY state.tar.gz …` → `ENV BENJI_STATE_PULL=false`)
and wires the resulting tag onto `brahmi-b1`. It owns *building and deploying*, not
the relations: the whole agent shape for a fork lives in this one block, and the
default needs no block at all.

### The legacy pod path (opt-in)

The pod substrate (agents as k8s-style pods via **pool-manager**) is off by
default — pool-manager is behind the `pool` profile. If a fork explicitly sets
its brahmi to `AGENT_PROVIDER=aramb` (pods), it must also fork pool-manager so the
static `BRAHMI_URL` pool-manager bakes into each agent points at the fork's brahmi,
and seed that fork's `pool-manager_<name>` svc_configs (`seed.py svc-configs
pool-manager_<name>`, image from `BENJI_IMAGE`). The aramb-vm default above avoids
all of that — prefer it for forks.

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
with no extra step. The agent image/state a fork runs isn't seed data — it's
declared in the `benji:` block (above) and built into the image, so it needs no
DB seed at all. (The dynamic pool-manager svc_config only applies to the opt-in
pod path.)

## Resource ceilings

`docker-compose.limits.yml` sets a per-service `mem_limit` + `cpus` **ceiling**
(caps a runaway container — like the usageq hot-loop that once filled the disk —
not tight packing). `up.py` applies it on every bring-up and each fork container
inherits it; `NO_LIMITS=1` skips it.

## No duplicate pools — solved by the on-demand VM substrate

The original worry was that per-fork agents would mean a per-fork **pool**
(duplicate warm capacity, per-fork pool-manager) — the reason the north star was a
per-request origin-aware router feeding a *single* shared pool. The aramb-vm
default retires that worry: there is **no pool** to duplicate. Each fork's brahmi
provisions VMs **on demand** and each VM call-homes to the brahmi that launched it
(its `BRAHMI_URL` baked into cloud-init), so N forks cost nothing at rest and never
share or contend for pool capacity. The per-request router (header baggage the
sealed code can't propagate today) is only still relevant for the opt-in pod path;
for the VM default, explicit per-fork `brahmi` containers already give clean
isolation with zero standing duplication.
