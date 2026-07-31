#!/usr/bin/env python3
"""Service relation map + dependency resolver for clode-stack.

Edges are the ACTUAL service call graph, derived from each service's own Go
source — NOT from compose `depends_on` (which is only boot order: brahmi does
not depends_on jumbo/toolkit-proxy yet calls them). For every service repo we
grep its .go files for the peer URL env-var names it reads (JUMBO_URL,
TOOLKIT_PROXY_BASE_URL, POOL_MANAGER_URL, RAKSHA_BASE_URL, …) and map each name
to the service it points at. An edge A -> B means "A's code reads B's URL" —
i.e. A calls B.

Compose is used ONLY for the node inventory + metadata (service name, its repo
dir via build.context, and its profile gate) — never for the edges.

Profile gates are NOT pruned: every service is a node tagged with its gate
(`core` = always-on). Resolving a seed set returns the transitive wake-closure
plus the profile gates that closure needs enabled.

The grepped graph is frozen into a committed static map (service-graph.json, a
standard adjacency representation) so `graph`/`resolve` are fast and the map is
inspectable/editable. Regenerate it after code changes with `build` or `--refresh`.

Usage:
  depgraph.py graph   [--json] [--refresh]   print the relation map
  depgraph.py resolve SVC... | --workspace FILE  [--json] [--refresh]
                                             wake-closure for seeds (or a workspace
                                             file's services); marks which nodes are
                                             branch vs connecting (in-between) nodes
  depgraph.py check   SVC... [--json]        pre-flight: is this set dependency-closed?
                                             exit 1 + list dropped in-between nodes if not
  depgraph.py build                          (re)generate service-graph.json from source

CLODE_STACK_DIR selects the stack checkout (default: current directory).
"""
import sys, json, subprocess, os, re

STACK_DIR = os.environ.get("CLODE_STACK_DIR", ".")

# Env-var name → service resolution. Strip role/transport tokens, join the rest,
# and match (separator-insensitive) against a service name. So MANG_PROXY_URL ->
# "mangproxy" -> mang-proxy; CHA_CHING_URL & CHACHING_URL -> "chaching" ->
# cha-ching; TOOLKIT_PROXY_BASE_URL -> toolkit-proxy; AUTH_ISSUER -> (alias) raksha.
_DROP = {"URL", "BASE", "INTERNAL", "EXTERNAL", "MCP", "API", "ADDR", "BROKER",
         "ENDPOINT", "ISSUER", "CONNECT", "SERVICE", "SVC", "HOST", "PORT",
         "GRPC", "WS", "HTTP", "V1", "V2"}
_ALIAS = {"auth": "raksha", "jwt": "raksha", "database": "db", "postgres": "db"}
# quoted env-var literal in Go source (os.Getenv("X"), `env:"X"` tags, etc.)
_GREP_PAT = r'"[A-Z][A-Z0-9_]*(URL|ADDR|ENDPOINT|ISSUER|BROKER)[A-Z0-9_]*"'
_NAME_RE = re.compile(r'"([A-Z][A-Z0-9_]*(?:URL|ADDR|ENDPOINT|ISSUER|BROKER)[A-Z0-9_]*)"')


def _norm(s):
    return re.sub(r'[^a-z0-9]', '', s.lower())


def load_inventory():
    """Nodes + metadata from compose (all gates enabled). Edges come from source."""
    d = STACK_DIR
    base = ["docker", "compose", "--project-directory", d,
            "-f", os.path.join(d, "docker-compose.yml"),
            "-f", os.path.join(d, "docker-compose.cache.yml")]
    profiles = subprocess.run(base + ["config", "--profiles"],
                              capture_output=True, text=True).stdout.split()
    env = dict(os.environ, COMPOSE_PROFILES=",".join(profiles))
    out = subprocess.run(base + ["config", "--format", "json"],
                         capture_output=True, text=True, env=env)
    if out.returncode != 0:
        sys.stderr.write(out.stderr)
        sys.exit(1)
    inv = {}
    for name, c in json.loads(out.stdout)["services"].items():
        b = c.get("build")
        ctx = b.get("context") if isinstance(b, dict) else (b if isinstance(b, str) else None)
        prof = c.get("profiles") or []
        inv[name] = {
            "gate": prof[0] if prof else "core",
            "buildable": bool(b),
            "dir": os.path.realpath(os.path.join(d, ctx)) if ctx else None,
        }
    return inv


