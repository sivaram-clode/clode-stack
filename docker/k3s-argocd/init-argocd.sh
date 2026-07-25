#!/bin/sh
# Waits for k3s API, then installs ArgoCD if not already present:
#   - upstream stable manifests
#   - argocd-cm patched so the admin account has both `login` and `apiKey`
#     capabilities (default is `login` only, which blocks token mint)
#   - argocd-secret patched so admin's bcrypt-hashed password is a known
#     value ("clode-local") — narnia-workers logs in with this at startup
#   - argocd-server svc converted to NodePort 30443 (https) so containers
#     on the `clode` bridge reach it as `k3s:30443`
# Idempotent: skips the whole block if the argocd namespace already exists.
set -eu

# Known local-only creds. Bcrypt hash below is bcrypt("clode-local", cost=10).
ADMIN_PW='clode-local'
ADMIN_PW_BCRYPT='$2b$10$0lnilODHf8IzWMN51vGPuOtyT7lNBY5H6rscDBWHyD5D7YLSRYH/K'

KUBECTL='/bin/k3s kubectl'

log() { echo "[k3s-argocd-init] $*"; }

log 'waiting for k3s API'
i=0
until $KUBECTL get --raw=/healthz >/dev/null 2>&1; do
  i=$((i + 1))
  if [ $i -gt 120 ]; then
    log 'k3s API never became reachable'
    exit 1
  fi
  sleep 2
done
log 'k3s API up'

if $KUBECTL get ns argocd >/dev/null 2>&1; then
  log 'argocd namespace present, skipping install'
  exit 0
fi

log 'installing ArgoCD'
$KUBECTL create namespace argocd
$KUBECTL apply -n argocd \
  -f https://raw.githubusercontent.com/argoproj/argo-cd/stable/manifests/install.yaml

log 'waiting for argocd-secret to appear'
i=0
until $KUBECTL -n argocd get secret argocd-secret >/dev/null 2>&1; do
  i=$((i + 1))
  if [ $i -gt 60 ]; then
    log 'argocd-secret never appeared'
    exit 1
  fi
  sleep 2
done

log 'preseeding admin password + enabling apiKey capability'
MTIME="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
$KUBECTL -n argocd patch secret argocd-secret --type=merge -p "{
  \"stringData\": {
    \"admin.password\":      \"${ADMIN_PW_BCRYPT}\",
    \"admin.passwordMtime\": \"${MTIME}\"
  }
}"
$KUBECTL -n argocd patch cm argocd-cm --type=merge \
  -p '{"data":{"accounts.admin":"apiKey, login"}}'

log 'rolling out argocd-server to pick up patched secret/cm'
$KUBECTL -n argocd rollout restart deploy/argocd-server
$KUBECTL -n argocd rollout status  deploy/argocd-server --timeout=300s

log 'exposing argocd-server as NodePort 30443'
$KUBECTL -n argocd patch svc argocd-server --type=merge -p '{
  "spec": {
    "type": "NodePort",
    "ports": [
      {"name":"http","port":80,"targetPort":8080,"nodePort":30080,"protocol":"TCP"},
      {"name":"https","port":443,"targetPort":8080,"nodePort":30443,"protocol":"TCP"}
    ]
  }
}'

log "argocd ready: https://k3s:30443  (admin / ${ADMIN_PW})"
