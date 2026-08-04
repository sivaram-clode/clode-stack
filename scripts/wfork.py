#!/usr/bin/env python3
"""wfork — within-network workspace fork, driven entirely by a YAML config.

A fork is declared in one reviewable file and applied atomically. Each changed
service runs as `<svc>-<name>` on the existing `clode` network, reached at
`http://<svc>-<name>.localhost:8080` via baseline traefik. Unchanged peers fall
through to baseline by DNS; peers you *also* fork are rewired by an env rewrite —
no routing layer.

    wfork preview --config fork.b1.yaml   # dry-run: boundary report + WRITE warnings
    wfork up      --config fork.b1.yaml   # the only mutating step (atomic)
    wfork down    --config fork.b1.yaml   # tear down one fork (containers + fresh DBs)
    wfork prune                           # tear down ALL forks (containers + DBs + fork images)
    wfork ls

Config schema:
    name: b1
    services:
      brahmi:        { branch: feat/x, db: reuse }   # branch -> build clode-stack/brahmi:b1
      aramb-gateway: { mirror: true }                # baseline image, run as aramb-gateway-b1
    console: true                                    # build console-b1 -> forked backends
"""
import argparse
import json
import os
import re
import sys
import tempfile
from pathlib import Path

import yaml

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s  # noqa: E402

GRAPH = Path(__file__).resolve().parent / "lib" / "service-graph.json"
STATE = s.STACK_DIR / ".forks"

# service -> the VITE_* var the console SPA reads (+ any path suffix) for its build
VITE_VAR = {
    "aramb-gateway": "VITE_GATEWAY_BASE_URL", "raksha": "VITE_RAKSHA_BASE_URL",
    "brahmi": "VITE_BRAHMI_BASE_URL", "jumbo": "VITE_JUMBO_BASE_URL",
    "cha-ching": "VITE_CHACHING_BASE_URL", "toolkit-proxy": "VITE_TOOLKIT_PROXY_BASE_URL",
    "skills-registry": "VITE_SKILLS_REGISTRY_BASE_URL", "ikki": "VITE_IKKI_BASE_URL",
}
VITE_SUFFIX = {"jumbo": "/api/v1", "cha-ching": "/api/v1", "toolkit-proxy": "/api/v1"}


# ── config ──────────────────────────────────────────────────────────────────
def load_config(path: str) -> dict:
    raw = yaml.safe_load(Path(path).read_text()) or {}
    name = raw.get("name") or s.die("config: missing 'name'")
    services = {}
    for svc, m in (raw.get("services") or {}).items():
        m = m or {}
        branch = m.get("branch")
        services[svc] = {
            "branch": branch,
            "mirror": bool(m.get("mirror")) or branch is None,
            "db": m.get("db", "reuse"),
            "env": m.get("env") or {},
        }
    console = raw.get("console")
    if console is True:
        console = {"fork": list(services)}
    elif not console:
        console = None
    elif isinstance(console, dict):
        console.setdefault("fork", list(services))
    return {"name": name, "services": services, "console": console}


def load_graph():
    g = json.loads(GRAPH.read_text())
    nodes = {k: v for k, v in g.items() if not k.startswith("_")}
    edges = {(a, b): meta
             for a, m in nodes.items()
             for b, meta in (m.get("calls") or {}).items()}
    return nodes, edges


# ── shared wiring ─────────────────────────────────────────────────────────────
def lb_port(svc_cfg) -> str:
    labels = svc_cfg.get("labels") or {}
    if isinstance(labels, list):
        labels = dict(x.split("=", 1) for x in labels if "=" in x)
    for k, v in labels.items():
        if k.endswith("loadbalancer.server.port"):
            return str(v)
    return "8080"


def build_env(svc, name, forked, dbmode, cfg_services, extra) -> dict:
    """Baseline env, with forked peers (and self) rewritten to <peer>-<name>."""
    env = dict(cfg_services[svc].get("environment") or {})
    for peer in forked:  # includes self: own bind addr / self-URL must point at the fork too
        # rewrite the HOST token only: require a trailing :port or /path so bare
        # non-host values (DB_NAME=brahmi) are untouched; the prefix boundary
        # stops `brahmi` from matching inside `brahmi-internal`.
        pat = re.compile(rf"(^|[/@]){re.escape(peer)}(?=[:/])")
        for k, v in env.items():
            if v is not None:
                env[k] = pat.sub(rf"\g<1>{peer}-{name}", str(v))
    if dbmode == "fresh" and env.get("DB_NAME"):
        env["DB_NAME"] = f"{env['DB_NAME']}_{name}"
    env.update(extra or {})
    return env


