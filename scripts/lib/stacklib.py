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
import subprocess
from pathlib import Path

REPO_DIR = Path(__file__).resolve().parents[2]             # scripts/lib/stacklib.py -> repo root
STACK_DIR = Path(os.environ.get("CLODE_STACK_DIR", REPO_DIR)).resolve()
NET = "clode"


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
    """The baseline postgres container name."""
    names = docker("ps", "--format", "{{.Names}}", capture=True).stdout.split()
    for n in names:
        if n == "clode-db-1" or n.endswith("-db-1"):
            return n
    return "clode-db-1"
