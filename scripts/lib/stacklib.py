"""Shared helpers for clode-stack Python orchestration scripts.

Thin wrappers over `docker` / `docker compose` so each script reads as a short
list of steps instead of hand-rolled bash. stdlib only (pyyaml is imported by the
scripts that read configs).

Two directories matter:
  REPO_DIR  — where the compose files live (this checkout; a worktree during dev)
  STACK_DIR — the canonical checkout whose ../<svc> siblings + .env resolve
              (override with CLODE_STACK_DIR when running from a worktree)
"""
import os
import sys
import json
import time
import ssl
import datetime
import subprocess
import urllib.request
import urllib.error
from pathlib import Path

REPO_DIR = Path(os.environ.get("CLODE_REPO_DIR")
                or Path(__file__).resolve().parents[2]).resolve()  # scripts/lib/stacklib.py -> repo root
STACK_DIR = Path(os.environ.get("CLODE_STACK_DIR", REPO_DIR)).resolve()
NET = os.environ.get("CLODE_NET", "clode")                 # the docker network forks/sweeps attach to


def log(msg: str) -> None:
    print(f"==> {msg}", flush=True)


def warn(msg: str) -> None:
    print(f"warn: {msg}", file=sys.stderr, flush=True)


def die(msg: str, code: int = 2):
    print(f"error: {msg}", file=sys.stderr)
    raise SystemExit(code)


def run(args, *, capture=False, check=True, env=None, stdin=None, cwd=None):
    """Run a command. capture=True returns stdout; check=True dies on failure."""
    r = subprocess.run(
        [str(a) for a in args], text=True, input=stdin, cwd=cwd,
        env={**os.environ, **(env or {})},
        stdout=subprocess.PIPE if capture else None,
        stderr=subprocess.PIPE if capture else None,
    )
    if check and r.returncode != 0:
        if capture and r.stderr:
            sys.stderr.write(r.stderr)
        die(f"command failed ({r.returncode}): {' '.join(str(a) for a in args)}")
    return r


def compose_files():
    """-f flags: base + cache (if present) + limits (unless NO_LIMITS)."""
    files = ["-f", REPO_DIR / "docker-compose.yml"]
    cache = REPO_DIR / "docker-compose.cache.yml"
    if cache.exists():
        files += ["-f", cache]
    limits = REPO_DIR / "docker-compose.limits.yml"
    if limits.exists() and not os.environ.get("NO_LIMITS"):
        files += ["-f", limits]
    return files


def compose(*args, project=None, **kw):
    cmd = ["docker", "compose", "--project-directory", STACK_DIR, *compose_files()]
    if project:
        cmd += ["-p", project]
    return run([*cmd, *args], **kw)


def compose_config() -> dict:
    """The fully-resolved compose config (env_file inlined, anchors merged)."""
    return json.loads(compose("config", "--format", "json", capture=True).stdout)


def docker(*args, **kw):
    return run(["docker", *args], **kw)


def db_container() -> str:
    """The postgres container on our network (NET). Scoping to NET keeps this correct
    when another stack (baseline vs a test bed on a different network) runs alongside."""
    names = docker("ps", "--filter", f"network={NET}", "--format", "{{.Names}}",
                   capture=True, check=False).stdout.split()
    for n in names:
        if n.endswith("-db-1"):
            return n
    return "clode-db-1"


# ── HTTP (replaces curl) ──────────────────────────────────────────────────────
def http(method, url, *, data=None, headers=None, timeout=5, insecure=True):
    """Minimal request. Returns (status, body_text). Never raises on HTTP/network
    errors — status 0 means the request didn't complete (like `curl --fail` failing)."""
    if isinstance(data, str):
        data = data.encode()
    req = urllib.request.Request(url, data=data, method=method, headers=headers or {})
    ctx = ssl._create_unverified_context() if insecure else None
    try:
        with urllib.request.urlopen(req, timeout=timeout, context=ctx) as r:
            return r.status, r.read().decode(errors="replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode(errors="replace")
    except Exception:
        return 0, ""


def get_json(url, **kw):
    """GET and parse JSON, or None on non-2xx / empty / parse error."""
    status, body = http("GET", url, **kw)
    if 200 <= status < 300 and body:
        try:
            return json.loads(body)
        except ValueError:
            return None
    return None


def wait_healthy(url, *, tries=60, delay=1.0, ok=lambda s: 200 <= s < 400) -> bool:
    """Poll url until healthy (default 2xx/3xx, like `curl -sf`); True if it came up."""
    for _ in range(tries):
        status, _b = http("GET", url)
        if ok(status):
            return True
        time.sleep(delay)
    return False


# ── postgres (replaces the `docker exec … psql` boilerplate) ──────────────────
def psql(dbname, sql=None, *, args=None, capture=False, check=True):
    """Run SQL (via stdin) in the baseline postgres. ON_ERROR_STOP=1, PGPASSWORD set."""
    cmd = ["exec", "-i", "-e", "PGPASSWORD=postgres", db_container(),
           "psql", "-U", "postgres", "-v", "ON_ERROR_STOP=1", "-d", dbname]
    if args:
        cmd += args
    return docker(*cmd, stdin=sql, capture=capture, check=check)


def db_exists(dbname) -> bool:
    out = psql("postgres", f"SELECT 1 FROM pg_database WHERE datname='{dbname}'",
               capture=True, check=False).stdout
    return "1" in out


# ── docker helpers ────────────────────────────────────────────────────────────
def containers(*filters, all=True):
    """Container IDs matching --filter args (ids, not names)."""
    args = ["ps", "-aq" if all else "-q"]
    for f in filters:
        args += ["--filter", f]
    return docker(*args, capture=True, check=False).stdout.split()


def image_exists(image) -> bool:
    return docker("image", "inspect", image, capture=True, check=False).returncode == 0


def lines(cp):
    """Non-empty stripped lines of a captured CompletedProcess's stdout."""
    return [ln for ln in cp.stdout.splitlines() if ln.strip()]


# ── compose helpers ───────────────────────────────────────────────────────────
def compose_bare(*args, **kw):
    """Base compose only — NO cache/limits overlays — but with --project-directory so
    sibling paths (../<svc>, .env) still resolve. Use where the overlays would change
    which profiles/services are seen (down/tail-logs discovery, config --profiles)."""
    return run(["docker", "compose", "--project-directory", STACK_DIR,
                "-f", REPO_DIR / "docker-compose.yml", *args], **kw)


def compose_profiles() -> str:
    """All defined profiles as a CSV (for COMPOSE_PROFILES=… to reach every profile)."""
    out = compose_bare("config", "--profiles", capture=True, check=False).stdout
    return ",".join(out.split())


def utc_stamp() -> str:
    """UTC filename stamp, e.g. 20260803T061500Z."""
    return datetime.datetime.now(datetime.timezone.utc).strftime("%Y%m%dT%H%M%SZ")
