# workspaces.sh — resolve per-service BUILD CONTEXT overrides from
# clode-stack/workspaces.yaml. Sourced by up.sh and gen-build-cache.sh.
#
# WHAT IT DOES. Normally every service builds from its main sibling repo
# (`../<svc>`). workspaces.yaml lets you point selected services at a git
# WORKTREE instead — so the code being built comes from a feature branch's
# checkout while everything else stays on main. It moves CODE only: each
# service's `env_file` still loads from its MAIN repo `.env`, so config is
# never taken from the worktree.
#
# CONFIG FILE (clode-stack/workspaces.yaml) — flat `service: selector` pairs.
# YAML `#` comments make worktree-hopping a one-character edit: comment a
# line out and that service snaps back to main.
#
#   brahmi: feat/persona-tests            # by branch name
#   raksha: .claude/worktrees/sa-endpoints  # by path (relative to ../raksha)
#   # jumbo: feat/x                        # commented out → builds from main
#
# Selector semantics per value:
#   ""  | "main" | "."   → the primary checkout (../<svc>) — same as omitting it
#   "<branch>"           → matched against `git worktree list` for that repo;
#                          resolves to that worktree's absolute path
#   "/abs/path"          → used verbatim
#   "rel/path"           → treated as a path under ../<svc>
#
# Only python3 is required to parse it (already a stack dependency via
# gen-build-cache.sh) — no yq / PyYAML / jq needed, so it behaves the same
# on every machine. `.yml` is accepted as an alias for `.yaml`.
#
# HOW IT PLUGS IN. resolve_workspaces exports `<SVC>_DIR` (brahmi→BRAHMI_DIR,
# pool-manager→POOL_MANAGER_DIR, ec2mock→EC2MOCK_DIR, …) — the exact var the
# compose `build.context: ${<SVC>_DIR:-../<svc>}` interpolates, and the var
# gen-build-cache.sh reads to find each Dockerfile. An already-set env var
# wins, so `BRAHMI_DIR=/some/path ./stack.sh up` overrides the file for one run.
#
# Populated globals (associative arrays), keyed by service name:
#   WS_DIR[svc]    resolved build context
#   WS_LABEL[svc]  human label (branch, "(primary)" suffix for main)
#   WS_STATUS[svc] "ok" | "MISSING"  (MISSING = no Dockerfile at the context)
#   WS_ANY         1 if any entry resolves to something other than ../<svc>

# Config file: honor an explicit WORKSPACES_FILE, else the first of the
# conventional names that exists (default target for messages: workspaces.yaml).
if [[ -z "${WORKSPACES_FILE:-}" ]]; then
  if   [[ -f workspaces.yaml ]]; then WORKSPACES_FILE="workspaces.yaml"
  elif [[ -f workspaces.yml  ]]; then WORKSPACES_FILE="workspaces.yml"
  else WORKSPACES_FILE="workspaces.yaml"
  fi
fi

declare -gA WS_DIR WS_LABEL WS_STATUS
declare -g WS_ANY=0

# Services whose default context is NOT ../<service-name>. The compose file
# is the source of truth for these defaults; the only current mismatch is
# ec2mock, built from ../ec2-docker-mock.
declare -gA _WS_DEFAULT_CTX=(
  [ec2mock]="../ec2-docker-mock"
)

# Services whose project marker is NOT a repo-root Dockerfile/package.json —
# a checkout is "ok" only when this path (relative to the resolved context)
# exists. agent-base-docker builds the brave-head image from its brave-headed
# subdir, so the repo root has no Dockerfile of its own.
declare -gA _WS_MARKER=(
  [agent-base-docker]="brave-headed/Dockerfile"
)

# service name → env var name (pool-manager → POOL_MANAGER_DIR)
_ws_var() { echo "$(echo "$1" | tr 'a-z-' 'A-Z_')_DIR"; }

# service name → default build context
_ws_base() { echo "${_WS_DEFAULT_CTX[$1]:-../$1}"; }

# Parse the flat YAML into <key>\t<value> lines. Handles `#` comments (full
# line and inline per the YAML "space before #" rule), quoted values, blank
# lines, and `---`; warns (stderr) on a non-empty line without a colon.
_ws_parse() {
  python3 - "$1" <<'PY'
import sys
path = sys.argv[1]
with open(path) as f:
    for raw in f:
        line = raw.rstrip("\n").rstrip("\r")
        out, prev = [], " "
        for ch in line:                       # strip comment
            if ch == "#" and (prev in " \t"):
                break
            out.append(ch); prev = ch
        line = "".join(out).strip()
        if not line or line in ("---", "..."):
            continue
        if ":" not in line:
            sys.stderr.write(f"warn: ignoring malformed line in {path}: {raw.strip()!r}\n")
            continue
        k, v = line.split(":", 1)
        k = k.strip().strip('"').strip("'")
        v = v.strip()
        if len(v) >= 2 and v[0] == v[-1] and v[0] in ("'", '"'):
            v = v[1:-1]
        if k:
            print(f"{k}\t{v}")
PY
}

