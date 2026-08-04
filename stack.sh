#!/usr/bin/env bash
# stack.sh — single entrypoint for the clode-stack lifecycle.
#
# All implementation scripts live under ./scripts/; this wrapper just
# dispatches to them so the repo root stays uncluttered. Every subcommand
# forwards positional args verbatim, so flags like `cleanup --postgres jumbo`
# or `up jumbo brahmi` work the same as calling the scripts directly.
#
# Subcommands:
#   up [--public] [svc...] build + start (whole stack or a subset) + seed;
#                          --public adds the cloudflared edge (see up.sh)
#   wfork preview|up|down|prune|ls --config fork.<name>.yaml
#                          within-network feature-branch fork, driven by one YAML
#                          config: runs <svc>-<name> on the clode network at
#                          <svc>-<name>.localhost:8080, peers env-rewritten to the
#                          fork, DB reuse|fresh. `prune` tears down ALL forks.
#   graph                 print the service relation map (A -> B = A calls B)
#   resolve <svc...>|--workspace <f>   wake-closure + connecting/in-between nodes
#   check <svc...>        pre-flight: is the set dependency-closed? (names dropped nodes)
#
#   --- teardown ladder (least → most destructive) ---
#   down                  stop containers; KEEP volumes, images, caches (reversible)
#   cleanup [flags]       truncate DATA in place; keep schema/containers/images
#                          (per-source flags — see `stack.sh cleanup -h`)
#   reseed [flags]        cleanup -a -y then re-seed (fast clean-data loop)
#   wipe [-y] [--prune-cache]  remove containers + volumes + images + agents +
#                          forks. KEEPS the BuildKit cache (fast rebuild) unless
#                          --prune-cache (global). prompts y/N by default
#
#   seed                  run the idempotent post-boot seeder against a running stack
#   tail-logs [svc...]    (re-)start per-service log tailers into ./logs/service/
#   build-cache           regenerate cache-mount Dockerfiles + overlay
#   help                  this message

set -euo pipefail
cd "$(dirname "$0")"

usage() { sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

cmd="${1:-help}"
shift || true

case "$cmd" in
  up)          exec python3 scripts/up.py       "$@" ;;
  wfork)       exec python3 scripts/wfork.py "$@" ;;   # preview|up|down|ls, all --config driven
  graph)       exec scripts/lib/depgraph.py graph   "$@" ;;
  resolve)     exec scripts/lib/depgraph.py resolve "$@" ;;
  check)       exec scripts/lib/depgraph.py check   "$@" ;;
  down)        exec python3 scripts/down.py     "$@" ;;
  wipe)        exec python3 scripts/wipe.py     "$@" ;;
  cleanup|clean-up|clean) exec python3 scripts/cleanup.py "$@" ;;
  # reseed = "cleanup everything, don't ask, then seed". Additional flags
  # after `reseed` forward to cleanup.sh, so `stack.sh reseed --dry-run`
  # or `stack.sh reseed --agents --minio` (scoping the wipe subset) both
  # work — the `-a` is the DEFAULT when no source flag is passed, so the
  # explicit scoping wins as expected.
  reseed)      exec python3 scripts/cleanup.py --reseed -y "$@" ;;
  seed)        exec python3 scripts/seed.py     "$@" ;;
  tail-logs|logs) exec python3 scripts/tail-logs.py "$@" ;;
  build-cache) exec python3 scripts/gen-build-cache.py "$@" ;;
  help|-h|--help) usage ;;
  *)
    echo "unknown subcommand: $cmd" >&2
    echo >&2
    usage >&2
    exit 2
    ;;
esac
