#!/bin/sh
# Fan out one `bore local` client per TUNNELS entry, all through a single
# container. TUNNELS is a space-separated list of `host:localport:remoteport`
# triples — the local (in-stack) service to forward, and the remote port to
# request on the relay. BORE_RELAY defaults to bore.pub.
#
# Requested remote ports are best-effort on the shared bore.pub relay; run a
# self-hosted `bore server` and point BORE_RELAY at it if you need the exact
# ports (the gRPC addrs are baked into E2B_* env ahead of time).
set -eu

RELAY="${BORE_RELAY:-bore.pub}"

if [ -z "${TUNNELS:-}" ]; then
  echo "bore-tunnel: TUNNELS is empty — nothing to forward" >&2
  exit 1
fi

pids=""
for spec in $TUNNELS; do
  host="${spec%%:*}"
  rest="${spec#*:}"
  localport="${rest%%:*}"
  remoteport="${rest##*:}"

  echo "bore-tunnel: ${host}:${localport} -> ${RELAY}:${remoteport}"
  bore local "$localport" --to "$RELAY" --local-host "$host" --port "$remoteport" &
  pids="$pids $!"
done

# If any tunnel dies, tear the whole container down so restart:unless-stopped
# brings every tunnel back together (a half-up set is worse than a clean retry).
# busybox sh has no `wait -n`, so poll the children instead.
term() {
  kill $pids 2>/dev/null || true
  exit 0
}
trap term TERM INT

while :; do
  for pid in $pids; do
    if ! kill -0 "$pid" 2>/dev/null; then
      echo "bore-tunnel: a tunnel (pid $pid) exited — stopping to trigger restart" >&2
      term
    fi
  done
  sleep 5
done
