#!/usr/bin/env python3
"""Service relation map + fork reasoning for clode-stack.

Reads the CODE-VERIFIED annotated graph `service-graph.json` (hand-maintained,
not grepped): each service has {gate, buildable, calls:{peer:{rw, for}}} where
rw = R (read) | W (writes/mutates peer) | RW | mint (issues a token). Keys
starting with `_` are metadata: `_dead_env` (URL read but never called — NOT a
dependency) and `_indirect` (config injected into the agent, which makes the call).

Commands:
  graph                 print the relation map (A -> B [rw] purpose)
  resolve SVC... [--json]   downstream closure (everything the seeds transitively call)
  check   SVC... [--json]   is the set dependency-closed? list dropped callees; flag W edges

CLODE_STACK_DIR is unused (the graph is committed next to this script).
"""
import sys, json, os

STATIC = os.path.join(os.path.dirname(os.path.abspath(__file__)), "service-graph.json")


def load():
    """-> (nodes:{svc:{gate,buildable,calls}}, edges:{(a,b):{rw,for}}, raw)."""
    raw = json.load(open(STATIC))
    nodes = {s: m for s, m in raw.items() if not s.startswith("_")}
    edges = {}
    for a, m in nodes.items():
        for b, meta in (m.get("calls") or {}).items():
            edges[(a, b)] = meta
    return nodes, edges, raw


def _adj(edges):
    a = {}
    for (x, y) in edges:
        a.setdefault(x, []).append(y)
    return a


def resolve(seeds, edges):
    adj = _adj(edges)
    seen, stack = set(), list(seeds)
    while stack:
        n = stack.pop()
        if n in seen:
            continue
        seen.add(n)
        stack.extend(b for b in adj.get(n, ()) if b not in seen)
    return seen


def cmd_graph(args):
    nodes, edges, raw = load()
    adj = {}
    for (a, b), meta in edges.items():
        adj.setdefault(a, []).append((b, meta))
    if "--json" in args:
        print(json.dumps(raw, indent=2))
        return
    print(f"# service relation map — {len(nodes)} nodes, {len(edges)} verified edges "
          "(A -> B [rw]; rw = R|W|RW|mint)\n")
    for n in sorted(nodes):
        outs = sorted(adj.get(n, []))
        gate = nodes[n]["gate"]
        if not outs:
            print(f"  {n:16} [{gate:9}] -> -")
            continue
        print(f"  {n:16} [{gate:9}] ->")
        for b, meta in outs:
            print(f"      [{meta.get('rw',''):4}] {b:16} {meta.get('for','')}")
    dead = raw.get("_dead_env", [])
    ind = raw.get("_indirect", [])
    print(f"\n  ({len(dead)} dead-env non-edges, {len(ind)} indirect/config-injection — see _dead_env/_indirect)")


def cmd_resolve(args):
    seeds = [a for a in args if not a.startswith("-")]
    if not seeds:
        sys.exit("resolve: need at least one service")
    nodes, edges, _ = load()
    unknown = [s for s in seeds if s not in nodes]
    if unknown:
        sys.stderr.write(f"warn: unknown service(s): {', '.join(unknown)}\n")
    closure = resolve(seeds, edges)
    gates = sorted({nodes[n]["gate"] for n in closure
                    if n in nodes and nodes[n]["gate"] not in ("core", "external")})
    if "--json" in args:
        print(json.dumps({"seeds": seeds, "wake": sorted(closure), "profiles": gates}, indent=2))
        return
    print(f"# resolve {seeds} -> {len(closure)} services\n")
    for n in sorted(closure):
        tag = "seed" if n in seeds else "dep"
        print(f"  {n:16} [{nodes[n]['gate'] if n in nodes else '?':9}] ({tag})")
    if gates:
        print(f"\n  profiles to enable: {','.join(gates)}")


def cmd_check(args):
    svcs = [a for a in args if not a.startswith("-")]
    if not svcs:
        sys.exit("check: need the service set to validate")
    nodes, edges, _ = load()
    have = set(svcs)
    missing = {}   # dropped callee -> [(caller, rw)]
    for (a, b), meta in edges.items():
        if a in have and b not in have:
            missing.setdefault(b, []).append((a, meta.get("rw", "")))
    if "--json" in args:
        print(json.dumps({"closed": not missing,
                          "missing": {k: sorted(c for c, _ in v) for k, v in missing.items()}}, indent=2))
        sys.exit(0 if not missing else 1)
    if not missing:
        print(f"OK: {len(have)} services form a dependency-closed set")
        return
    sys.stderr.write("DISCONNECTED: in-between node(s) missing from the set —\n")
    for b in sorted(missing):
        callers = missing[b]
        w = any(rw in ("W", "RW") for _, rw in callers)
        flag = "  ⚠ WRITE (mutates baseline)" if w else ""
        who = ", ".join(f"{c}[{rw}]" for c, rw in sorted(callers))
        sys.stderr.write(f"  {b}  <- {who}{flag}\n")
    sys.stderr.write("\nadd them to the set, or pass --resolve to auto-include. "
                     "⚠ WRITE edges to a baseline peer mutate baseline state.\n")
    sys.exit(1)


CMDS = {"graph": cmd_graph, "resolve": cmd_resolve, "check": cmd_check}
if len(sys.argv) < 2 or sys.argv[1] not in CMDS:
    sys.exit(__doc__)
CMDS[sys.argv[1]](sys.argv[2:])
