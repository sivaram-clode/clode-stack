# Single-container k3s + ArgoCD for the clode-stack `deploy` profile.
# k3s runs foregrounded as PID 1; a small init script runs in the background,
# waits for the k3s API, and installs ArgoCD with a known admin password and
# the `apiKey` capability enabled so narnia-workers can mint a real token at
# its own startup — no shared volume, no separate init container.
FROM rancher/k3s:v1.30.5-k3s1

COPY entrypoint.sh   /entrypoint.sh
COPY init-argocd.sh  /init-argocd.sh

ENTRYPOINT ["/entrypoint.sh"]
