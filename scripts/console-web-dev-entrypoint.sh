#!/bin/sh
# console-web dev-server entrypoint.
#
# Default path: install deps from npm and run Vite (hot reload, host :3001).
#
# Local-SDK override: when workspaces.yaml selects an `aramb-sdk-node:` worktree,
# up.sh exports ARAMB_SDK_NODE_DIR → compose sets LOCAL_SDK=1 and bind-mounts that
# worktree at /aramb-sdk-node. Then console-web's @aramb-ai/sdk is repointed at the
# locally-built copy, so an UNPUBLISHED SDK (e.g. the `browserSession` event) works
# without an npm release. With no such workspace selected (LOCAL_SDK empty),
# package.json is left untouched and the published version installs as normal.
set -e
cd /app

SDK_DIR=/aramb-sdk-node

if [ -n "$LOCAL_SDK" ] && [ -f "$SDK_DIR/package.json" ]; then
  echo "[console-web] local aramb-sdk-node workspace → linking $SDK_DIR"
  # Build the SDK if dist/ is absent (dist is gitignored, so a fresh worktree
  # checkout lacks it). Edit the SDK and re-run `bun run build` there to refresh.
  if [ ! -d "$SDK_DIR/dist" ]; then
    echo "[console-web] building local SDK…"
    ( cd "$SDK_DIR" && bun install --backend=copyfile && bun run build )
  fi
  # Repoint the dependency at the local build for THIS install only, then restore
  # the committed manifest so the mounted worktree stays git-clean (the committed
  # @aramb-ai/sdk spec — e.g. ^0.0.9 — is what ships in the PR).
  cp package.json /tmp/package.json.orig
  sed -i 's#"@aramb-ai/sdk"[[:space:]]*:[[:space:]]*"[^"]*"#"@aramb-ai/sdk": "file:'"$SDK_DIR"'"#' package.json
  bun install --backend=copyfile
  cp /tmp/package.json.orig package.json
  rm -f bun.lock
else
  echo "[console-web] using @aramb-ai/sdk from npm (no local SDK workspace)"
  bun install --backend=copyfile
fi

exec bun run dev --host 0.0.0.0
