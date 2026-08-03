#!/usr/bin/env bash
# wfork.sh — WITHIN-NETWORK workspace fork, driven ENTIRELY by a config file.
#
# A fork is declared in one reviewable YAML and applied atomically. There is no
# per-service invocation — the config is the single source of truth, so what runs
# is exactly what the file says (easy to review, deterministic, no interleaved
# build/up commands).
#
#   wfork preview --config fork.<name>.yaml   dry-run: forked set + boundary report + ⚠WRITE warnings
#   wfork up      --config fork.<name>.yaml   apply: build/mirror + peer-rewrite + DB + console (atomic)
#   wfork down    --config fork.<name>.yaml   tear the fork down (containers + fresh DBs)
#   wfork ls                                  list running forks
#
# Config schema (see docs/parallel-stacks.md):
#   name: b1
#   services:
#     brahmi:  { branch: feat/x, db: reuse }   # branch → build clode-stack/brahmi:b1
#     gateway: { mirror: true }                # baseline image, run as gateway-b1
#   console: true                              # build console-b1 → forked backends
#
# Each fork service runs as <svc>-<name> on the `clode` network, reached at
# <svc>-<name>.localhost:8080. Env is lifted from baseline, then: peers that are
# ALSO in the fork are rewritten to <peer>-<name>; unlisted peers fall through to
# baseline. db: reuse (default) keeps the baseline DB; db: fresh makes <svc>_<name>
# (schema-copied). console builds a static console pointing at the forked backends.

set -euo pipefail
SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPTS_DIR/.."
REPO_DIR="$PWD"
STACK_DIR="${CLODE_STACK_DIR:-$PWD}"
NET=clode
GRAPH="$SCRIPTS_DIR/lib/service-graph.json"
STATE_DIR="$STACK_DIR/.forks"

die()  { echo "wfork: $*" >&2; exit 2; }
info() { echo "==> $*"; }

# service -> the VITE_* var the SPA reads (+ path suffix) for the console build
declare -A _VITE_VAR=(
  [aramb-gateway]=VITE_GATEWAY_BASE_URL   [raksha]=VITE_RAKSHA_BASE_URL
  [brahmi]=VITE_BRAHMI_BASE_URL           [jumbo]=VITE_JUMBO_BASE_URL
  [cha-ching]=VITE_CHACHING_BASE_URL      [toolkit-proxy]=VITE_TOOLKIT_PROXY_BASE_URL
  [skills-registry]=VITE_SKILLS_REGISTRY_BASE_URL [ikki]=VITE_IKKI_BASE_URL )
declare -A _VITE_SUFFIX=( [jumbo]=/api/v1 [cha-ching]=/api/v1 [toolkit-proxy]=/api/v1 )

_compose_files() {
  local f=(-f "$REPO_DIR/docker-compose.yml")
  [[ -f "$REPO_DIR/docker-compose.cache.yml" ]] && f+=(-f "$REPO_DIR/docker-compose.cache.yml")
  [[ -z "${NO_LIMITS:-}" && -f "$REPO_DIR/docker-compose.limits.yml" ]] && f+=(-f "$REPO_DIR/docker-compose.limits.yml")
  printf '%s\n' "${f[@]}"
}
_config_json() {
  mapfile -t _cf < <(_compose_files)
  docker compose --project-directory "$STACK_DIR" "${_cf[@]}" config --format json
}

# Parse + normalize the fork config to JSON on stdout.
_parse_config() {
  python3 - "$1" <<'PY'
import sys, yaml, json
c = yaml.safe_load(open(sys.argv[1])) or {}
name = c.get("name")
if not name: sys.exit("config: missing 'name'")
svcs = {}
for s, m in (c.get("services") or {}).items():
    m = m or {}
    branch = m.get("branch")
    svcs[s] = {"branch": branch,
               "mirror": bool(m.get("mirror")) or branch is None,
               "db": m.get("db", "reuse"),
               "env": m.get("env") or {}}
console = c.get("console")
if console is True: console = {"fork": list(svcs)}
elif not console:  console = None
elif isinstance(console, dict): console.setdefault("fork", list(svcs))
print(json.dumps({"name": name, "services": svcs,
                  "forked": list(svcs), "console": console}))
PY
}

