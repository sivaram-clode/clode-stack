#!/bin/sh
# Bind-mounted into the narnia-workers container as its command wrapper for
# the local-stack `deploy` profile. Waits for the in-cluster ArgoCD (from the
# `k3s` compose service) to answer, logs in as admin with the local-only
# password preseeded by init-argocd.sh, mints a long-lived apiKey token,
# exports ARGOCD_ADDRESS/API_KEY/PROJECT, then execs the worker binary.
#
# Runs inside the narnia-workers image without any Dockerfile edit — the
# base image already ships curl (Dockerfile line 31) and node (Node 22
# nodesource install), which cover HTTP + JSON parsing.
set -eu

: "${ARGOCD_HOST:=k3s:30443}"
: "${ARGOCD_ADMIN_USER:=admin}"
: "${ARGOCD_ADMIN_PW:=clode-local}"

log() { echo "[argocd-token] $*"; }

# node -e reader that extracts a single top-level key from stdin JSON.
# Prints empty string on missing key or parse failure — caller checks emptiness.
extract() {
  key="$1"
  node -e "
    let d='';
    process.stdin.on('data',c=>d+=c);
    process.stdin.on('end',()=>{
      try { const v = JSON.parse(d)['$key']; if (v) process.stdout.write(v); }
      catch(e) {}
    });
  "
}

log "waiting for argocd at https://${ARGOCD_HOST}"
i=0
until curl -sk -o /dev/null -w '%{http_code}' \
        "https://${ARGOCD_HOST}/api/v1/session" 2>/dev/null | grep -qE '^(2..|4..)$'; do
  i=$((i + 1))
  if [ "$i" -gt 150 ]; then
    log "argocd never came up at https://${ARGOCD_HOST}" >&2
    exit 1
  fi
  sleep 2
done
log 'argocd reachable'

log 'logging in as admin'
SESSION=$(
  curl -sk -X POST "https://${ARGOCD_HOST}/api/v1/session" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"${ARGOCD_ADMIN_USER}\",\"password\":\"${ARGOCD_ADMIN_PW}\"}" \
  | extract token
)
if [ -z "$SESSION" ]; then
  log 'session login failed (is admin password preseeded?)' >&2
  exit 1
fi

log 'minting long-lived apiKey token'
TOKEN=$(
  curl -sk -X POST "https://${ARGOCD_HOST}/api/v1/account/${ARGOCD_ADMIN_USER}/token" \
    -H "Authorization: Bearer ${SESSION}" \
    -H 'Content-Type: application/json' \
    -d '{"name":"narnia-workers","expiresIn":"0"}' \
  | extract token
)
if [ -z "$TOKEN" ]; then
  log 'apiKey token mint failed (is accounts.admin=apiKey,login patched?)' >&2
  exit 1
fi

export ARGOCD_ADDRESS="$ARGOCD_HOST"
export ARGOCD_API_KEY="$TOKEN"
export ARGOCD_PROJECT="${ARGOCD_PROJECT:-default}"

log "argocd wired: ARGOCD_ADDRESS=${ARGOCD_ADDRESS} ARGOCD_PROJECT=${ARGOCD_PROJECT}"
exec "$@"
