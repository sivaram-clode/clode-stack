#!/usr/bin/env bash
# clode-stack/up.sh — build and start the stack (or a subset of services),
# tail logs, and run the unified seeder.
#
# Usage:
#   ./up.sh                              # full stack: build + up + seed
#   ./up.sh jumbo                        # only `jumbo` (compose pulls its depends_on too)
#   ./up.sh jumbo brahmi raksha          # multiple services
#   ./up.sh --batch 4                    # let 4 services build concurrently (default 2, max 6)
#   ./up.sh --batch 4 jumbo brahmi       # flag and subset can be combined
#   ./up.sh --profile browser,tools      # CSV — equivalent to repeated --profile flags
#   ./up.sh --profile voice --profile org
#   ./up.sh --agent                      # + build the full benji agent image (benji Dockerfile,
#                                        #   target benji) and flip brahmi to aramb-vm (via ec2mock).
#                                        #   Builds from the workspaces.yaml `benji:` override if set,
#                                        #   else ../benji.
#   ./up.sh --agent --state              # + bake <benji>/archives/benji-state.tar.gz into the agent
#                                        #   image and skip the boot-time OCI state pull entirely
#   ./up.sh --agent --state=/path/to/state.tar.gz      # same, custom tarball
#   ./up.sh --browser                    # + build the brave-head browser image
#                                        #   (agent-base-docker/brave-headed Dockerfile) that
#                                        #   pool-manager warms as the aramb-browser pool. Builds
#                                        #   from the workspaces.yaml `agent-base-docker:` override
#                                        #   if set, else ../agent-base-docker. Pair with
#                                        #   `--profile browser` to bring up ikki (IKKI_CONNECT).
#   ./up.sh --public                     # + cloudflared edge: flips outward URLs to https://*.srclode.online
#   BUILD_BATCH_SIZE=4 ./up.sh           # env var still honored (--batch wins if both set)
#
# Local vs public:
#   Default is fully local — traefik on host 8080 is the only HTTP entry
#   point (`<svc>.localhost:8080`), nothing depends on Cloudflare. --public
#   additionally starts cloudflared (compose profile `public`) and exports
#   STACK_SCHEME/STACK_DOMAIN/STACK_PORT/STACK_TUNNEL_DOMAIN so the
#   outward-facing URL values interpolate to https://*.srclode.online.
#   A capability report at the end says which inbound-from-internet paths
#   are off in local mode (provider webhooks, OAuth installs, …).
#
# When a subset is passed, the seeder is SKIPPED — it expects the full stack
# to be healthy and would either fail or no-op against an incomplete one.
# Run `./seed.sh` manually once the full stack is up.
#
# Build concurrency:
#   The script builds the 9 Go services in batches of BUILD_BATCH_SIZE
#   (default 2, max 6), not all at once. This is the canonical mode, not a
#   fallback. `docker compose up --build` / `docker compose build` without
#   args hands every service to BuildKit in one shot, which then schedules
#   all of them in parallel — 9 concurrent `go build`s pin every core for
#   60-90s and make the desktop unusable. COMPOSE_PARALLEL_LIMIT does not
#   affect this (it only throttles compose's own start/pull/recreate ops,
#   not builds). The only way to cap concurrent builds without restarting
#   dockerd with a custom buildkitd.toml is to call `docker compose build
#   <svc...>` in batches ourselves, which is what we do below. Tune via
#   `--batch <N>` or BUILD_BATCH_SIZE.
#
# Idempotent: re-running just confirms healthy + re-seeds (the seeder is
# itself idempotent — SQL ON CONFLICT, skills-registry slug-409, etc.).

set -euo pipefail
cd "$(dirname "$0")/.."

# Parse args: --batch <N> (1..6), --profile <name> (repeatable),
# --agent, --state [tarball], and positional service names. Order-independent.
BATCH_ARG=""
PROFILES=()
AGENT_BUILD=0    # 0 = don't build benji at all
BROWSER_BUILD=0  # 0 = don't build the brave-head browser image at all
STATE_TARBALL="" # unset = agent image keeps its default boot-time state pull
STATE_DEFAULTED=0 # 1 = bare --state (no path); default tarball is resolved after
                  #     workspace resolution, against the same checkout benji builds from