_q()  { printf '%s' "$1" | python3 -c "import sys,json;print(json.load(sys.stdin)$2)"; }        # scalar
_qj() { printf '%s' "$1" | python3 -c "import sys,json;print(json.dumps(json.load(sys.stdin)$2))"; }  # list/dict as JSON

_db_container() { docker ps --format '{{.Names}}' | grep -m1 -E '(^|-)db(-1)?$' || echo clode-db-1; }

# ─────────────────────────────────────────────────────────────── preview ───
cmd_preview() {
  local cfg; cfg="$(_get_cfg "$@")"
  local spec; spec="$(_parse_config "$cfg")"
  local name; name="$(_q "$spec" '["name"]')"
  mapfile -t forked < <(_qj "$spec" '["forked"]' | tr -d '[]"' | tr ',' '\n' | sed 's/ //g')
  echo "# fork '$name' — preview (nothing is applied)"
  echo "forked services (run as <svc>-$name):"
  local s
  for s in "${forked[@]}"; do
    [[ -z "$s" ]] && continue
    local src db; src="$(_q "$spec" "[\"services\"][\"$s\"][\"branch\"]")"; db="$(_q "$spec" "[\"services\"][\"$s\"][\"db\"]")"
    [[ "$src" == None ]] && src="mirror(baseline image)" || src="build($src)"
    echo "  - $s   source=$src  db=$db"
  done
  echo
  # graph-driven boundary report
  FORKED_CSV="$(IFS=,; echo "${forked[*]}")" python3 - "$GRAPH" <<'PY'
import sys, json, os
g = json.load(open(sys.argv[1]))
forked = set(x for x in os.environ["FORKED_CSV"].split(",") if x)
nodes = {k: v for k, v in g.items() if not k.startswith("_")}
edges = {(a, b): meta for a, m in nodes.items() for b, meta in (m.get("calls") or {}).items()}
print("edges OUT of the fork (what your forked services call):")
for (a, b), meta in sorted(edges.items()):
    if a in forked:
        rw = meta.get("rw", "")
        if b in forked:
            print(f"  ✓ {a} → {b}  routed to {b}-fork  [{rw}] {meta.get('for','')}")
        else:
            warn = "  ⚠ WRITE mutates BASELINE" if rw in ("W", "RW") else "  (read — safe fall-through)"
            print(f"  {'⚠' if rw in ('W','RW') else '·'} {a} → {b} (baseline){warn}  [{rw}] {meta.get('for','')}")
print("\nedges INTO the fork from baseline callers (won't reach your fork unless mirrored):")
any_in = False
for (a, b), meta in sorted(edges.items()):
    if b in forked and a not in forked:
        any_in = True
        print(f"  ⚠ {a} (baseline) → {b}: {meta.get('for','')}  [add {a} to the fork to exercise this path]")
if not any_in:
    print("  (none — entry is direct / via console)")
PY
  local console; console="$(_q "$spec" '["console"]')"
  [[ "$console" != None ]] && echo -e "\nconsole: console-web-$name built pointing forked backends → -$name (rest baseline)"
  echo -e "\nreview the above, then: stack wfork up --config $cfg"
}

