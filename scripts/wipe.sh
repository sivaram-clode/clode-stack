#!/usr/bin/env bash
# clode-stack/wipe.sh — destroy the stack down to a fresh-clone equivalent.
#
# What this drops:
#   • every clode-stack compose container + anonymous/named volume
#   • every image referenced by a compose service (built + pulled base)
#   • every out-of-compose agent container attached to the `clode`
#     network (pool-manager LOCAL_MODE kairos + ec2mock aramb-vm `i-<hex>`s)
#   • every named volume ec2mock owns (label aws.mock.owned=true)
#   • the BuildKit cache — GLOBAL, affects every project on this docker
#     daemon (cache mounts are anonymous; can't filter to this project)
#   • generated build-cache/*.Dockerfile + docker-compose.cache.yml
#
# What this preserves:
#   • the working tree (source files, configs)
#   • ./logs/service/<svc>/ files (down.sh's pruning still applies)
#
# Flags:
#   -y, --yes    Skip the confirmation prompt (CI or scripted teardown).
#   -n, --dry-run  Print every command that would run; touch nothing.
#   -h, --help   This message.

set -euo pipefail
cd "$(dirname "$0")/.."

# shellcheck source=lib/agent-sweep.sh
source scripts/lib/agent-sweep.sh

YES=0
DRY=0
for arg in "$@"; do
  case "$arg" in
    -y|--yes)      YES=1 ;;
    -n|--dry-run)  DRY=1 ;;
    -h|--help)     sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *)             echo "wipe.sh: unknown arg: $arg" >&2; exit 2 ;;
  esac
done

# Confirmation. --yes skips; --dry-run also skips (nothing destructive).
if (( !YES && !DRY )); then
  cat <<'WARN'
==> wipe will destroy:
    • all clode-stack containers, volumes (named + anonymous)
    • all images for this stack (built + pulled bases + kairo agent)
    • every ec2mock-owned volume (aws.mock.owned=true)
    • the BuildKit cache for the entire docker daemon (every project, not just this one)
    • generated build-cache/*.Dockerfile + docker-compose.cache.yml
WARN
  read -rp "Proceed? [y/N] " ans
  if [[ ! "$ans" =~ ^[Yy]$ ]]; then
    echo "aborted (no changes made)"
    exit 0
  fi
fi

# ── project name (needed for volume straggler filter) ──────────────────
PROJECT=$(docker compose config --format json | python3 -c 'import json,sys;print(json.load(sys.stdin)["name"])')

# Include every profile so `compose down` sees profile-gated services.
COMPOSE_PROFILES=$(docker compose config --profiles | paste -sd, -)
export COMPOSE_PROFILES

# ── stage 1: release the `clode` network from non-compose containers ───
# `docker compose down` can't drop the `clode` network while
# out-of-compose containers are attached. Kill the ec2mock aramb-vm and
# pool-manager LOCAL_MODE kairo classes here first via the shared lib.
echo "==> sweeping non-compose agent containers on the \`clode\` network"
sweep_agent_containers "$DRY"

# Backstop for anything the image/label sweep above missed (a legacy
# kairo-pmlocal-* container from an older stack, a bare `docker run`
# on the bridge, …). Compose-owned containers (clode-*) stay: `compose
# down` will drop them next.
if docker network inspect clode >/dev/null 2>&1; then
  netleft=$(docker network inspect clode --format '{{range .Containers}}{{.Name}}{{"\n"}}{{end}}' \
            | grep -v '^$' | grep -Ev '^clode-' || true)
  if [[ -n "$netleft" ]]; then
    echo "==> removing other non-compose containers on the clode network:"
    echo "$netleft" | sed 's/^/    /'
    if (( DRY )); then
      printf '  \033[2m$\033[0m docker rm -f  # %d container(s)\n' \
        "$(printf '%s\n' "$netleft" | wc -l)"
    else
      echo "$netleft" | xargs -r docker rm -f >/dev/null
    fi
  fi
fi

# ── stage 2: compose down --rmi all -v ─────────────────────────────────
# --rmi all drops every image referenced by a service in the compose file
# (both locally built `clode-*` and pulled bases like postgres, redis,
# minio, databend, cloudflared, mc).
echo "==> docker compose down --rmi all -v --remove-orphans"
if (( DRY )); then
  printf '  \033[2m$\033[0m docker compose down --rmi all -v --remove-orphans\n'
else
  docker compose down --rmi all -v --remove-orphans
fi

# ── stage 3: kairo agent image(s) — not declared as compose services ───
# Pool-manager pulls them at runtime; `--rmi all` doesn't reach them.
# Read every image tag from the svc_configs blob and drop each explicitly.
KAIRO_CFG=data/pool-manager-svc-configs.json
if [[ -r "$KAIRO_CFG" ]] && command -v jq >/dev/null 2>&1; then
  mapfile -t kairo_images < <(jq -r '.configs[].settings.image // empty' "$KAIRO_CFG" | sort -u)
  for img in "${kairo_images[@]}"; do
    if docker image inspect "$img" >/dev/null 2>&1; then
      echo "==> removing kairo agent image: ${img}"
      if (( DRY )); then
        printf '  \033[2m$\033[0m docker rmi -f %s\n' "$img"
      else
        docker rmi -f "$img" >/dev/null 2>&1 || true
      fi
    fi
  done
fi

# ── stage 4: ec2mock-owned volumes ─────────────────────────────────────
# These are named volumes carrying $BENJI_HOME for each mock instance.
# `--rmi all -v` catches ONLY anonymous volumes bound to compose services;
# the named ones ec2mock creates outside compose stay behind.
echo "==> sweeping ec2mock-owned volumes (label ${EC2MOCK_VOLUME_LABEL})"
sweep_agent_volumes "$DRY"

# ── stage 5: compose-project volume stragglers ─────────────────────────
# Anonymous volumes from older runs whose containers are already gone but
# still carry the compose-project label.
stragglers=$(docker volume ls -q --filter "label=com.docker.compose.project=${PROJECT}")
if [[ -n "$stragglers" ]]; then
  echo "==> removing ${PROJECT} volume stragglers:"
  echo "$stragglers" | sed 's/^/    /'
  if (( DRY )); then
    printf '  \033[2m$\033[0m docker volume rm  # %d volume(s)\n' \
      "$(printf '%s\n' "$stragglers" | wc -l)"
  else
    echo "$stragglers" | xargs -r docker volume rm
  fi
fi

# ── stage 6: generated build artifacts ─────────────────────────────────
# gen-build-cache.sh recreates these on every `up`, but a wipe should leave
# the working tree clean.
if (( !DRY )); then
  if [[ -d build-cache ]]; then
    find build-cache -maxdepth 1 -name '*.Dockerfile' -delete
  fi
  rm -f docker-compose.cache.yml
else
  printf '  \033[2m$\033[0m rm build-cache/*.Dockerfile docker-compose.cache.yml\n'
fi

# ── stage 7: BuildKit cache (global) ───────────────────────────────────
# gen-build-cache.sh injects `--mount=type=cache,target=/go/pkg/mod` and
# `target=/root/.cache/go-build` into every service's Dockerfile, so this
# is where the heavy reusable state lives. The cache mounts are anonymous
# in BuildKit's storage — we can't filter to just our project, so this
# clears every BuildKit cache on the daemon.
echo "==> pruning BuildKit cache (global — affects every project on this docker daemon)"
if (( DRY )); then
  printf '  \033[2m$\033[0m docker builder prune -af\n'
else
  docker builder prune -af >/dev/null 2>&1 || true
fi
