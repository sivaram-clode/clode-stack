#!/usr/bin/env bash
# wfork.sh — WITHIN-NETWORK workspace fork.
#
# Run a service as `<svc>-<name>` on the EXISTING baseline `clode` network (no
# separate project or network), reached at http://<svc>-<name>.localhost:8080
# through the baseline traefik. Every unchanged peer it calls resolves by normal
# DNS to the BASELINE instance (same network) — i.e. it falls through to baseline
# for everything you didn't fork. This is the lean model: only the forked service
# gets a container; unchanged peers are reused, not mirrored.
#
#   wfork up   <svc> --name <n> [--image <img>]   run <svc>-<n> (default image: baseline)
#   wfork down <svc>-<n>                           remove it
#   wfork ls                                       list within-network forks
#
# Env, container port, resource caps and command are lifted from the baseline
# service definition (docker compose config), so <svc>-<n> is configured exactly
# like baseline <svc> — only its identity (name/host) and image differ.
#
# Scope today: a SINGLE forked service against an otherwise-baseline stack works
# (fall-through). A chain of forked services that call EACH OTHER needs the origin
# router (next step) — without it, forked A -> unchanged-name -> baseline B.
# It shares the baseline service's datastore/DB; per-branch logical DB is next.

set -euo pipefail
SCRIPTS_DIR="$(cd "$(dirname "$0")" && pwd)"
cd "$SCRIPTS_DIR/.."
REPO_DIR="$PWD"                          # where the compose files (+ my edits) live
STACK_DIR="${CLODE_STACK_DIR:-$PWD}"     # canonical checkout for path resolution (../<svc>, .env)
NET=clode

die()  { echo "wfork: $*" >&2; exit 2; }
info() { echo "==> $*"; }

_config_json() {
  local files=(-f "$REPO_DIR/docker-compose.yml")
  [[ -f "$REPO_DIR/docker-compose.cache.yml" ]] && files+=(-f "$REPO_DIR/docker-compose.cache.yml")
  [[ -z "${NO_LIMITS:-}" && -f "$REPO_DIR/docker-compose.limits.yml" ]] \
    && files+=(-f "$REPO_DIR/docker-compose.limits.yml")
  docker compose --project-directory "$STACK_DIR" "${files[@]}" config --format json
}

cmd_up() {
  local svc="" name="" image=""
  while (( $# )); do
    case "$1" in
      --name)    name="$2"; shift 2 ;;
      --name=*)  name="${1#--name=}"; shift ;;
      --image)   image="$2"; shift 2 ;;
      --image=*) image="${1#--image=}"; shift ;;
      -*)        die "unknown flag: $1" ;;
      *)         if [[ -z "$svc" ]]; then svc="$1"; else die "unexpected arg: $1"; fi; shift ;;
    esac
  done
  [[ -n "$svc"  ]] || die "usage: stack wfork <svc> --name <n> [--image <img>]"
  [[ -n "$name" ]] || die "--name <n> is required"
  [[ "$name" =~ ^[a-z0-9][a-z0-9-]*$ ]] || die "--name must be [a-z0-9-]"
  local cname="${svc}-${name}"
  image="${image:-clode-${svc}:latest}"
  docker image inspect "$image" >/dev/null 2>&1 || die "image not found: $image (build it, or pass --image)"
  docker ps -a --format '{{.Names}}' | grep -qx "$cname" && die "$cname already exists (wfork down $cname first)"

  # Lift port + resource caps + env + command from the baseline service def.
  # config json goes via a FILE (not stdin) — the heredoc below owns stdin.
  local envfile cfgfile; envfile="$(mktemp)"; cfgfile="$(mktemp)"
  _config_json > "$cfgfile" || { rm -f "$envfile" "$cfgfile"; die "compose config failed"; }
  local meta
  meta="$(python3 - "$svc" "$envfile" "$cfgfile" <<'PY'
import sys, json
svc, envpath, cfgpath = sys.argv[1], sys.argv[2], sys.argv[3]
d = json.load(open(cfgpath))["services"]
if svc not in d:
    sys.exit(f"unknown service: {svc}")
c = d[svc]
labels = c.get("labels") or {}
if isinstance(labels, list):
    labels = dict(x.partition("=")[::2] for x in labels)
port = "8080"
for k, v in labels.items():
    if k.endswith("loadbalancer.server.port"):
        port = str(v)
mem = c.get("mem_limit") or ""
cpus = c.get("cpus") or ""
cmd = c.get("command") or []
if isinstance(cmd, str):
    cmd = [cmd]
with open(envpath, "w") as f:
    for k, v in (c.get("environment") or {}).items():
        if v is None:
            continue
        v = str(v)
        if "\n" in v:      # env-file can't hold multiline values
            continue
        f.write(f"{k}={v}\n")
print(port); print(mem); print(cpus); print(json.dumps(cmd))
PY
)" || { rm -f "$envfile" "$cfgfile"; die "failed to read baseline config for '$svc'"; }
  rm -f "$cfgfile"

  # newline-separated (tab-IFS collapses empty fields; newline read preserves them)
  local port mem cpus cmdjson
  { IFS= read -r port; IFS= read -r mem; IFS= read -r cpus; IFS= read -r cmdjson; } <<<"$meta"
  mapfile -t cmd < <(printf '%s' "$cmdjson" | python3 -c 'import sys,json;[print(x) for x in json.load(sys.stdin)]')

  info "starting within-network fork: $cname (image=$image, host=$cname.localhost:8080)"
  docker run -d --name "$cname" --network "$NET" --restart unless-stopped \
    --label com.docker.compose.project=clode \
    --label clode.wfork=1 --label "clode.wfork.svc=$svc" \
    --label traefik.enable=true \
    --label "traefik.http.routers.${cname}.rule=Host(\`${cname}.localhost\`)" \
    --label "traefik.http.services.${cname}.loadbalancer.server.port=${port}" \
    --env-file "$envfile" \
    ${mem:+--memory "$mem"} ${cpus:+--cpus "$cpus"} \
    "$image" "${cmd[@]}" >/dev/null
  rm -f "$envfile"

  info "up: http://${cname}.localhost:8080  (peers fall through to baseline on the $NET network)"
  info "logs: docker logs -f $cname   |   remove: stack wfork-down $cname"
}

cmd_down() {
  local cname="${1:-}"
  [[ -n "$cname" ]] || die "usage: stack wfork-down <svc>-<name>"
  docker inspect -f '{{index .Config.Labels "clode.wfork"}}' "$cname" 2>/dev/null | grep -qx 1 \
    || die "$cname is not a within-network fork (refusing to touch it)"
  docker rm -f "$cname" >/dev/null && info "removed $cname"
}

cmd_ls() {
  echo "WITHIN-NETWORK FORKS (network=$NET)"
  docker ps -a --filter label=clode.wfork=1 \
    --format 'table {{.Names}}\t{{.Status}}\t{{.Image}}' 2>/dev/null || true
}

sub="${1:-}"; shift || true
case "$sub" in
  up)   cmd_up   "$@" ;;
  down) cmd_down "$@" ;;
  ls)   cmd_ls   "$@" ;;
  *)    die "unknown subcommand '$sub' (use: up | down | ls)" ;;
esac
