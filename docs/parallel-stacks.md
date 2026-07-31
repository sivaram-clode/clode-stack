# Parallel stacks — running feature-branch clones alongside baseline

Goal: run feature-branch copies of the stack **concurrently** with the baseline,
for **max isolation at minimum duplication**, **derivable addressing** (see a
request → know the service/clone without hunting ports or reading docs), and
**no application-code changes** (the service repos are sealed; only clode-stack's
own compose/scripts change).

This lands in two phases. **Phase 1 (built — `stack fork`)** is full per-clone
duplication via a Compose project; it's simple, deterministic, and isolates data
and migrations completely. **Phase 2 (designed, not built — the origin router)**
is the lean shared-baseline model that clones only the changed subtree. Phase 1
is what you use today; Phase 2 is the north-star the tooling evolves toward.

---

## Phase 1 — `stack fork` (implemented)

A clone is a **separate Compose project** (`-p <name>`) on its **own bridge
network**, with its **own traefik on its own host port**. Everything else — the
Go services, datastores, agents — is namespaced by the project, so two stacks
never collide.

```
stack fork <name> --port <traefik-port> [--workspaces <yaml>] [svc...]
stack fork-down <name>
stack fork-ls
```

- **`<name>`** — the Compose project **and** network name (baseline is `clode`;
  a clone is e.g. `b1`). Containers land as `<name>-<svc>-1`.
- **`--port`** — the clone's single host port. traefik is the **only** service
  with a host binding; everything else is in-network. So a whole clone costs
  **one host port**.
- **`--workspaces <yaml>`** — a `workspaces.yaml`-format file listing which
  services build from a **feature-branch worktree**. Those (and only those) are
  rebuilt and tagged `clode-stack/<svc>:<branch>`; every other service **reuses
  the baseline image** (`clode-<svc>:latest`) — no rebuild, no extra image disk.
- **`[svc...]`** — optional subset to bring up (like `stack up`); omit for all.

### Addressing (derivable)

Same hostnames as baseline, on the clone's port. The **port tells you the
clone**, the **hostname tells you the service**:

```
baseline:   http://brahmi.localhost:8080
clone b1:   http://brahmi.localhost:8180      (--port 8180)
clone b1 db viewer: http://whodb.localhost:8180
```

> Phase 2 moves the clone into the **hostname** (`brahmi-b1.localhost:8080`) so a
> single shared traefik serves every clone on one port — fully self-describing.
> Phase 1 keeps per-clone ports because it needs no shared-ingress machinery.

### What the fork does (overlay-only; base compose untouched at runtime)

Generates `.forks/<name>.yml`, a Compose overlay applied on top of the base
files, that:

1. **Renames the network** — `networks.clode.name: <name>` (the base pins a
   literal `name: clode`; without this override two projects share one bridge and
   collide on DNS aliases). This is the load-bearing change that makes a project
   a self-contained stack.
2. **Repoints + constrains traefik** — `--providers.docker.network=<name>` and
   `--providers.docker.constraints=Label(com.docker.compose.project,<name>)` so
   the clone's traefik only routes its **own** containers, and host port
   `<traefik-port>:8080` (via the `!override` merge tag, since Compose otherwise
   *concatenates* `ports`).
3. **Makes datastores internal** — strips host bindings on `db`, `redis`,
   `databend`, `louie`, `console-web` (via `!reset []`). No port collisions with
   baseline; reach data through the clone's WhoDB or `docker exec`.
4. **Pins images** — every buildable service → `clode-<svc>:latest` (reuse
   baseline), except changed services → `clode-stack/<svc>:<branch>` (rebuilt).
   The `clode-stack/` prefix is jumbo's `skipImageValidation` allow-list.
5. **Adds WhoDB** — a per-clone DB viewer on the clone network, connections
   pre-wired to the clone's `db`/`redis`, routed at `whodb.localhost:<port>`.

### Base-compose change (one, committed)

Baseline traefik gets `--providers.docker.constraints=Label(com.docker.compose.
project,clode)` too — otherwise it would pick up a clone's container labels (both
define a `brahmi` router for `brahmi.localhost`) and log route conflicts. Applying
it needs one baseline `traefik` recreate.

### Isolation & cost

- Own network + own `db`/`redis`/`minio`/`databend` volumes + own migrations →
  a clone's schema changes never touch baseline (**separate, non-collapsing
  databases**).
- An **unchanged** service in a clone = the baseline image run under a new
  container name: **RAM + a container, ~0 GB disk** (shared image layers). Only
  **changed** services rebuild. The dial between "one changed service" and "full
  stack" is just how many services the workspaces file lists.

### Known Phase-1 limitations

- `console-web` (a Vite dev server, not behind traefik) has its host port
  stripped in a clone, so its UI isn't reachable in a clone yet — Phase 2 routes
  it through traefik. Backend/API/agent testing is unaffected.
- Per-clone ports mean the clone is identified by port, not hostname (Phase 2
  fixes that).

---

## Phase 2 — the origin-aware router (designed, not built)

The lean end of the dial: one shared baseline; a clone runs **only the changed
subtree** and everything else falls through to baseline, decided per request by
a small Go router. No per-service URL/env changes.

### Addressing

```
External (ingress):   http://<feature>-<svc>.localhost:8080   (one shared traefik/router, always :8080)
Internal (svc→svc):    http://<svc>.internal:<port>            (unchanged callers; router disambiguates)
```

### The router (single Go binary, replaces traefik)

- `.localhost` → dumb passthrough: Host = container name → that container.
- `.internal` → **origin-aware**: source IP → container name → branch (via
  `docker.sock:ro` + `/containers/json`, refreshed off the events stream);
  Host → service; `table[branch][svc]` → `<svc>-<branch>` else baseline `<svc>`.
- Routing table is a **hot-reloaded JSON** (mtime poll + atomic swap + logged
  diff); unreachable target → 502.

### Branch = a closed subtree of the dependency graph

Origin survives one hop, so any service that must *reach* a branched service must
**itself** be branched (else its egress reverts to baseline mid-chain). The
branch is therefore the **transitive closure** over the dependency graph — the
agent declares the seed (what it changed) and the closure is computed from the
graph (extracted from every `*_URL`/`*_ADDR`/`*_BASE_URL` in the compose).

Two tiers per closure member: **build** (has a worktree diff → rebuild from it)
vs **mirror** (byte-identical → run the baseline image under `<svc>-<branch>`,
no build, just for origin-tagging).

### Agents

One shared pool-manager + pool; the **claim wrapper** injects the requesting
branch's call-home URL into the claimed agent — no duplicate pools/VMs.

### Known Phase-2 limit

Branch/stack-scoped, not per-request (true per-request needs header baggage the
sealed code can't propagate). Sufficient for "test my change against otherwise-
baseline services."

---

## Safety prerequisite (both phases)

Apply the pending `daemon.json` fix (log rotation `max-size:20m/max-file:5` +
`builder.gc.defaultKeepStorage` 20→8 GB, one docker restart) before running
looping clone containers, so a runaway clone can't refill the disk. A per-service
resource-limit overlay complements it.
