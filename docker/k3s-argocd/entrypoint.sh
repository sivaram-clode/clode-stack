#!/bin/sh
# Fork the ArgoCD installer, then hand PID 1 to k3s so container lifecycle
# tracks the API server. The installer polls the API and no-ops if ArgoCD is
# already present (k3s_server volume survives restarts).
set -eu

/init-argocd.sh >/proc/1/fd/1 2>/proc/1/fd/2 &

exec /bin/k3s server \
  --tls-san=k3s \
  --write-kubeconfig=/etc/rancher/k3s/k3s.yaml \
  --write-kubeconfig-mode=666 \
  --disable=traefik \
  --disable=metrics-server \
  --disable=servicelb