PUBLIC_MODE=0
SERVICES=()
while (( $# > 0 )); do
  case "$1" in
    --public)
      PUBLIC_MODE=1
      PROFILES+=("public")
      shift
      ;;
    --batch)
      [[ -n "${2:-}" ]] || { echo "error: --batch requires a value (1..6)" >&2; exit 2; }
      BATCH_ARG="$2"
      shift 2
      ;;
    --batch=*)
      BATCH_ARG="${1#--batch=}"
      shift
      ;;
    --profile)
      [[ -n "${2:-}" ]] || { echo "error: --profile requires a value" >&2; exit 2; }
      IFS=',' read -ra _pvals <<< "$2"
      PROFILES+=("${_pvals[@]}")
      shift 2
      ;;
    --profile=*)
      IFS=',' read -ra _pvals <<< "${1#--profile=}"
      PROFILES+=("${_pvals[@]}")
      shift
      ;;
    --agent)
      AGENT_BUILD=1
      # Swallow a legacy mode value (dev/vm/slim/voice) so old invocations
      # don't misparse it as a compose service name.
      case "${2:-}" in
        dev|vm|slim|voice)
          echo "warn: --agent no longer takes a mode (got '$2') — building the full benji image" >&2
          shift
          ;;
      esac
      shift
      ;;
    --agent=*)
      AGENT_BUILD=1
      echo "warn: --agent no longer takes a mode (got '${1#--agent=}') — building the full benji image" >&2
      shift
      ;;
    --browser)
      BROWSER_BUILD=1
      shift
      ;;
    --state)
      # Optional value; bare --state defers to the default tarball, resolved
      # after workspace resolution so it tracks a benji worktree override.
      if [[ -n "${2:-}" && "${2:0:1}" != "-" && -f "${2}" ]]; then
        STATE_TARBALL="$2"
        shift 2
      else
        STATE_DEFAULTED=1
        shift
      fi
      ;;
    --state=*)
      STATE_TARBALL="${1#--state=}"
      [[ -n "$STATE_TARBALL" ]] || STATE_DEFAULTED=1
      shift
      ;;
    --)
      shift
      SERVICES+=("$@")
      break
      ;;
    -*)
      echo "error: unknown flag: $1" >&2
      exit 2
      ;;
    *)
      SERVICES+=("$1")
      shift
      ;;
  esac
done

# --state gate: overlays the given tarball onto the built image as the
# baked state (/opt/benji/state.tar.gz) and sets BENJI_STATE_PULL=false so
# the entrypoint seeds from it instead of pulling the prod OCI registry's
# :latest at boot. Pure docker — no registry, no extra services; same
# pattern as ../benji/Dockerfile.local-overlay. Meaningless without an
# image build to overlay onto.
if [[ -n "$STATE_TARBALL" || "$STATE_DEFAULTED" -eq 1 ]]; then
  if [[ "$AGENT_BUILD" -eq 0 ]]; then
    echo "error: --state requires --agent (the state is baked into the locally-built agent image)" >&2
    exit 2
  fi
fi
# An explicit --state path is checked now; a bare --state default is resolved
# and checked after workspace resolution (it tracks the benji build context).
if [[ -n "$STATE_TARBALL" && ! -f "$STATE_TARBALL" ]]; then
  echo "error: state tarball not found: $STATE_TARBALL" >&2
  exit 2
fi

# --public: flip every outward-facing URL from http://<svc>.localhost:8080
# to https://<svc>.srclode.online. The compose interpolates these with
# local defaults (${STACK_SCHEME:-http} etc.), so exporting here — before
# any `docker compose` call — is the entire mode switch. STACK_PORT uses
# the `-` (set-and-empty is honored) form so exporting "" drops the :8080.
if (( PUBLIC_MODE )); then
  export STACK_SCHEME=https
  export STACK_DOMAIN=srclode.online
  export STACK_PORT=""
  export STACK_TUNNEL_DOMAIN=srclode.online
  # raksha's BACKEND_URL defaults to the local localhost/raksha passthrough;
  # in public mode it's the real https host (already a valid provider redirect
  # host + reachable email-link host), so override the local default.
  export STACK_RAKSHA_BACKEND_URL=https://raksha.srclode.online
  echo "==> public mode: outward URLs = https://*.srclode.online (cloudflared profile on)"