def _env_names_in(path):
    """Peer URL env-var names referenced in a repo's .go source."""
    if not path or not os.path.isdir(path):
        return set()
    r = subprocess.run(
        ["grep", "-rhoE", "--include=*.go",
         "--exclude-dir=vendor", "--exclude-dir=.worktrees",
         "--exclude-dir=.claude", "--exclude-dir=node_modules",
         _GREP_PAT, path],
        capture_output=True, text=True)
    names = set()
    for line in r.stdout.splitlines():
        m = _NAME_RE.search(line)
        if m:
            names.add(m.group(1))
    return names


def _target(envname, index):
    core = "".join(p for p in envname.split("_") if p not in _DROP).lower()
    return _ALIAS.get(core) or index.get(core)


def build_edges(inv):
    """A -> B when A's source reads an env var naming B. Returns (edges, why)."""
    index = {_norm(s): s for s in inv}
    edges, why = set(), {}
    for svc, meta in inv.items():
        for name in _env_names_in(meta["dir"]):
            t = _target(name, index)
            if t and t != svc and t in inv:
                edges.add((svc, t))
                why.setdefault((svc, t), name)
    return edges, why


def resolve(seeds, edges):
    adj = {}
    for a, b in edges:
        adj.setdefault(a, set()).add(b)
    seen, stack = set(), list(seeds)
    while stack:
        n = stack.pop()
        if n in seen:
            continue
        seen.add(n)
        stack.extend(b for b in adj.get(n, ()) if b not in seen)
    return seen


def _adj(edges):
    a = {}
    for x, y in sorted(edges):
        a.setdefault(x, []).append(y)
    return a


STATIC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "service-graph.json")


def write_static(inv, edges):
    adj = _adj(edges)
    data = {s: {"gate": inv[s]["gate"], "buildable": inv[s]["buildable"],
                "calls": adj.get(s, [])} for s in sorted(inv)}
    with open(STATIC, "w") as f:
        json.dump(data, f, indent=2)
        f.write("\n")
    return data


def load_graph(refresh=False):
    """(inv, edges). Uses the committed static map unless --refresh or it's absent."""
    if refresh or not os.path.exists(STATIC):
        inv = load_inventory()
        edges, _ = build_edges(inv)
        write_static(inv, edges)
        return inv, edges
    data = json.load(open(STATIC))
    inv = {s: {"gate": m["gate"], "buildable": m["buildable"]} for s, m in data.items()}
    edges = {(s, t) for s, m in data.items() for t in m.get("calls", [])}
    return inv, edges


def cmd_build(args):
    inv = load_inventory()
    edges, _ = build_edges(inv)
    write_static(inv, edges)
    print(f"wrote {STATIC}\n  {len(inv)} nodes, {len(edges)} edges (from config.go grep)")


def cmd_graph(args):
    inv, edges = load_graph("--refresh" in args)
    adj = _adj(edges)
    if "--json" in args:
        print(json.dumps({s: {"gate": inv[s]["gate"], "buildable": inv[s]["buildable"],
                              "calls": adj.get(s, [])} for s in sorted(inv)}, indent=2))
        return
    src = "live grep (rewrote static map)" if "--refresh" in args \
        else ("static map" if os.path.exists(STATIC) else "live grep")
    print(f"# service relation map — {len(inv)} nodes, {len(edges)} edges "
          f"(A -> B = A calls B; source: {src})\n")
    for n in sorted(inv):
        mark = "*" if inv[n]["buildable"] else " "
        deps = ", ".join(adj.get(n, [])) or "-"
        print(f"  {mark} {n:16} [{inv[n]['gate']:9}] -> {deps}")
    print("\n  (* = buildable; [gate] = profile, 'core' = always on)")
    print("  regenerate after code changes: stack graph --refresh")


