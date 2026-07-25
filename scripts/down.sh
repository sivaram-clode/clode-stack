#!/usr/bin/env bash
# clode-stack/down.sh — stop the stack. Preserves everything.
#
# What this does:
#   - COMPOSE_PROFILES=<every profile> docker compose down --remove-orphans
#     so services tied to `profiles: [...]` (deploy, inbox, …) get stopped
#     too — compose ignores them otherwise.
#   - Reap per-service log tailers started by up.sh.
#   - Prune each ./logs/service/<svc>/ dir to the newest 10 files.
#
# What this does NOT do:
#   - Touch volumes, images, buildkit cache, or agent containers.
#     For those, use `./stack.sh wipe`. That separation is deliberate —
#     `down` is a fast reversible stop; `wipe` is the destructive path
#     that reads --yes and asks for confirmation.

set -euo pipefail
cd "$(dirname "$0")/.."

# down.sh takes no flags. Anything passed is a user misunderstanding —
# most likely `--wipe` from muscle memory. Point them at the right script.
if (( $# > 0 )); then
  echo "down.sh: unexpected argument(s): $*" >&2
  echo "        for a destructive teardown use: ./stack.sh wipe" >&2
  exit 2
fi

# Include every profile so services tied to `profiles: [...]` (deploy, inbox, …)
# get stopped/removed too — `docker compose down` ignores them otherwise.
COMPOSE_PROFILES=$(docker compose config --profiles | paste -sd, -)
export COMPOSE_PROFILES

# Reap any per-service log tailers started by up.sh.
if [[ -f logs/service/.tailer-pids ]]; then
  awk '{print $1}' logs/service/.tailer-pids | xargs -r kill 2>/dev/null || true
  rm -f logs/service/.tailer-pids
  echo "==> stopped log tailers (files in ./logs/service/<svc>/ preserved)"
fi

# Prune per-service run logs to the 10 most recent files each.
if [[ -d logs/service ]]; then
  for svc_dir in logs/service/*/; do
    [[ -d "$svc_dir" ]] || continue
    mapfile -t files < <(find "$svc_dir" -maxdepth 1 -type f -name '*.log' -printf '%f\n' | sort)
    count=${#files[@]}
    if (( count > 10 )); then
      remove=$(( count - 10 ))
      echo "==> pruning ${remove} old log(s) from ${svc_dir} (kept newest 10)"
      for ((i=0; i<remove; i++)); do
        rm -f -- "${svc_dir}${files[i]}"
      done
    fi
  done
fi

echo "==> docker compose down --remove-orphans   (preserves volumes — use \`./stack.sh wipe\` to drop)"
docker compose down --remove-orphans
