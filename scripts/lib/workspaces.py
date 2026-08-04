"""workspaces — resolve per-service BUILD CONTEXT overrides from
clode-stack/workspaces.yaml. Imported by up.py and gen-build-cache.

WHAT IT DOES. Normally every service builds from its main sibling repo
(``../<svc>``). workspaces.yaml lets you point selected services at a git
WORKTREE instead — so the code being built comes from a feature branch's
checkout while everything else stays on main. It moves CODE only: each
service's ``env_file`` still loads from its MAIN repo ``.env``, so config is
never taken from the worktree.

CONFIG FILE (clode-stack/workspaces.yaml) — flat ``service: selector`` pairs.
YAML ``#`` comments make worktree-hopping a one-character edit: comment a
line out and that service snaps back to main.

    brahmi: feat/persona-tests            # by branch name
    raksha: .claude/worktrees/sa-endpoints  # by path (relative to ../raksha)
    # jumbo: feat/x                        # commented out -> builds from main

Selector semantics per value:
    ""  | "main" | "."   -> the primary checkout (../<svc>) — same as omitting it
    "<branch>"           -> matched against ``git worktree list`` for that repo;
                            resolves to that worktree's absolute path
    "/abs/path"          -> used verbatim
    "rel/path"           -> treated as a path under ../<svc>

``.yml`` is accepted as an alias for ``.yaml``.

HOW IT PLUGS IN. resolve_workspaces exports ``<SVC>_DIR`` (brahmi->BRAHMI_DIR,
pool-manager->POOL_MANAGER_DIR, mock-services->MOCK_SERVICES_DIR, …) — the exact var the
compose ``build.context: ${<SVC>_DIR:-../<svc>}`` interpolates, and the var
gen-build-cache reads to find each Dockerfile. An already-set env var wins, so
``BRAHMI_DIR=/some/path ./stack.sh up`` overrides the file for one run.

Populated module state, keyed by service name:
    WS_DIR[svc]    resolved build context
    WS_LABEL[svc]  human label (branch, "(primary)" suffix for main)
    WS_STATUS[svc] "ok" | "MISSING"  (MISSING = no Dockerfile at the context)
    WS_ANY         True if any entry resolves to something other than ../<svc>

``resolve_workspaces()`` returns ``{svc: {"dir":…, "label":…, "status":…}}``.
"""
import os
import sys
import subprocess
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import stacklib as s  # noqa: E402

# Services whose default context is NOT ../<service-name>. The compose file
# is the source of truth for these defaults; the only current mismatch is
# mock-services, built from ./docker/mock-services (inside clode-stack).
_WS_DEFAULT_CTX = {
    "mock-services": "./docker/mock-services",
}

# Services whose project marker is NOT a repo-root Dockerfile/package.json —
# a checkout is "ok" only when this path (relative to the resolved context)
# exists. agent-base-docker builds the brave-head image from its brave-headed
# subdir, so the repo root has no Dockerfile of its own. benji-state and
# aramb-skills aren't compose services or built images — they're data-source
# checkouts consumed by `up --state=build` — so their marker is a defining file
# (validates the path is a real checkout, not a stale one).
_WS_MARKER = {
    "agent-base-docker": "brave-headed/Dockerfile",
    "benji-state": "agent-skills.yaml",
    "aramb-skills": "aramb-chat/SKILL.md",
}

# Populated by resolve_workspaces (module globals so print_workspace_table can
# be called after it, exactly like the sourced-bash version).
WS_DIR = {}
WS_LABEL = {}
WS_STATUS = {}
WS_ANY = False
WORKSPACES_FILE = "workspaces.yaml"


def _workspaces_file() -> str:
    """Honor an explicit WORKSPACES_FILE, else the first of the conventional
    names that exists (default target for messages: workspaces.yaml)."""
    env = os.environ.get("WORKSPACES_FILE")
    if env:
        return env
    if (s.STACK_DIR / "workspaces.yaml").is_file():
        return "workspaces.yaml"
    if (s.STACK_DIR / "workspaces.yml").is_file():
        return "workspaces.yml"
    return "workspaces.yaml"


def _ctx_path(ctx: str) -> Path:
    """Absolutize a build-context string. Relative contexts (``../<svc>`` and
    the like) resolve against the canonical stack checkout, the same dir the
    compose ``--project-directory`` interpolates them against."""
    p = Path(ctx)
    return p if p.is_absolute() else (s.STACK_DIR / p)


def _ws_var(svc: str) -> str:
    """service name -> env var name (pool-manager -> POOL_MANAGER_DIR)."""
    return svc.upper().replace("-", "_") + "_DIR"


def _ws_base(svc: str) -> str:
    """service name -> default build context."""
    return _WS_DEFAULT_CTX.get(svc, f"../{svc}")


def _ws_parse(path: Path):
    """Parse the flat YAML into (key, value) pairs. Handles ``#`` comments
    (full line and inline per the YAML "space before #" rule), quoted values,
    blank lines, and ``---``; warns (stderr) on a non-empty line without a
    colon. Only stdlib — no yq/PyYAML/jq needed."""
    pairs = []
    with open(path) as f:
        for raw in f:
            line = raw.rstrip("\n").rstrip("\r")
            out, prev = [], " "
            for ch in line:                       # strip comment
                if ch == "#" and (prev in " \t"):
                    break
                out.append(ch)
                prev = ch
            line = "".join(out).strip()
            if not line or line in ("---", "..."):
                continue
            if ":" not in line:
                sys.stderr.write(
                    f"warn: ignoring malformed line in {path}: {raw.strip()!r}\n")
                continue
            k, v = line.split(":", 1)
            k = k.strip().strip('"').strip("'")
            v = v.strip()
            if len(v) >= 2 and v[0] == v[-1] and v[0] in ("'", '"'):
                v = v[1:-1]
            if k:
                pairs.append((k, v))
    return pairs