def _seeds_from_workspace(path):
    """Service names listed in a workspaces.yaml-format file (flat `svc: selector`)."""
    seeds = []
    with open(path) as f:
        for raw in f:
            line = raw.split("#", 1)[0].strip()
            if line and ":" in line and not line.startswith("_"):
                seeds.append(line.split(":", 1)[0].strip())
    return seeds


def cmd_resolve(args):
    a = list(args)
    ws = None
    for flag in ("--workspace", "--workspaces"):
        if flag in a:
            i = a.index(flag)
            ws = a[i + 1]
            del a[i:i + 2]
    seeds = [x for x in a if not x.startswith("-")]
    branch = set()
    if ws:
        wsseeds = _seeds_from_workspace(ws)
        seeds += wsseeds
        branch |= set(wsseeds)
    if not seeds:
        sys.exit("resolve: need a service or --workspace FILE")
    inv, edges = load_graph("--refresh" in a)
    unknown = [s for s in seeds if s not in inv]
    if unknown:
        sys.stderr.write(f"warn: unknown service(s): {', '.join(unknown)}\n")
    closure = resolve(seeds, edges)
    gates = sorted({inv[n]["gate"] for n in closure
                    if n in inv and inv[n]["gate"] != "core"})
    connecting = sorted(n for n in closure if n not in (branch or set(seeds)))
    if "--json" in a:
        print(json.dumps({"seeds": seeds, "wake": sorted(closure),
                          "connecting": connecting, "profiles": gates}, indent=2))
        return
    print(f"# resolve {seeds} -> {len(closure)} services to wake\n")
    for n in sorted(closure):
        if ws:
            tag = "branch" if n in branch else "connecting"
        else:
            tag = "seed" if n in seeds else "dep"
        gate = inv[n]["gate"] if n in inv else "?"
        print(f"  {n:16} [{gate:9}] ({tag})")
    if ws and connecting:
        print(f"\n  in-between nodes (run on MAIN unless branched): {', '.join(connecting)}")
    if gates:
        print(f"  profiles to enable: {','.join(gates)}")


def cmd_check(args):
    svcs = [a for a in args if not a.startswith("-")]
    if not svcs:
        sys.exit("check: need the service set to validate")
    inv, edges = load_graph("--refresh" in args)
    adj = _adj(edges)
    have = set(svcs)
    missing = {}   # dropped callee -> [callers in the set that need it]
    for a in svcs:
        for b in adj.get(a, []):
            if b not in have:
                missing.setdefault(b, []).append(a)
    if "--json" in args:
        print(json.dumps({"closed": not missing,
                          "missing": {k: sorted(v) for k, v in missing.items()}}, indent=2))
        sys.exit(0 if not missing else 1)
    if not missing:
        print(f"OK: {len(have)} services form a dependency-closed set")
        return
    sys.stderr.write("DISCONNECTED: in-between node(s) missing from the set —\n")
    for b in sorted(missing):
        sys.stderr.write(f"  {b}  <- called by {', '.join(sorted(missing[b]))}\n")
    sys.stderr.write("\nadd them to the set, or pass --resolve to auto-include the closure.\n")
    sys.exit(1)


CMDS = {"graph": cmd_graph, "resolve": cmd_resolve, "check": cmd_check, "build": cmd_build}
if len(sys.argv) < 2 or sys.argv[1] not in CMDS:
    sys.exit(__doc__)
CMDS[sys.argv[1]](sys.argv[2:])
