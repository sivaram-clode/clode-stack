# Static console-web for the local stack.
#
# Build context (primary) is THIS directory (clode-stack/docker/console-web),
# which holds the Dockerfile, Caddyfile, and console-web.env — all committed, so
# the build is self-contained and worktree-safe. The console-web SOURCE comes
# from the `src` named build context (the checkout — main repo or a worktree),
# wired in compose as `additional_contexts: { src: ${CONSOLE_WEB_DIR:-../console-web} }`.
#
# Why not the app's .env.local? It is gitignored (worktrees don't have it) AND
# .dockerignored (never enters the build). So clode-stack owns the VITE_* URLs in
# console-web.env; any app-local .env is stripped so this is the sole source.
#
# Extend here as needed (build args, extra assets, a different static server).

FROM oven/bun:1 AS build
WORKDIR /app
COPY --from=src . .
# stack owns the env — drop any app-local env so console-web.env is authoritative
RUN rm -f .env .env.local .env.*.local
COPY console-web.env .env.production
RUN bun install --backend=copyfile
RUN bun run build

FROM caddy:2-alpine
COPY --from=build /app/dist /srv
COPY Caddyfile /etc/caddy/Caddyfile
EXPOSE 8080