fi

# Honored by every `docker compose` call in this script + tail-logs.sh.
if (( ${#PROFILES[@]} > 0 )); then
  joined=$(IFS=,; echo "${PROFILES[*]}")
  export COMPOSE_PROFILES="$joined"
  echo "==> active compose profiles: $COMPOSE_PROFILES"
fi

PARTIAL=0
if (( ${#SERVICES[@]} > 0 )); then
  PARTIAL=1
  echo "==> partial bring-up: ${SERVICES[*]}"
fi

# ── workspace overrides ────────────────────────────────────────────────
# clode-stack/workspaces.yaml can point selected services' BUILD CONTEXT at
# a git worktree instead of the main sibling repo — code from a feature
# branch's checkout, config (env_file) still from the main repo. resolve
# exports <SVC>_DIR, read by the compose build.context and gen-build-cache.
# The table prints here AND again at the end so it can't scroll past unseen.
source scripts/lib/workspaces.sh
resolve_workspaces || exit 2
print_workspace_table
# Hard-fail on a configured-but-missing override so a stale worktree path
# never silently falls back to main (the "running in circles" trap).
ws_bad=0
for _svc in "${!WS_STATUS[@]}"; do
  if [[ "${WS_STATUS[$_svc]}" == "MISSING" ]]; then
    echo "error: workspace override '$_svc' → ${WS_DIR[$_svc]} has no Dockerfile (fix or remove it in workspaces.yaml)" >&2
    ws_bad=1
  fi
done
(( ws_bad )) && exit 2

# The --agent benji image builds from a workspace override too: resolve_workspaces
# exports BENJI_DIR when workspaces.yaml carries a `benji:` line (default ../benji).
# benji isn't a compose service, so this is the one place that consumes it.
BENJI_CTX="${BENJI_DIR:-../benji}"
# Resolve a bare `--state` default against that same checkout, so a benji
# worktree's own archived state is what gets baked in.
if (( STATE_DEFAULTED )); then
  STATE_TARBALL="${BENJI_CTX}/archives/benji-state.tar.gz"
  if [[ ! -f "$STATE_TARBALL" ]]; then
    echo "error: state tarball not found: $STATE_TARBALL" >&2
    exit 2
  fi
fi

echo "==> regenerating cache-mount Dockerfiles from upstream"
scripts/gen-build-cache.sh

# Batched build — see the "Build concurrency" header at the top of this file.
# --batch flag wins over BUILD_BATCH_SIZE env; default is 2 if neither is set.
BATCH_SIZE="${BATCH_ARG:-${BUILD_BATCH_SIZE:-2}}"
if (( BATCH_SIZE > 6 )); then
  echo "error: batch size max is 6 (got $BATCH_SIZE)" >&2
  exit 2
fi
COMPOSE_FILES=(-f docker-compose.yml -f docker-compose.cache.yml)

mapfile -t TARGET_SERVICES < <(
  if (( ${#SERVICES[@]} > 0 )); then
    printf '%s\n' "${SERVICES[@]}"
  else
    docker compose "${COMPOSE_FILES[@]}" config --services
  fi
)

# Filter to services that have a build: section (skip prebuilt images).
SERVICES_JSON=$(docker compose "${COMPOSE_FILES[@]}" config --format json)
BUILDABLE=()
for svc in "${TARGET_SERVICES[@]}"; do
  if jq -e --arg s "$svc" '.services[$s].build // empty' <<<"$SERVICES_JSON" >/dev/null; then
    BUILDABLE+=("$svc")
  fi
done

if (( ${#BUILDABLE[@]} > 0 )); then
  echo "==> building ${#BUILDABLE[@]} services in batches of ${BATCH_SIZE}: ${BUILDABLE[*]}"
  for ((i=0; i<${#BUILDABLE[@]}; i+=BATCH_SIZE)); do
    batch=("${BUILDABLE[@]:i:BATCH_SIZE}")
    echo "    --- batch: ${batch[*]} ---"
    DOCKER_BUILDKIT=1 COMPOSE_DOCKER_CLI_BUILD=1 \
      docker compose "${COMPOSE_FILES[@]}" build "${batch[@]}"
  done
fi

# Build the full benji agent image locally from the benji checkout
# (BENJI_CTX = the workspaces.yaml `benji:` override, else ../benji;
# Dockerfile, target benji). Only runs when --agent is passed; otherwise
# skipped entirely — brahmi stays on the pool path with no local build needed.
# Reuses the brahmi image built above instead of pulling
# ghcr.io/clode-labs/brahmi:main. The agent-base base image is pulled from
# GHCR (requires a docker login with a token that can read the
# agent-base-docker packages).
if [[ "$AGENT_BUILD" -eq 1 ]]; then
  # The clode-stack/ prefix is jumbo's local-dev allow-list (see
  # jumbo/internal/service/service_configuration_service.go's
  # skipImageValidation) — anything with this prefix skips the registry
  # HEAD check.
  BENJI_IMAGE="clode-stack/benji:latest"
  echo "==> building benji agent image: ${BENJI_IMAGE} (Dockerfile --target benji) from ${BENJI_CTX}"
  DOCKER_BUILDKIT=1 docker build \
    -f "${BENJI_CTX}/Dockerfile" --target benji \
    --build-arg BRAHMI_IMAGE=clode-brahmi:latest \
    -t "$BENJI_IMAGE" "${BENJI_CTX}"

  # --state: retag with the tarball as the baked state and the boot-time
  # OCI pull disabled (same pattern as ../benji/Dockerfile.local-overlay).
  if [[ -n "$STATE_TARBALL" ]]; then
    echo "==> overlaying local state: ${STATE_TARBALL} → ${BENJI_IMAGE} (BENJI_STATE_PULL=false)"
    STATE_CTX=$(mktemp -d)
    cp "$STATE_TARBALL" "$STATE_CTX/state.tar.gz"
    cat > "$STATE_CTX/Dockerfile" <<EOF
FROM ${BENJI_IMAGE}
COPY state.tar.gz /opt/benji/state.tar.gz
ENV BENJI_STATE_PULL=false
EOF
    DOCKER_BUILDKIT=1 docker build -t "$BENJI_IMAGE" "$STATE_CTX"
    rm -rf "$STATE_CTX"
  fi

  # Consumed by the docker-compose x-arambvm anchor
  # (AGENT_VM_IMAGE=${BENJI_IMAGE:-…}) and flips brahmi's provider to
  # the direct-EC2 path via ec2mock. Also read by seed.sh's pool-manager
  # step so the svc_configs row uses the same tag.
  export BENJI_IMAGE
  export AGENT_PROVIDER=aramb-vm
  echo "==> brahmi will provision via aramb-vm (AGENT_PROVIDER=aramb-vm, AGENT_VM_IMAGE=${BENJI_IMAGE})"
fi

# Build the brave-head browser image locally from the agent-base-docker
# checkout (BROWSER_CTX = the workspaces.yaml `agent-base-docker:` override,
# else ../agent-base-docker; the brave-headed subdir is the build context and
# the Dockerfile's own dir). Only runs when --browser is passed; otherwise
# skipped entirely — pool-manager keeps the aramb-browser row's JSON default
# image with no local build. The clode-stack/ tag matches the JSON default
# (imagePullPolicy IfNotPresent), so pool-manager's DockerDeployer uses this
# build instead of a registry pull. The louie build stage is pulled from GHCR
# (requires a docker login with a token that can read the clode-labs packages).
if [[ "$BROWSER_BUILD" -eq 1 ]]; then
  BROWSER_CTX="${AGENT_BASE_DOCKER_DIR:-../agent-base-docker}/brave-headed"
  if [[ ! -f "${BROWSER_CTX}/Dockerfile" ]]; then
    echo "error: brave-head Dockerfile not found at ${BROWSER_CTX}/Dockerfile" >&2
    exit 2
  fi
  BROWSER_IMAGE="clode-stack/brave-head:latest"
  echo "==> building brave-head browser image: ${BROWSER_IMAGE} from ${BROWSER_CTX}"
  DOCKER_BUILDKIT=1 docker build \
    -f "${BROWSER_CTX}/Dockerfile" \
    -t "$BROWSER_IMAGE" "${BROWSER_CTX}"

  # Read by seed.sh's pool-manager step so the aramb-browser svc_configs row
  # uses the same tag that was just built.
  export BROWSER_IMAGE
  echo "==> pool-manager will warm aramb-browser from ${BROWSER_IMAGE}"
fi

echo "==> docker compose up -d ${TARGET_SERVICES[*]:-(all)}"
docker compose "${COMPOSE_FILES[@]}" up -d "${SERVICES[@]}"

echo "==> starting per-service log tailers"
scripts/tail-logs.sh "${SERVICES[@]}"

if (( PARTIAL == 0 )); then
  echo "==> running seeder"
  scripts/seed.sh
else
  echo "==> skipping seeder (partial bring-up — run ./stack.sh seed manually if needed)"
fi

echo
echo "==> stack ready"
docker compose ps --format 'table {{.Service}}\t{{.Status}}'

# Re-print the override table so it lands in the final screenful, after all
# the build/up output has streamed past.
print_workspace_table

# ── mode report ────────────────────────────────────────────────────────
# Same discovery trick as seed.sh: what's actually running decides what
# gets said — profiles, subsets, and future services need no edits here.
RUNNING=$(docker compose "${COMPOSE_FILES[@]}" ps --services --status running,restarting,created 2>/dev/null || true)
has() { grep -qx "$1" <<<"$RUNNING"; }

echo
if (( PUBLIC_MODE )); then
  # Healthy wildcard: the probe host falls through traefik's catch-all to
  # louie's HTTP proxy, which 404s an unknown tunnel name. 530 = the CF
  # wildcard DNS CNAME is gone (restore command in CLAUDE.md).
  probe="probe-$RANDOM-$RANDOM.srclode.online"
  code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 "https://$probe" || echo 000)
  case "$code" in
    404)  echo "==> public edge healthy (https://$probe → 404 via tunnel)" ;;
    530)  echo "==> WARNING: CF edge returned 530 — wildcard DNS CNAME missing; see CLAUDE.md 'ingress rule ≠ DNS routing'" ;;
    *)    echo "==> WARNING: unexpected $code probing https://$probe — check cloudflared logs" ;;
  esac
else
  echo "==> local mode (no --public) — everything above runs; only inbound-from-internet paths are off:"
  has notify        && echo "    ⚠ notify: outbound email OK; Resend delivery webhooks won't arrive"
  has chil          && echo "    ⚠ chil: Slack events + blob attachments OK (socket mode); OAuth install, Telegram webhook, and kind=url artifact links for other workspace members need --public"
  has gitana        && echo "    ⚠ gitana: GitHub App install/OAuth callbacks need --public"
  has toolkit-proxy && echo "    ⚠ toolkit-proxy: Composio connect callbacks need --public"
  has mcp-server    && echo "    ℹ mcp-server: reachable by MCP clients on this host only"
  has louie         && echo "    ℹ louie: tunnel URLs (*.tunnel.localhost:8080) resolve on this host only"
  echo "    ingress: http://<svc>.localhost:8080 (traefik dashboard: http://traefik.localhost:8080)"
fi
# console-web (Vite dev server) is reachable BOTH directly on its host port
# and — since it now carries traefik labels — through the single ingress.
has console-web && echo "    ▶ console-web: http://console.localhost:8080 (via traefik) or http://localhost:3001 (direct); Vite HMR on both"
