#!/usr/bin/env python3
"""Service relation map + dependency resolver for clode-stack.

Builds a DIRECTED graph of services from docker-compose. An edge A -> B means
"A needs B" — B must be up for A to run. The edge source is `depends_on`, the
stack's accurate "must be healthy/complete before I start" signal.

Why not scan env URLs? The shared `*service-urls` anchor injects every service
URL into ~22 services, so scanning environment would yield a near-complete mesh
(every service "refers to" every other) — useless for minimal resolution. So
depends_on is the signal; runtime-only HTTP calls that aren't health-gated
(e.g. brahmi -> jumbo) are intentionally not edges here — add such a service as
an explicit seed if a clone needs it.

Profile gates are NOT pruned: every service is a node, tagged with its gate
(the profile name, or "core" when always-on). Resolving a seed set returns the
transitive closure (services to wake) plus the profile gates that closure needs
enabled.

Usage:
  depgraph.py graph   [--json]         print the relation map
  depgraph.py resolve SVC [SVC...] [--json]
                                       print the wake-closure for the seeds

CLODE_STACK_DIR selects the stack checkout (default: current directory).
"""
import sys, json, subprocess, os


def _compose_base():
    d = os.environ.get("CLODE_STACK_DIR", ".")
    return ["docker", "compose", "--project-directory", d,
            "-f", os.path.join(d, "docker-compose.yml"),
            "-f", os.path.join(d, "docker-compose.cache.yml")]


def load_config():
    """Resolved compose with ALL profile gates enabled, so every node is present."""
    base = _compose_base()
    profiles = subprocess.run(base + ["config", "--profiles"],
                              capture_output=True, text=True).stdout.split()
    env = dict(os.environ, COMPOSE_PROFILES=",".join(profiles))
    out = subprocess.run(base + ["config", "--format", "json"],
                         capture_output=True, text=True, env=env)
    if out.returncode != 0:
        sys.stderr.write(out.stderr)
        sys.exit(1)
    return json.loads(out.stdout)


def build_graph(cfg):
    """-> (nodes: {svc: {gate, buildable}}, edges: set[(a,b)]  meaning a needs b)."""
    svcs = cfg["services"]
    nodes, edges = {}, set()
    for name, c in svcs.items():
        prof = c.get("profiles") or []
        nodes[name] = {"gate": prof[0] if prof else "core",
                       "buildable": bool(c.get("build"))}
        dep = c.get("depends_on") or {}
        deps = dep.keys() if isinstance(dep, dict) else dep
        for b in deps:
            edges.add((name, b))
    return nodes, edges


def resolve(seeds, edges):
    """Transitive closure over outgoing edges (seed + everything it needs)."""
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


def cmd_graph(args):
    nodes, edges = build_graph(load_config())
    adj = _adj(edges)
    if "--json" in args:
        print(json.dumps({"nodes": nodes, "edges": adj}, indent=2))
        return
    print(f"# service relation map — {len(nodes)} nodes, {len(edges)} edges "
          "(A -> B = A needs B)\n")
    for n in sorted(nodes):
        mark = "*" if nodes[n]["buildable"] else " "
        deps = ", ".join(adj.get(n, [])) or "-"
        print(f"  {mark} {n:16} [{nodes[n]['gate']:9}] -> {deps}")
    print("\n  (* = buildable; [gate] = profile, 'core' = always on)")


def cmd_resolve(args):
    seeds = [a for a in args if not a.startswith("-")]
    if not seeds:
        sys.exit("resolve: need at least one service")
    nodes, edges = build_graph(load_config())
    unknown = [s for s in seeds if s not in nodes]
    if unknown:
        sys.stderr.write(f"warn: unknown service(s): {', '.join(unknown)}\n")
    closure = resolve(seeds, edges)
    gates = sorted({nodes[n]["gate"] for n in closure
                    if n in nodes and nodes[n]["gate"] != "core"})
    if "--json" in args:
        print(json.dumps({"seeds": seeds, "wake": sorted(closure),
                          "profiles": gates}, indent=2))
        return
    print(f"# resolve {seeds} -> {len(closure)} services to wake\n")
    for n in sorted(closure):
        tag = "seed" if n in seeds else "dep"
        gate = nodes[n]["gate"] if n in nodes else "?"
        print(f"  {n:16} [{gate:9}] ({tag})")
    if gates:
        print(f"\n  profiles to enable: {','.join(gates)}")


CMDS = {"graph": cmd_graph, "resolve": cmd_resolve}
if len(sys.argv) < 2 or sys.argv[1] not in CMDS:
    sys.exit(__doc__)
CMDS[sys.argv[1]](sys.argv[2:])
