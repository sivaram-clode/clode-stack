# syntax=docker/dockerfile:1
#
# Static console-web for the local stack.
#
# Build context (primary) is THIS directory (clode-stack/docker/console-web),
# which holds the Dockerfile + Caddyfile — both committed, so the build is
# self-contained and worktree-safe. The console-web SOURCE comes from the `src`
# named build context (the checkout — main repo or a worktree), wired in compose
# as `additional_contexts: { src: ${CONSOLE_WEB_DIR:-../console-web} }`.
#
# Why not the app's .env.local? It is gitignored (worktrees don't have it) AND
# .dockerignored (never enters the build). So the VITE_* service URLs are baked
# here as ARG defaults (no committed .env file); any app-local .env is stripped so
# these are the sole source. Override per build with --build-arg (e.g. for a fork
# pointing at fork backends).

FROM oven/bun:1 AS build
WORKDIR /app
COPY --from=src . .
# stack owns the env — drop any app-local env so the ARGs below are authoritative
RUN rm -f .env .env.local .env.*.local

# VITE_* service URLs (traefik host names; host-browser reachable). Defaults make
# the build self-contained; override with --build-arg for a fork's backends.
ARG VITE_GATEWAY_BASE_URL=http://aramb-gateway.localhost:8080
ARG VITE_RAKSHA_BASE_URL=http://raksha.localhost:8080
ARG VITE_BRAHMI_BASE_URL=http://brahmi.localhost:8080
ARG VITE_JUMBO_BASE_URL=http://jumbo.localhost:8080/api/v1
ARG VITE_CHACHING_BASE_URL=http://cha-ching.localhost:8080/api/v1
ARG VITE_TOOLKIT_PROXY_BASE_URL=http://toolkit-proxy.localhost:8080/api/v1
ARG VITE_SKILLS_REGISTRY_BASE_URL=http://skills-registry.localhost:8080
ARG VITE_IKKI_BASE_URL=http://ikki.localhost:8080
ARG VITE_DEV_JWT=1
RUN { \
      echo "VITE_GATEWAY_BASE_URL=$VITE_GATEWAY_BASE_URL"; \
      echo "VITE_RAKSHA_BASE_URL=$VITE_RAKSHA_BASE_URL"; \
      echo "VITE_BRAHMI_BASE_URL=$VITE_BRAHMI_BASE_URL"; \
      echo "VITE_JUMBO_BASE_URL=$VITE_JUMBO_BASE_URL"; \
      echo "VITE_CHACHING_BASE_URL=$VITE_CHACHING_BASE_URL"; \
      echo "VITE_TOOLKIT_PROXY_BASE_URL=$VITE_TOOLKIT_PROXY_BASE_URL"; \
      echo "VITE_SKILLS_REGISTRY_BASE_URL=$VITE_SKILLS_REGISTRY_BASE_URL"; \
      echo "VITE_IKKI_BASE_URL=$VITE_IKKI_BASE_URL"; \
      echo "VITE_DEV_JWT=$VITE_DEV_JWT"; \
    } > .env.production

# Cache bun's package store across builds (fewer network fetches) and retry once
# — `bun install` occasionally fails on a transient registry hiccup.
RUN --mount=type=cache,target=/root/.bun/install/cache \
    bun install --backend=copyfile || bun install --backend=copyfile
RUN bun run build

FROM caddy:2-alpine
COPY --from=build /app/dist /srv
COPY Caddyfile /etc/caddy/Caddyfile
EXPOSE 8080
