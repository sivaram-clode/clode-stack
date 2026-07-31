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
- **`--resolve`** — expand the given `[svc...]` seeds to their **dependency
  closure** (see the resolver below) and auto-enable the profile gates that
  closure needs, plus `traefik` + `whodb`. So `stack fork b1 --port 8180 --resolve
  brahmi` wakes brahmi *and everything it needs* — nothing more.

### Service relation map + dependency resolver

`scripts/lib/depgraph.py` (exposed as `stack graph` / `stack resolve`) builds a
**directed** service graph from the compose — an edge `A -> B` means *A needs B*.
The edge source is **`depends_on`**, the stack's accurate "must be healthy/complete
before I start" signal.

> Why not scan env URLs? The shared `*service-urls` anchor injects every service
> URL into ~22 services, so env-scanning would make it a near-complete mesh —
> useless for minimal resolution. Runtime-only HTTP calls that aren't
> health-gated (e.g. `brahmi -> jumbo`) are therefore not edges; add such a
> service as an explicit seed if a clone needs it.

Profile gates are **not** pruned — every service is a node tagged with its gate
(`core` = always-on). `resolve` returns the transitive closure to wake **and the
profile gates that closure touches** (it walks *across* gates, e.g.
`intervix` → `vova` surfaces both `interview` and `voice`).

```
stack graph                     # full relation map (nodes, edges, gates)
stack resolve brahmi            # brahmi + db, ec2mock, minio, minio-setup, raksha, redis
stack resolve mang-proxy toolkit-proxy   # closure + "profiles to enable: tools"
```

`fork --resolve` chains this in: seeds → closure → per node, **rebuild from
branch if it's in the workspaces file, else run the baseline image** (the
build/mirror split), with the needed profiles enabled automatically.

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

### console-web through the ingress

`console-web` is a Vite dev server, historically the traefik exception (direct
host port `:3001` only). It now also carries traefik labels, so it's reachable
at `http://console.localhost:8080` on baseline and `http://console.localhost:<port>`
in a clone (where the `:3001` host port is stripped). Vite 7 blocks unknown Host
headers, so the compose sets `__VITE_ADDITIONAL_SERVER_ALLOWED_HOSTS=console.localhost`
— Vite's own env hook, so `vite.config.ts` (sealed) is untouched and the prod
static build (no Vite) is unaffected. HMR's WS upgrade is proxied by traefik.

> Note: console-web's dev proxy targets (`VITE_*_BASE_URL`) must resolve to the
> clone's own services for a clone's UI to be fully wired — they use in-network
> service names, so within the clone network they already point at the clone's
> instances.

### Known Phase-1 limitations

- Per-clone ports mean the clone is identified by port, not hostname (Phase 2
  moves the clone into the hostname so one shared ingress serves all clones).

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