# ──────────────────────────────────────────────────────────────────── up ───
cmd_up() {
  local cfg; cfg="$(_get_cfg "$@")"
  local spec; spec="$(_parse_config "$cfg")"
  local name; name="$(_q "$spec" '["name"]')"
  [[ "$name" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "name must be [a-z0-9-]"
  mapfile -t forked < <(_qj "$spec" '["forked"]' | tr -d '[]"' | tr ',' '\n' | sed 's/ //g')
  local cfgjson; cfgjson="$(_config_json)"     # baseline resolved config (env/ports/limits)
  mkdir -p "$STATE_DIR"
  local forked_csv; forked_csv="$(IFS=,; echo "${forked[*]}")"

  local s
  for s in "${forked[@]}"; do
    [[ -z "$s" ]] && continue
    local cname="${s}-${name}"
    docker ps -a --format '{{.Names}}' | grep -qx "$cname" && die "$cname exists (wfork down first)"
    local branch db image
    branch="$(_q "$spec" "[\"services\"][\"$s\"][\"branch\"]")"
    db="$(_q "$spec" "[\"services\"][\"$s\"][\"db\"]")"

    if [[ "$branch" == None ]]; then
      image="clode-${s}:latest"                 # mirror: baseline image
      docker image inspect "$image" >/dev/null 2>&1 || die "$image not built (run stack up $s first)"
      info "$s: mirror ($image)"
    else
      image="clode-stack/${s}:${name}"          # branch build from worktree
      info "$s: building $image from '$branch'"
      _branch_build "$s" "$branch" "$image"
    fi

    # env: lift baseline, rewrite forked peers, apply db + overrides
    local envfile; envfile="$(mktemp)"
    _svc_env "$s" "$name" "$forked_csv" "$db" "$cfgjson" \
      "$(_qj "$spec" "[\"services\"][\"$s\"][\"env\"]")" > "$envfile"
    local port; port="$(_svc_port "$s" "$cfgjson")"
    _run "$cname" "$s" "$name" "$image" "$port" "$envfile" "$cfgjson"
    rm -f "$envfile"
    info "  → http://${cname}.localhost:8080"
  done

  # console (build with forked backends baked)
  local console; console="$(_q "$spec" '["console"]')"
  if [[ "$console" != None ]]; then
    mapfile -t cfork < <(_qj "$spec" '["console"]["fork"]' | tr -d '[]"' | tr ',' '\n' | sed 's/ //g')
    _console_up "$name" "$(IFS=,; echo "${cfork[*]}")"
  fi

  cp "$cfg" "$STATE_DIR/${name}.applied.yaml"
  echo; info "fork '$name' up. down: stack wfork down --config $cfg"
}

# build clode-stack/<svc>:<name> from the branch worktree, reusing the up build path
_branch_build() {
  local svc=$1 branch=$2 image=$3
  local base="../$svc"; [[ "$svc" == ec2mock ]] && base="../ec2-docker-mock"
  local dir; dir="$(git -C "$STACK_DIR/$base" worktree list --porcelain 2>/dev/null \
    | awk -v b="refs/heads/$branch" '/^worktree /{p=$2} /^branch /{if($2==b)print p}' | head -1)"
  [[ -z "$dir" ]] && { [[ -d "$STACK_DIR/$base/$branch" ]] && dir="$STACK_DIR/$base/$branch"; }
  [[ -n "$dir" && -f "$dir/Dockerfile" ]] || die "$svc: no worktree/Dockerfile for branch '$branch' under $base"
  local var; var="$(echo "$svc" | tr 'a-z-' 'A-Z_')_DIR"
  local overlay; overlay="$(mktemp --suffix=.yml)"
  printf 'services:\n  %s:\n    image: %s\n' "$svc" "$image" > "$overlay"
  mapfile -t _cf < <(_compose_files)
  ( cd "$STACK_DIR" && export "$var=$dir" && scripts/gen-build-cache.sh >/dev/null 2>&1 || true
    DOCKER_BUILDKIT=1 docker compose --project-directory "$STACK_DIR" "${_cf[@]}" -f "$overlay" build "$svc" )
  rm -f "$overlay"
}

# lift env for one service (stdout = env-file), with peer-rewrite + db + overrides
_svc_env() {
  local svc=$1 name=$2 forked_csv=$3 db=$4 cfgjson=$5 extra=$6
  local cfgfile; cfgfile="$(mktemp)"; printf '%s' "$cfgjson" > "$cfgfile"
  SVC="$svc" NAME="$name" FORKED="$forked_csv" DB="$db" EXTRA="$extra" CFG="$cfgfile" \
  python3 - <<'PY'
import sys, json, os, re
cfg = json.load(open(os.environ["CFG"]))["services"][os.environ["SVC"]]
name = os.environ["NAME"]
# include SELF: the container is <svc>-<name>, so its own bind addr / self-URL
# (e.g. CLUSTER_GRPC_ADDR=brahmi:9500) must point at <svc>-<name> too.
forked = [x for x in os.environ["FORKED"].split(",") if x]
env = dict((cfg.get("environment") or {}))
# rewrite the HOST token of every forked peer: <peer> -> <peer>-<name>. Require a
# trailing :port or /path so bare non-host values (DB_NAME=brahmi) are untouched,
# and a prefix boundary so brahmi-internal is not matched by brahmi.
for p in forked:
    pat = re.compile(rf'(^|[/@]){re.escape(p)}(?=[:/])')
    for k, v in list(env.items()):
        if v is None: continue
        env[k] = pat.sub(rf'\g<1>{p}-{name}', str(v))
# db: fresh -> point at <base>_<name>
if os.environ["DB"] == "fresh":
    for k in ("DB_NAME",):
        if k in env and env[k]:
            env[k] = f"{env[k]}_{name}"
# explicit overrides win
for k, v in (json.loads(os.environ["EXTRA"]) or {}).items():
    env[k] = v
for k, v in env.items():
    if v is None: continue
    v = str(v)
    if "\n" in v: continue
    print(f"{k}={v}")
PY
  rm -f "$cfgfile"
  # fresh DB: create + copy baseline schema
  if [[ "$db" == "fresh" ]]; then
    local base new dbc; base="$(printf '%s' "$cfgjson" | python3 -c "import sys,json;print((json.load(sys.stdin)['services']['$svc'].get('environment') or {}).get('DB_NAME',''))")"
    if [[ -n "$base" ]]; then
      new="${base}_${name}"; dbc="$(_db_container)"
      docker exec "$dbc" psql -U postgres -tc "SELECT 1 FROM pg_database WHERE datname='$new'" 2>/dev/null | grep -q 1 \
        || docker exec "$dbc" psql -U postgres -c "CREATE DATABASE \"$new\" TEMPLATE template0" >/dev/null 2>&1
      docker exec "$dbc" pg_dump -s -U postgres "$base" 2>/dev/null | docker exec -i "$dbc" psql -U postgres -d "$new" >/dev/null 2>&1 || true
      info "  db: fresh → $new (schema copied; migrations may still be needed)" >&2
    fi
  fi
}

_svc_port() {
  printf '%s' "$2" | python3 -c "
import sys,json
c=json.load(sys.stdin)['services']['$1']; l=c.get('labels') or {}
if isinstance(l,list): l=dict(x.partition('=')[::2] for x in l)
print(next((v for k,v in l.items() if k.endswith('loadbalancer.server.port')),'8080'))"
}

_run() {
  local cname=$1 svc=$2 name=$3 image=$4 port=$5 envfile=$6 cfgjson=$7
  local mem cpus entry
  mem="$(printf '%s' "$cfgjson" | python3 -c "import sys,json;print(json.load(sys.stdin)['services']['$svc'].get('mem_limit') or '')")"
  cpus="$(printf '%s' "$cfgjson" | python3 -c "import sys,json;print(json.load(sys.stdin)['services']['$svc'].get('cpus') or '')")"
  # lift the compose command + entrypoint (e.g. brahmi needs `serve`) — image default alone crashes
  entry="$(printf '%s' "$cfgjson" | python3 -c "import sys,json;e=json.load(sys.stdin)['services']['$svc'].get('entrypoint');print(e if isinstance(e,str) else '')")"
  mapfile -t cmd < <(printf '%s' "$cfgjson" | python3 -c "import sys,json;c=json.load(sys.stdin)['services']['$svc'].get('command') or [];c=[c] if isinstance(c,str) else c;[print(x) for x in c]")
  docker run -d --name "$cname" --network "$NET" --restart unless-stopped \
    --label com.docker.compose.project=clode --label clode.wfork=1 \
    --label "clode.fork=$name" --label "clode.svc=$svc" \
    --label traefik.enable=true \
    --label "traefik.http.routers.${cname}.rule=Host(\`${cname}.localhost\`)" \
    --label "traefik.http.services.${cname}.loadbalancer.server.port=${port}" \
    --env-file "$envfile" ${mem:+--memory "$mem"} ${cpus:+--cpus "$cpus"} \
    ${entry:+--entrypoint "$entry"} \
    "$image" "${cmd[@]}" >/dev/null
}

_console_up() {
  local name=$1 forked=$2
  local cname="console-web-${name}" img="clode-console-web-${name}:latest" bargs=() s fsvcs
  IFS=',' read -ra fsvcs <<<"$forked"
  for s in "${fsvcs[@]}"; do
    s="${s// /}"; [[ -z "$s" || -z "${_VITE_VAR[$s]:-}" ]] && continue
    bargs+=(--build-arg "${_VITE_VAR[$s]}=http://${s}-${name}.localhost:8080${_VITE_SUFFIX[$s]:-}")
  done
  info "console: building $cname (forked → -$name)"
  DOCKER_BUILDKIT=1 docker build -f "$REPO_DIR/docker/console-web/Dockerfile" \
    --build-context "src=${CONSOLE_WEB_DIR:-$STACK_DIR/../console-web}" "${bargs[@]}" -t "$img" "$REPO_DIR/docker/console-web"
  docker run -d --name "$cname" --network "$NET" --restart unless-stopped \
    --label com.docker.compose.project=clode --label clode.wfork=1 \
    --label "clode.fork=$name" --label clode.svc=console-web \
    --label traefik.enable=true \
    --label "traefik.http.routers.${cname}.rule=Host(\`${cname}.localhost\`)" \
    --label "traefik.http.services.${cname}.loadbalancer.server.port=8080" \
    "$img" >/dev/null
  info "  → http://${cname}.localhost:8080"
}

# ────────────────────────────────────────────────────────────────── down ───
cmd_down() {
  local name
  if [[ "${1:-}" == "--config" ]]; then name="$(_q "$(_parse_config "$2")" '["name"]')"; else name="${1:-}"; fi
  [[ -n "$name" ]] || die "usage: wfork down --config <f> | <name>"
  local ids; ids="$(docker ps -aq --filter "label=clode.fork=$name")"
  [[ -n "$ids" ]] && docker rm -f $ids >/dev/null && info "removed fork '$name' containers"
  # drop fresh DBs recorded in the applied spec
  local applied="$STATE_DIR/${name}.applied.yaml"
  if [[ -f "$applied" ]]; then
    local dbc; dbc="$(_db_container)"
    while IFS= read -r svc; do
      local base; base="$(printf '%s' "$(_config_json)" | python3 -c "import sys,json;print((json.load(sys.stdin)['services'].get('$svc',{}).get('environment') or {}).get('DB_NAME',''))" 2>/dev/null || true)"
      [[ -n "$base" ]] && docker exec "$dbc" psql -U postgres -c "DROP DATABASE IF EXISTS \"${base}_${name}\"" >/dev/null 2>&1 || true
    done < <(_parse_config "$applied" | python3 -c 'import sys,json;d=json.load(sys.stdin);[print(s) for s,m in d["services"].items() if m["db"]=="fresh"]')
    rm -f "$applied"
  fi
  info "fork '$name' down"
}

cmd_ls() {
  echo "WITHIN-NETWORK FORKS"
  docker ps -a --filter label=clode.wfork=1 \
    --format 'table {{.Label "clode.fork"}}\t{{.Names}}\t{{.Status}}' 2>/dev/null || true
}

_get_cfg() {
  [[ "${1:-}" == "--config" && -n "${2:-}" ]] || die "requires --config <path>"
  [[ -f "$2" ]] || die "config not found: $2"
  echo "$2"
}

sub="${1:-}"; shift || true
case "$sub" in
  up)      cmd_up      "$@" ;;
  preview) cmd_preview "$@" ;;
  down)    cmd_down    "$@" ;;
  ls)      cmd_ls      "$@" ;;
  *)       die "unknown subcommand '$sub' (use: preview | up | down | ls — all --config driven)" ;;
esac
