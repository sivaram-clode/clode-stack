#!/usr/bin/env bash
# scripts/lib/agent-sweep.sh — shared agent + volume sweep helpers.
#
# Sourced by cleanup.sh (--agents) and wipe.sh. Every "agent" container in
# this stack lives outside `docker compose`'s project graph, so `compose
# down` can't reach them and they linger on the `clode` bridge:
#
#   1. pool-manager LOCAL_MODE spawns kairo containers via the docker
#      socket. No mock labels — matched by IMAGE, and by their `kairo-`
#      NAME prefix as a fallback (a rebuild leaves them on a dangling image
#      id / an alternate tag that the image match misses).
#   2. ec2mock spawns aramb-vm containers named `i-<hex>` for RunInstances.
#      Every container carries an `aws.mock.instance-id` label; every
#      backing named volume carries `aws.mock.owned=true`. Matched by
#      LABEL — image-agnostic, so it survives an image tag change.
#
# Consumers get three functions:
#
#   agent_images         → prints deduped image list, one per line
#                          (ec2mock GET → JSON → $BENJI_IMAGE, in order).
#   sweep_agent_containers [dry]
#                        → docker rm -f (containers-by-label ∪
#                          containers-by-image on the clode network).
#   sweep_agent_volumes  [dry]
#                        → docker volume rm on every volume labeled
#                          aws.mock.owned=true (ec2mock's per-instance
#                          $BENJI_HOME volumes). Pool-manager LOCAL_MODE
#                          agents don't use named volumes so nothing to
#                          collect from that side.
#
# Every function accepts an optional `dry` arg — pass "1" to print what
# WOULD run instead of doing it. Empty / "0" runs for real.

# Guard against double-sourcing.
[[ -n "${_AGENT_SWEEP_SH_LOADED:-}" ]] && return 0
_AGENT_SWEEP_SH_LOADED=1

# Ports must match what compose publishes for ec2mock (see the
# `ports:` block in docker-compose.yml — 8100 → 8080).
: "${EC2MOCK_URL:=http://ec2mock.localhost:8080}"

# Path from the clode-stack root (callers `cd` there before sourcing).
: "${KAIRO_CFG:=data/pool-manager-svc-configs.json}"

# All agents attach to this docker network. Both filters scope through it
# to avoid torching an unrelated container that happens to share a benji
# tag.
: "${AGENT_NETWORK:=clode}"

# Container label ec2mock stamps on every instance it launches (mirrors
# containerLabelInstanceID in ec2-docker-mock/internal/mock/docker.go).
: "${EC2MOCK_INSTANCE_LABEL:=aws.mock.instance-id}"

# Container label ec2mock's /narnia group stamps on every service container it
# deploys (mirrors deploy.LabelDeployed in the ec2-docker-mock deploy package).
: "${EC2MOCK_DEPLOYED_LABEL:=aws.mock.deployed-service}"

# Volume label ec2mock stamps on every named volume it creates (mirrors
# labelValueTrue + "aws.mock.owned" in ensureVolume).
: "${EC2MOCK_VOLUME_LABEL:=aws.mock.owned=true}"

# agent_images: emit the deduped set of docker image refs that back
# every agent-class container in this stack. Precedence (first hit wins,
# but all sources are unioned because different container populations
# come from different sources):
#
#   1. ec2mock GET /_admin/config/default-image  — the live image ec2mock
#      is currently launching (source of truth when ec2mock is up).
#   2. .configs[].settings.image in pool-manager-svc-configs.json — the
#      images pool-manager LOCAL_MODE spawns; also seeds ec2mock on boot.
#   3. $BENJI_IMAGE — up.sh --agent exports this, overrides
#      the JSON's image at seed time; kept in the union so we still match
#      containers launched under it before the JSON was resyncd.
agent_images() {
  # The compound `{ … } | awk` runs under the caller's `set -o pipefail`;
  # every statement inside MUST end with a zero exit so the group returns
  # 0. `[[ ]] && …` short-circuits to 1 when the condition is false, so
  # every optional block below is written as `if ...; then ...; fi` (which
  # is 0 whether or not the branch fired).
  {
    # Live ec2mock lookup — non-fatal on connection error / non-JSON body /
    # unset value. curl exits non-zero on connect failure; --fail bails
    # early instead of piping HTML into jq.
    if command -v curl >/dev/null 2>&1; then
      curl -fsS --max-time 2 "${EC2MOCK_URL}/_admin/config/default-image" 2>/dev/null \
        | jq -r '.default_image // empty' 2>/dev/null || true
    fi
    if [[ -r "$KAIRO_CFG" ]] && command -v jq >/dev/null 2>&1; then
      jq -r '.configs[].settings.image // empty' "$KAIRO_CFG" 2>/dev/null || true
    fi
    if [[ -n "${BENJI_IMAGE:-}" ]]; then
      printf '%s\n' "$BENJI_IMAGE"
    fi
    # Legacy + current local tags up.sh has produced — catches containers
    # launched under an older tag scheme that is no longer the seeded
    # default.
    printf 'clode-stack/benji:%s\n' latest dev vm voice slim
  } | awk 'NF && !seen[$0]++'
}