# Resolve a single selector value to a build-context path.
_ws_resolve_one() {
  local svc=$1 val=$2 base path
  base=$(_ws_base "$svc")
  # trim leading whitespace
  val="${val#"${val%%[![:space:]]*}"}"
  if [[ -z "$val" || "$val" == "main" || "$val" == "." ]]; then
    echo "$base"; return
  fi
  if [[ "$val" == /* ]]; then
    echo "$val"; return
  fi
  # branch match against the repo's worktrees
  if git -C "$base" rev-parse --git-dir >/dev/null 2>&1; then
    path=$(git -C "$base" worktree list --porcelain 2>/dev/null \
      | awk -v b="refs/heads/$val" '/^worktree /{p=$2} /^branch /{if ($2==b) print p}' \
      | head -1)
    if [[ -n "$path" ]]; then echo "$path"; return; fi
  fi
  # otherwise a path relative to the main repo
  echo "$base/$val"
}

# Human-readable branch/source label for the table.
_ws_label() {
  local svc=$1 dir=$2 base branch
  base=$(_ws_base "$svc")
  branch=$(git -C "$dir" rev-parse --abbrev-ref HEAD 2>/dev/null || true)
  [[ -z "$branch" ]] && branch="(no git)"
  if [[ "$(realpath -m "$dir" 2>/dev/null)" == "$(realpath -m "$base" 2>/dev/null)" ]]; then
    echo "$branch (primary)"
  else
    echo "$branch"
  fi
}

# Parse workspaces.yaml and populate the globals + export <SVC>_DIR.
resolve_workspaces() {
  WS_DIR=(); WS_LABEL=(); WS_STATUS=(); WS_ANY=0
  [[ -f "$WORKSPACES_FILE" ]] || return 0
  local svc val var base dir
  while IFS=$'\t' read -r svc val; do
    [[ -z "$svc" || "$svc" == _* ]] && continue
    base=$(_ws_base "$svc")
    var=$(_ws_var "$svc")
    if [[ -n "${!var:-}" ]]; then
      dir="${!var}"                       # explicit env var wins
    else
      dir=$(_ws_resolve_one "$svc" "$val")
      export "$var=$dir"
    fi
    WS_DIR[$svc]="$dir"
    WS_LABEL[$svc]=$(_ws_label "$svc" "$dir")
    # A valid checkout has a project marker: a per-service override in
    # _WS_MARKER (nested Dockerfile), else a Dockerfile (built services) or a
    # package.json (bind-mounted dev servers like console-web) at the root.
    local marker="${_WS_MARKER[$svc]:-}"
    if [[ -n "$marker" ]]; then
      [[ -d "$dir" && -f "$dir/$marker" ]] && WS_STATUS[$svc]="ok" || WS_STATUS[$svc]="MISSING"
    elif [[ -d "$dir" && ( -f "$dir/Dockerfile" || -f "$dir/package.json" ) ]]; then
      WS_STATUS[$svc]="ok"
    else
      WS_STATUS[$svc]="MISSING"
    fi
    if [[ "$(realpath -m "$dir" 2>/dev/null)" != "$(realpath -m "$base" 2>/dev/null)" ]]; then
      WS_ANY=1
    fi
  done < <(_ws_parse "$WORKSPACES_FILE")
  return 0
}

# Left-truncate with a leading ellipsis so the informative tail survives.
_ws_trunc() {
  local s=$1 w=$2
  if (( ${#s} > w )); then echo "…${s: -$((w-1))}"; else echo "$s"; fi
}

# Print a loud, well-spaced table of the active overrides. Safe to call
# after resolve_workspaces (which may have populated nothing).
print_workspace_table() {
  local line="=========================================================================================="
  if (( ${#WS_DIR[@]} == 0 )); then
    printf '\n  workspace overrides: none — all services build from their main repos (../<svc>)\n\n'
    return 0
  fi
  printf '\n%s\n' "$line"
  printf '  WORKSPACE OVERRIDES   (clode-stack/%s)\n' "$WORKSPACES_FILE"
  printf '  code builds from these checkouts; env still loads from each service'\''s main-repo .env\n'
  printf '%s\n' "$line"
  printf '  %-16s  %-24s  %-34s  %s\n' "SERVICE" "BRANCH / SOURCE" "BUILD CONTEXT" "STATUS"
  printf '  %-16s  %-24s  %-34s  %s\n' "----------------" "------------------------" "----------------------------------" "------"
  local svc
  for svc in $(printf '%s\n' "${!WS_DIR[@]}" | sort); do
    printf '  %-16s  %-24s  %-34s  %s\n' \
      "$(_ws_trunc "$svc" 16)" \
      "$(_ws_trunc "${WS_LABEL[$svc]}" 24)" \
      "$(_ws_trunc "${WS_DIR[$svc]}" 34)" \
      "${WS_STATUS[$svc]}"
  done
  printf '%s\n\n' "$line"
}