def write_env_file(env: dict) -> str:
    fd, path = tempfile.mkstemp(prefix="wfork-env-")
    with os.fdopen(fd, "w") as f:
        for k, v in env.items():
            if v is None:
                continue
            v = str(v)
            if "\n" not in v:  # env-file can't hold multiline values
                f.write(f"{k}={v}\n")
    return path


def run_service(cname, svc, name, image, cfg_services, envfile, project):
    c = cfg_services[svc]
    cmd = c.get("command") or []
    if isinstance(cmd, str):
        cmd = [cmd]
    args = [
        "run", "-d", "--name", cname, "--network", s.NET, "--restart", "unless-stopped",
        "--label", f"com.docker.compose.project={project}", "--label", "clode.wfork=1",
        "--label", f"clode.fork={name}", "--label", f"clode.svc={svc}",
        "--label", "traefik.enable=true",
        "--label", f"traefik.http.routers.{cname}.rule=Host(`{cname}.localhost`)",
        "--label", f"traefik.http.services.{cname}.loadbalancer.server.port={lb_port(c)}",
        "--env-file", envfile,
    ]
    if c.get("mem_limit"):
        args += ["--memory", str(c["mem_limit"])]
    if c.get("cpus"):
        args += ["--cpus", str(c["cpus"])]
    if isinstance(c.get("entrypoint"), str):
        args += ["--entrypoint", c["entrypoint"]]
    args += [image, *cmd]
    s.docker(*args, capture=True)


def branch_build(svc, branch, image):
    """Build clode-stack/<svc>:<name> from the branch worktree, reusing the up build path."""
    base = s.STACK_DIR / ".." / svc
    wt = s.run(["git", "-C", base, "worktree", "list", "--porcelain"],
               capture=True, check=False).stdout
    dir_, cur = None, None
    for line in wt.splitlines():
        if line.startswith("worktree "):
            cur = line.split(maxsplit=1)[1]
        elif line.startswith("branch ") and line.split()[1] == f"refs/heads/{branch}":
            dir_ = cur
    if not dir_ and (base / branch).is_dir():
        dir_ = str(base / branch)
    if not dir_ or not Path(dir_, "Dockerfile").exists():
        s.die(f"{svc}: no worktree/Dockerfile for branch '{branch}' under {base}")
    var = svc.upper().replace("-", "_") + "_DIR"
    overlay = tempfile.NamedTemporaryFile("w", suffix=".yml", delete=False)
    overlay.write(f"services:\n  {svc}:\n    image: {image}\n")
    overlay.close()
    s.run([sys.executable, s.REPO_DIR / "scripts" / "gen-build-cache.py"],
          env={var: dir_}, check=False)
    s.compose("-f", overlay.name, "build", svc, env={var: dir_})
    os.unlink(overlay.name)


def fresh_db(svc, name, base_db):
    new = f"{base_db}_{name}"
    dbc = s.db_container()
    exists = s.docker("exec", dbc, "psql", "-U", "postgres", "-tc",
                      f"SELECT 1 FROM pg_database WHERE datname='{new}'",
                      capture=True, check=False).stdout
    if "1" not in exists:
        s.docker("exec", dbc, "psql", "-U", "postgres", "-c",
                 f'CREATE DATABASE "{new}" TEMPLATE template0', check=False)
    # copy baseline schema (pg_dump -s is safe against a live baseline; TEMPLATE is not)
    dump = s.docker("exec", dbc, "pg_dump", "-s", "-U", "postgres", base_db,
                    capture=True, check=False).stdout
    s.docker("exec", "-i", dbc, "psql", "-U", "postgres", "-d", new, stdin=dump, check=False)
    s.log(f"  db: fresh -> {new} (schema copied; migrations may still be needed)")


