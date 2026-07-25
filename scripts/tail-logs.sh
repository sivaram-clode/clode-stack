#!/usr/bin/env bash
# tail-logs.sh — start one background `docker compose logs -f` tailer per
# service, writing to logs/service/<svc>/<UTC-timestamp>.log. Called from
# up.sh after the stack is up. Each new ./up.sh creates a fresh timestamped
# file; previous logs are kept untouched on disk.
#
# Usage:
#   ./tail-logs.sh                  # tail every service in the compose file
#   ./tail-logs.sh jumbo brahmi     # tail only the listed services
#
# Tailers run as nohup'd background processes so they survive the parent
# shell exit. They exit on their own when the corresponding container is
# removed (docker compose down). down.sh also explicitly reaps any stragglers.

set -euo pipefail
cd "$(dirname "$0")/.."

LOGS_DIR="logs/service"
TS=$(date -u +%Y%m%dT%H%M%SZ)
PID_FILE="$LOGS_DIR/.tailer-pids"

mkdir -p "$LOGS_DIR"

# If a previous run left tailers around, reap them first.
if [[ -f "$PID_FILE" ]]; then
  awk '{print $1}' "$PID_FILE" | xargs -r kill 2>/dev/null || true
  rm -f "$PID_FILE"
fi

touch "$PID_FILE"

if (( $# > 0 )); then
  services="$*"
else
  services=$(docker compose -f docker-compose.yml -f docker-compose.cache.yml config --services)
fi

for svc in $services; do
  mkdir -p "$LOGS_DIR/$svc"
  out="$LOGS_DIR/$svc/$TS.log"
  # --no-log-prefix strips the leading "svc-1  | " so the file reads like a
  # normal service log. -t adds RFC3339 timestamps per line.
  nohup docker compose -f docker-compose.yml -f docker-compose.cache.yml \
        logs --no-color --no-log-prefix -t -f "$svc" \
        > "$out" 2>&1 < /dev/null &
  echo "$! $svc $out" >> "$PID_FILE"
done

count=$(wc -l < "$PID_FILE")
echo "==> tailing $count services into ./$LOGS_DIR/<svc>/$TS.log"