def _git(base_dir: Path, *args) -> subprocess.CompletedProcess:
    return subprocess.run(
        ["git", "-C", str(base_dir), *args],
        text=True, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)


def _ws_resolve_one(svc: str, val: str) -> str:
    """Resolve a single selector value to a build-context path."""
    base = _ws_base(svc)
    val = val.lstrip()                            # trim leading whitespace
    if val == "" or val == "main" or val == ".":
        return base
    if val.startswith("/"):
        return val
    # branch match against the repo's worktrees
    base_dir = _ctx_path(base)
    if _git(base_dir, "rev-parse", "--git-dir").returncode == 0:
        wt = _git(base_dir, "worktree", "list", "--porcelain")
        path = None
        cur = None
        for line in wt.stdout.splitlines():
            if line.startswith("worktree "):
                cur = line.split()[1]
            elif line.startswith("branch "):
                if line.split()[1] == f"refs/heads/{val}" and path is None:
                    path = cur
        if path:
            return path
    # otherwise a path relative to the main repo
    return f"{base}/{val}"


def _ws_label(svc: str, dir_: str) -> str:
    """Human-readable branch/source label for the table."""
    base = _ws_base(svc)
    r = _git(_ctx_path(dir_), "rev-parse", "--abbrev-ref", "HEAD")
    branch = r.stdout.strip() if r.returncode == 0 else ""
    if not branch:
        branch = "(no git)"
    if os.path.realpath(_ctx_path(dir_)) == os.path.realpath(_ctx_path(base)):
        return f"{branch} (primary)"
    return branch


def resolve_workspaces() -> dict:
    """Parse workspaces.yaml, populate the module globals, export ``<SVC>_DIR``,
    and return ``{svc: {"dir":…, "label":…, "status":…}}``."""
    global WS_DIR, WS_LABEL, WS_STATUS, WS_ANY, WORKSPACES_FILE
    WS_DIR, WS_LABEL, WS_STATUS, WS_ANY = {}, {}, {}, False
    WORKSPACES_FILE = _workspaces_file()
    wf = _ctx_path(WORKSPACES_FILE)
    if not wf.is_file():
        return {}
    for svc, val in _ws_parse(wf):
        if not svc or svc.startswith("_"):
            continue
        base = _ws_base(svc)
        var = _ws_var(svc)
        if os.environ.get(var):
            dir_ = os.environ[var]                 # explicit env var wins
        else:
            dir_ = _ws_resolve_one(svc, val)
            os.environ[var] = dir_
        WS_DIR[svc] = dir_
        WS_LABEL[svc] = _ws_label(svc, dir_)
        # A valid checkout has a project marker: a per-service override in
        # _WS_MARKER (nested Dockerfile), else a Dockerfile (built services) or
        # a package.json (bind-mounted dev servers like console-web) at the root.
        marker = _WS_MARKER.get(svc, "")
        d = _ctx_path(dir_)
        if marker:
            WS_STATUS[svc] = "ok" if (d.is_dir() and (d / marker).is_file()) else "MISSING"
        elif d.is_dir() and ((d / "Dockerfile").is_file() or (d / "package.json").is_file()):
            WS_STATUS[svc] = "ok"
        else:
            WS_STATUS[svc] = "MISSING"
        if os.path.realpath(d) != os.path.realpath(_ctx_path(base)):
            WS_ANY = True
    return {svc: {"dir": WS_DIR[svc], "label": WS_LABEL[svc], "status": WS_STATUS[svc]}
            for svc in WS_DIR}


def _ws_trunc(text: str, w: int) -> str:
    """Left-truncate with a leading ellipsis so the informative tail survives."""
    if len(text) > w:
        return "…" + text[-(w - 1):]
    return text


def print_workspace_table() -> None:
    """Print a loud, well-spaced table of the active overrides. Safe to call
    after resolve_workspaces (which may have populated nothing)."""
    line = "=========================================================================================="
    if len(WS_DIR) == 0:
        print("\n  workspace overrides: none — all services build from their main repos (../<svc>)\n")
        return
    print()
    print(line)
    print(f"  WORKSPACE OVERRIDES   (clode-stack/{WORKSPACES_FILE})")
    print("  code builds from these checkouts; env still loads from each service's main-repo .env")
    print(line)
    print(f"  {'SERVICE':<16}  {'BRANCH / SOURCE':<24}  {'BUILD CONTEXT':<34}  {'STATUS'}")
    print(f"  {'----------------':<16}  {'------------------------':<24}  "
          f"{'----------------------------------':<34}  {'------'}")
    for svc in sorted(WS_DIR):
        print(f"  {_ws_trunc(svc, 16):<16}  {_ws_trunc(WS_LABEL[svc], 24):<24}  "
              f"{_ws_trunc(WS_DIR[svc], 34):<34}  {WS_STATUS[svc]}")
    print(line)
    print()