def console_up(name, forked, project):
    cname, img = f"console-web-{name}", f"clode-console-web-{name}:latest"
    bargs = []
    for p in forked:
        if p in VITE_VAR:
            bargs += ["--build-arg",
                      f"{VITE_VAR[p]}=http://{p}-{name}.localhost:8080{VITE_SUFFIX.get(p, '')}"]
    src = os.environ.get("CONSOLE_WEB_DIR", str(s.STACK_DIR / ".." / "console-web"))
    s.log(f"console: building {cname} (forked backends -> -{name})")
    s.docker("build", "-f", s.REPO_DIR / "docker/console-web/Dockerfile",
             "--build-context", f"src={src}", *bargs, "-t", img, s.REPO_DIR / "docker/console-web")
    s.docker("run", "-d", "--name", cname, "--network", s.NET, "--restart", "unless-stopped",
             "--label", f"com.docker.compose.project={project}", "--label", "clode.wfork=1",
             "--label", f"clode.fork={name}", "--label", "clode.svc=console-web",
             "--label", "traefik.enable=true",
             "--label", f"traefik.http.routers.{cname}.rule=Host(`{cname}.localhost`)",
             "--label", f"traefik.http.services.{cname}.loadbalancer.server.port=8080",
             img, capture=True)
    s.log(f"  -> http://{cname}.localhost:8080")


# ── commands ──────────────────────────────────────────────────────────────────
def cmd_preview(cfg):
    name, forked = cfg["name"], list(cfg["services"])
    nodes, edges = load_graph()
    fset = set(forked)
    print(f"# fork '{name}' — preview (nothing is applied)\nforked services (run as <svc>-{name}):")
    for svc, m in cfg["services"].items():
        src = f"build({m['branch']})" if m["branch"] else "mirror(baseline image)"
        print(f"  - {svc}   source={src}  db={m['db']}")

    print("\nedges OUT of the fork (what your forked services call):")
    for (a, b), meta in sorted(edges.items()):
        if a not in fset:
            continue
        rw = meta.get("rw", "")
        if b in fset:
            print(f"  ✓ {a} → {b}  routed to {b}-{name}  [{rw}] {meta.get('for', '')}")
        else:
            flag = "  ⚠ WRITE mutates BASELINE" if rw in ("W", "RW") else "  (read — safe fall-through)"
            mark = "⚠" if rw in ("W", "RW") else "·"
            print(f"  {mark} {a} → {b} (baseline){flag}  [{rw}] {meta.get('for', '')}")

    print("\nedges INTO the fork from baseline callers (won't reach the fork unless mirrored):")
    into = [(a, b, meta) for (a, b), meta in sorted(edges.items()) if b in fset and a not in fset]
    for a, b, meta in into:
        print(f"  ⚠ {a} (baseline) → {b}: {meta.get('for', '')}  [add {a} to the fork to exercise this]")
    if not into:
        print("  (none — entry is direct / via console)")

    if cfg["console"]:
        print(f"\nconsole: console-web-{name} built pointing forked backends -> -{name} (rest baseline)")
    print("\nreviewed? then: wfork up")


def cmd_up(cfg):
    name, forked = cfg["name"], list(cfg["services"])
    if not re.match(r"^[a-z0-9][a-z0-9-]*$", name):
        s.die("name must be [a-z0-9-]")
    # Include every profile so profile-gated services (toolkit-proxy=tools,
    # chil=org, …) resolve in the baseline config we read env/caps/command from.
    os.environ["COMPOSE_PROFILES"] = s.compose_profiles()
    base = s.compose_config()                          # baseline resolved env/ports/caps/command
    cfg_services, project = base["services"], base["name"]
    STATE.mkdir(exist_ok=True)

    for svc, m in cfg["services"].items():
        cname = f"{svc}-{name}"
        running = s.docker("ps", "-a", "--format", "{{.Names}}", capture=True).stdout.split()
        if cname in running:
            s.die(f"{cname} exists (wfork down first)")

        if m["branch"]:
            image = f"clode-stack/{svc}:{name}"
            s.log(f"{svc}: building {image} from '{m['branch']}'")
            branch_build(svc, m["branch"], image)
        else:
            image = f"{project}-{svc}:latest"
            s.docker("image", "inspect", image, capture=True, check=False).returncode == 0 or \
                s.die(f"{image} not built (run stack up {svc} first)")
            s.log(f"{svc}: mirror ({image})")

        if m["db"] == "fresh":
            base_db = (cfg_services[svc].get("environment") or {}).get("DB_NAME")
            if base_db:
                fresh_db(svc, name, base_db)

        env = build_env(svc, name, forked, m["db"], cfg_services, m["env"])
        envfile = write_env_file(env)
        run_service(cname, svc, name, image, cfg_services, envfile, project)
        os.unlink(envfile)
        s.log(f"  → http://{cname}.localhost:8080")

    if cfg["console"]:
        console_up(name, cfg["console"]["fork"], project)

    (STATE / f"{name}.applied.json").write_text(json.dumps(cfg, indent=2))
    print()
    s.log(f"fork '{name}' up. down: wfork down --config <file> (or: wfork down {name})")


