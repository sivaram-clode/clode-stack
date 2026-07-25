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
#   down                  stop; preserves volumes and everything else
#   wipe [-y|-n]          total teardown: containers + volumes + images +
#                          buildkit cache + agent containers + ec2mock
#                          volumes; prompts y/N by default
#   cleanup [flags]       truncate data sources in place without dropping
#                          volumes — see `stack.sh cleanup -h`
#   reseed [flags]        cleanup -a -y --reseed  (data reset + fresh seed
#                          in one go; forwards extra flags to cleanup.sh)
#   seed                  run the idempotent post-boot seeder against a
#                          running stack
#   tail-logs [svc...]    (re-)start per-service log tailers into ./logs/service/
#   build-cache           regenerate cache-mount Dockerfiles + overlay
#   help                  this message

set -euo pipefail
cd "$(dirname "$0")"

usage() { sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

cmd="${1:-help}"
shift || true

case "$cmd" in
  up)          exec scripts/up.sh       "$@" ;;
  down)        exec scripts/down.sh     "$@" ;;
  wipe)        exec scripts/wipe.sh     "$@" ;;
  cleanup|clean-up|clean) exec scripts/cleanup.sh "$@" ;;
  # reseed = "cleanup everything, don't ask, then seed". Additional flags
  # after `reseed` forward to cleanup.sh, so `stack.sh reseed --dry-run`
  # or `stack.sh reseed --agents --minio` (scoping the wipe subset) both
  # work — the `-a` is the DEFAULT when no source flag is passed, so the
  # explicit scoping wins as expected.
  reseed)      exec scripts/cleanup.sh --reseed -y "$@" ;;
  seed)        exec scripts/seed.sh     "$@" ;;
  tail-logs|logs) exec scripts/tail-logs.sh "$@" ;;
  build-cache) exec scripts/gen-build-cache.sh "$@" ;;
  help|-h|--help) usage ;;
  *)
    echo "unknown subcommand: $cmd" >&2
    echo >&2
    usage >&2
    exit 2
    ;;
esac