# _agent_container_ids <image ...>: print container ids that are either
# labeled by ec2mock OR built from one of the passed images, AND attached
# to the clode network. The two sets are unioned via `sort -u` because
# `docker ps --filter` semantics can't OR across filter TYPES (label OR
# ancestor) in a single call.
_agent_container_ids() {
  local images=("$@")

  # Set A: containers ec2mock owns (label-based, image-agnostic).
  local a_ids
  a_ids=$(docker ps -aq \
    --filter "label=${EC2MOCK_INSTANCE_LABEL}" \
    --filter "network=${AGENT_NETWORK}" 2>/dev/null)

  # Set A2: services deployed via ec2mock's /narnia group (label-based,
  # image-agnostic). Separate `docker ps` because multiple `--filter label`
  # AND-combine; this OR-unions with set A via the final sort -u.
  local a2_ids
  a2_ids=$(docker ps -aq \
    --filter "label=${EC2MOCK_DEPLOYED_LABEL}" \
    --filter "network=${AGENT_NETWORK}" 2>/dev/null)

  # Set B: containers matching any pool-manager image (image-based).
  # Multiple `--filter ancestor=` are OR'd by docker; the network filter
  # AND-combines. Skip the call entirely if no images resolved.
  local b_ids=""
  if (( ${#images[@]} > 0 )); then
    local filter_args=(--filter "network=${AGENT_NETWORK}")
    local img
    for img in "${images[@]}"; do filter_args+=(--filter "ancestor=$img"); done
    b_ids=$(docker ps -aq "${filter_args[@]}" 2>/dev/null)
  fi

  # Set C: pool-manager LOCAL_MODE agents matched by NAME on the clode
  # network. Their image tag is not stable — a rebuild leaves the running
  # container on a bare image ID (dangling `<none>`), and some agents run
  # `sivaclode/kairo:latest`; both break the ancestor match in set B. Every
  # pool-manager agent is named `kairo-*` (pool-manager's container-name
  # prefix), which survives any retag, so match on that. Scoped to the clode
  # network so a compose service never matches (those are `clode-<svc>-1`).
  local c_ids
  c_ids=$(docker ps -aq \
    --filter "name=kairo-" \
    --filter "network=${AGENT_NETWORK}" 2>/dev/null)

  printf '%s\n%s\n%s\n%s\n' "$a_ids" "$a2_ids" "$b_ids" "$c_ids" | awk 'NF' | sort -u
}

# sweep_agent_containers [dry_run]: remove every agent container.
# Prints one indented line per removed container. Returns 0 whether or
# not it removed anything.
sweep_agent_containers() {
  local dry="${1:-0}"

  mapfile -t images < <(agent_images)

  mapfile -t ids < <(_agent_container_ids "${images[@]}")
  if (( ${#ids[@]} == 0 )); then
    return 0
  fi

  # Show what we're about to touch — the operator gets one shot to Ctrl-C
  # if this is scoped too broadly.
  docker ps -a --filter "id=$(printf '%s\n' "${ids[@]}" | paste -sd, -)" \
    --format '    {{.Names}}  ({{.Image}})' 2>/dev/null || true

  if [[ "$dry" == "1" ]]; then
    printf '  \033[2m$\033[0m docker rm -fv  # %d container(s)\n' "${#ids[@]}"
    return 0
  fi
  # -v takes each container's ANONYMOUS volumes with it (named volumes are
  # untouched — ec2mock's are swept by label in sweep_agent_volumes). This
  # keeps a rebuild from orphaning per-container scratch volumes.
  printf '%s\n' "${ids[@]}" | xargs -r docker rm -fv >/dev/null
}

# sweep_agent_volumes [dry_run]: remove every named volume ec2mock owns.
# Volume-remove is safe here because sweep_agent_containers is called
# first by every consumer — the volumes are detached by the time we get
# here. If a volume is still in use, docker volume rm returns
# non-zero; suppress it (best-effort) rather than aborting the wipe.
sweep_agent_volumes() {
  local dry="${1:-0}"

  local vols
  vols=$(docker volume ls -q --filter "label=${EC2MOCK_VOLUME_LABEL}" 2>/dev/null)
  if [[ -z "$vols" ]]; then
    return 0
  fi

  printf '%s\n' "$vols" | sed 's/^/    /'
  if [[ "$dry" == "1" ]]; then
    printf '  \033[2m$\033[0m docker volume rm  # %d volume(s)\n' "$(printf '%s\n' "$vols" | wc -l)"
    return 0
  fi
  printf '%s\n' "$vols" | xargs -r docker volume rm >/dev/null 2>&1 || true
}