def cmd_down(name):
    ids = s.docker("ps", "-aq", "--filter", f"label=clode.fork={name}", capture=True).stdout.split()
    if ids:
        s.docker("rm", "-f", *ids, capture=True)
        s.log(f"removed fork '{name}' containers")
    applied = STATE / f"{name}.applied.json"
    if applied.exists():
        cfg = json.loads(applied.read_text())
        fresh = [svc for svc, m in cfg["services"].items() if m["db"] == "fresh"]
        if fresh:
            os.environ["COMPOSE_PROFILES"] = s.compose_profiles()
            dbc, services = s.db_container(), s.compose_config()["services"]
            for svc in fresh:
                base_db = (services.get(svc, {}).get("environment") or {}).get("DB_NAME")
                if base_db:
                    s.docker("exec", dbc, "psql", "-U", "postgres", "-c",
                             f'DROP DATABASE IF EXISTS "{base_db}_{name}"', check=False)
        applied.unlink()
    s.log(f"fork '{name}' down")


def cmd_prune():
    """Tear down EVERY fork: all clode.wfork containers + their fork DBs + fork images."""
    names = {f.name[:-len(".applied.json")] for f in STATE.glob("*.applied.json")}
    labels = s.docker("ps", "-a", "--filter", "label=clode.wfork=1",
                      "--format", '{{.Label "clode.fork"}}', capture=True).stdout.split()
    names.update(x for x in labels if x)
    if not names:
        s.log("no forks to prune")
        return
    for n in sorted(names):
        cmd_down(n)  # removes containers + fork DBs + the applied spec
    # drop fork-specific images (branch builds + fork consoles); never baseline mirrors
    imgs = s.docker("images", "--format", "{{.Repository}}:{{.Tag}}", capture=True).stdout.split()
    tiers = {"latest", "dev", "vm", "voice", "slim"}
    fork_imgs = [i for i in imgs
                 if i.startswith("clode-console-web-")
                 or (i.startswith("clode-stack/") and i.rsplit(":", 1)[-1] in names)]
    if fork_imgs:
        s.docker("rmi", *fork_imgs, capture=True, check=False)
        s.log(f"removed {len(fork_imgs)} fork image(s)")
    s.log(f"pruned {len(names)} fork(s)")


def cmd_ls():
    s.docker("ps", "-a", "--filter", "label=clode.wfork=1",
             "--format", 'table {{.Label "clode.fork"}}\t{{.Names}}\t{{.Status}}')


def main():
    ap = argparse.ArgumentParser(prog="wfork", description="within-network workspace fork (config-driven)")
    sub = ap.add_subparsers(dest="cmd", required=True)
    for c in ("preview", "up", "down"):
        p = sub.add_parser(c)
        p.add_argument("--config", help="fork.<name>.yaml")
        if c == "down":
            p.add_argument("name", nargs="?", help="fork name (alternative to --config)")
    sub.add_parser("ls")
    sub.add_parser("prune")   # tear down ALL forks
    a = ap.parse_args()

    if a.cmd == "ls":
        return cmd_ls()
    if a.cmd == "prune":
        return cmd_prune()
    if a.cmd == "down":
        name = load_config(a.config)["name"] if a.config else a.name
        return cmd_down(name or s.die("down: need --config <file> or a fork name"))
    a.config or s.die(f"{a.cmd}: requires --config <file>")
    Path(a.config).exists() or s.die(f"config not found: {a.config}")
    cfg = load_config(a.config)
    (cmd_preview if a.cmd == "preview" else cmd_up)(cfg)


if __name__ == "__main__":
    main()
