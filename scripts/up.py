#!/usr/bin/env python3
"""up — build and start the stack (or a subset of services), tail logs, and
run the unified seeder.

Usage:
    ./up.py                              # full stack: build + up + seed
    ./up.py jumbo                        # only `jumbo` (compose pulls its depends_on too)
    ./up.py jumbo brahmi raksha          # multiple services
    ./up.py --batch 4                    # let 4 services build concurrently (default 2, max 6)
    ./up.py --batch 4 jumbo brahmi       # flag and subset can be combined
    ./up.py --profile browser,tools      # CSV — equivalent to repeated --profile flags
    ./up.py --profile voice --profile org
    ./up.py --agent                      # + build the full benji agent image (benji Dockerfile,
                                         #   target benji) and flip brahmi to aramb-vm (via mock-services).
                                         #   Builds from the workspaces.yaml `benji:` override if set,
                                         #   else ../benji.
    ./up.py --agent --state              # + bake <benji>/archives/benji-state.tar.gz into the agent
                                         #   image and skip the boot-time OCI state pull entirely
    ./up.py --agent --state=/path/to/state.tar.gz      # same, custom tarball
    ./up.py --state=build                # build state.tar.gz FRESH from ../benji-state +
                                         #   ../aramb-skills (benji-state/scripts/zip-and-push.sh
                                         #   --no-push) and bake that in. Implies --agent. Both
                                         #   source checkouts honor workspaces.yaml overrides
                                         #   (benji-state:/aramb-skills: -> BENJI_STATE_DIR/ARAMB_SKILLS_DIR).
    ./up.py --browser                    # + build the brave-head browser image
                                         #   (agent-base-docker/brave-headed Dockerfile) that
                                         #   pool-manager warms as the aramb-browser pool. Builds
                                         #   from the workspaces.yaml `agent-base-docker:` override
                                         #   if set, else ../agent-base-docker. Pair with
                                         #   `--profile browser` to bring up ikki (IKKI_CONNECT).
    ./up.py --public                     # + cloudflared edge: flips outward URLs to https://*.srclode.online
    BUILD_BATCH_SIZE=4 ./up.py           # env var still honored (--batch wins if both set)

Local vs public:
    Default is fully local — traefik on host 8080 is the only HTTP entry
    point (`<svc>.localhost:8080`), nothing depends on Cloudflare. --public
    additionally starts cloudflared (compose profile `public`) and exports
    STACK_SCHEME/STACK_DOMAIN/STACK_PORT/STACK_TUNNEL_DOMAIN so the
    outward-facing URL values interpolate to https://*.srclode.online.
    A capability report at the end says which inbound-from-internet paths
    are off in local mode (provider webhooks, OAuth installs, …).

When a subset is passed, the seeder is SKIPPED — it expects the full stack
to be healthy and would either fail or no-op against an incomplete one.
Run `./seed.sh` manually once the full stack is up.

Build concurrency:
    The script builds the 9 Go services in batches of BUILD_BATCH_SIZE
    (default 2, max 6), not all at once. This is the canonical mode, not a
    fallback. `docker compose up --build` / `docker compose build` without
    args hands every service to BuildKit in one shot, which then schedules
    all of them in parallel — 9 concurrent `go build`s pin every core for
    60-90s and make the desktop unusable. COMPOSE_PARALLEL_LIMIT does not
    affect this (it only throttles compose's own start/pull/recreate ops,
    not builds). The only way to cap concurrent builds without restarting
    dockerd with a custom buildkitd.toml is to call `docker compose build
    <svc...>` in batches ourselves, which is what we do below. Tune via
    `--batch <N>` or BUILD_BATCH_SIZE.

Idempotent: re-running just confirms healthy + re-seeds (the seeder is
itself idempotent — SQL ON CONFLICT, skills-registry slug-409, etc.).
"""
import os
import sys
import shutil
import random
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s  # noqa: E402
import workspaces  # noqa: E402


def _abs_ctx(ctx: str) -> Path:
    """Absolutize a context/artifact string against the canonical stack dir."""
    p = Path(ctx)
    return p if p.is_absolute() else (s.STACK_DIR / p)


def _err(msg: str) -> None:
    sys.stderr.write(msg + "\n")


def parse_args(argv):
    """Parse args: --batch <N> (1..6), --profile <name> (repeatable),
    --agent, --state [tarball], and positional service names. Order-independent.

    Mirrors the hand-rolled bash token loop exactly — argparse can't reproduce
    ``--agent``'s legacy-mode swallow or ``--state``'s consume-only-if-a-file
    heuristic.
    """
    batch_arg = ""
    profiles = []
    agent_build = False    # False = don't build benji at all
    browser_build = False  # False = don't build the brave-head browser image at all
    state_tarball = ""     # "" = agent image keeps its default boot-time state pull
    state_defaulted = False  # True = bare --state (no path); default tarball is resolved
                             #        after workspace resolution, against benji's checkout
    state_build = False    # True = --state=build; build state.tar.gz fresh from
                           #        benji-state + aramb-skills (implies --agent)
    public_mode = False
    services = []

    i = 0
    n = len(argv)
    while i < n:
        arg = argv[i]
        if arg == "--public":
            public_mode = True
            profiles.append("public")
            i += 1
        elif arg == "--batch":
            if i + 1 >= n or argv[i + 1] == "":
                _err("error: --batch requires a value (1..6)")
                raise SystemExit(2)
            batch_arg = argv[i + 1]
            i += 2
        elif arg.startswith("--batch="):
            batch_arg = arg[len("--batch="):]
            i += 1
        elif arg == "--profile":
            if i + 1 >= n or argv[i + 1] == "":
                _err("error: --profile requires a value")
                raise SystemExit(2)
            profiles.extend(argv[i + 1].split(","))
            i += 2
        elif arg.startswith("--profile="):
            profiles.extend(arg[len("--profile="):].split(","))
            i += 1
        elif arg == "--agent":
            agent_build = True
            # Swallow a legacy mode value (dev/vm/slim/voice) so old invocations
            # don't misparse it as a compose service name.
            if i + 1 < n and argv[i + 1] in ("dev", "vm", "slim", "voice"):
                _err(f"warn: --agent no longer takes a mode (got '{argv[i + 1]}') "
                     "— building the full benji image")
                i += 1
            i += 1
        elif arg.startswith("--agent="):
            agent_build = True
            _err(f"warn: --agent no longer takes a mode (got '{arg[len('--agent='):]}') "
                 "— building the full benji image")
            i += 1
        elif arg == "--browser":
            browser_build = True
            i += 1
        elif arg == "--state":
            # Optional value: `build` = build fresh from benji-state (below);
            # a file path = that tarball; bare = the default committed tarball,
            # resolved after workspace resolution so it tracks a benji override.
            nxt = argv[i + 1] if i + 1 < n else ""
            if nxt == "build":
                state_build = True
                i += 2
            elif nxt and nxt[0] != "-" and _abs_ctx(nxt).is_file():
                state_tarball = nxt
                i += 2
            else:
                state_defaulted = True
                i += 1
        elif arg.startswith("--state="):
            val = arg[len("--state="):]
            if val == "build":
                state_build = True
            elif not val:
                state_defaulted = True
            else:
                state_tarball = val
            i += 1
        elif arg == "--":
            services.extend(argv[i + 1:])
            break
        elif arg.startswith("-"):
            _err(f"error: unknown flag: {arg}")
            raise SystemExit(2)
        else:
            services.append(arg)
            i += 1

    return dict(
        batch_arg=batch_arg, profiles=profiles, agent_build=agent_build,
        browser_build=browser_build, state_tarball=state_tarball,
        state_defaulted=state_defaulted, state_build=state_build,
        public_mode=public_mode, services=services,
    )


def main(argv=None):
    a = parse_args(sys.argv[1:] if argv is None else argv)
    batch_arg = a["batch_arg"]
    profiles = a["profiles"]
    agent_build = a["agent_build"]
    browser_build = a["browser_build"]
    state_tarball = a["state_tarball"]
    state_defaulted = a["state_defaulted"]
    state_build = a["state_build"]
    public_mode = a["public_mode"]
    services = a["services"]

    # --state=build builds the agent image too (the state is baked into it),
    # so it implies --agent — no need to pass both.
    if state_build:
        agent_build = True

    # --state gate: overlays the given tarball onto the built image as the
    # baked state (/opt/benji/state.tar.gz) and sets BENJI_STATE_PULL=false so
    # the entrypoint seeds from it instead of pulling the prod OCI registry's
    # :latest at boot. Pure docker — no registry, no extra services; same
    # pattern as ../benji/Dockerfile.local-overlay. Meaningless without an
    # image build to overlay onto.
    if state_tarball or state_defaulted:
        if not agent_build:
            _err("error: --state requires --agent (the state is baked into the "
                 "locally-built agent image)")
            raise SystemExit(2)
    # An explicit --state path is checked now; a bare --state default is resolved
    # and checked after workspace resolution (it tracks the benji build context).
    if state_tarball and not _abs_ctx(state_tarball).is_file():
        _err(f"error: state tarball not found: {state_tarball}")
        raise SystemExit(2)

    # --public: flip every outward-facing URL from http://<svc>.localhost:8080
    # to https://<svc>.srclode.online. The compose interpolates these with
    # local defaults (${STACK_SCHEME:-http} etc.), so exporting here — before
    # any `docker compose` call — is the entire mode switch. STACK_PORT uses
    # the `-` (set-and-empty is honored) form so exporting "" drops the :8080.
    if public_mode:
        os.environ["STACK_SCHEME"] = "https"
        os.environ["STACK_DOMAIN"] = "srclode.online"
        os.environ["STACK_PORT"] = ""
        os.environ["STACK_TUNNEL_DOMAIN"] = "srclode.online"
        # raksha's BACKEND_URL defaults to the local localhost/raksha passthrough;
        # in public mode it's the real https host (already a valid provider redirect
        # host + reachable email-link host), so override the local default.
        os.environ["STACK_RAKSHA_BACKEND_URL"] = "https://raksha.srclode.online"
        s.log("public mode: outward URLs = https://*.srclode.online (cloudflared profile on)")

    # Honored by every `docker compose` call in this script + tail-logs.sh.
    if profiles:
        joined = ",".join(profiles)
        os.environ["COMPOSE_PROFILES"] = joined
        s.log(f"active compose profiles: {joined}")

    partial = False
    if services:
        partial = True
        s.log(f"partial bring-up: {' '.join(services)}")

    # ── workspace overrides ────────────────────────────────────────────────
    # clode-stack/workspaces.yaml can point selected services' BUILD CONTEXT at
    # a git worktree instead of the main sibling repo — code from a feature
    # branch's checkout, config (env_file) still from the main repo. resolve
    # exports <SVC>_DIR, read by the compose build.context and gen-build-cache.
    # The table prints here AND again at the end so it can't scroll past unseen.
    ws = workspaces.resolve_workspaces()
    workspaces.print_workspace_table()
    # Hard-fail on a configured-but-missing override so a stale worktree path
    # never silently falls back to main (the "running in circles" trap).
    ws_bad = False
    for svc, info in ws.items():
        if info["status"] == "MISSING":
            _err(f"error: workspace override '{svc}' → {info['dir']} has no Dockerfile "
                 "(fix or remove it in workspaces.yaml)")
            ws_bad = True
    if ws_bad:
        raise SystemExit(2)

    # The --agent benji image builds from a workspace override too:
    # resolve_workspaces exports BENJI_DIR when workspaces.yaml carries a
    # `benji:` line (default ../benji). benji isn't a compose service, so this
    # is the one place that consumes it.
    benji_ctx = os.environ.get("BENJI_DIR") or "../benji"
    # --state=build: assemble state.tar.gz fresh from the benji-state repo +
    # aramb-skills, instead of baking benji's committed archive. Both source
    # checkouts honor workspaces.yaml (BENJI_STATE_DIR / ARAMB_SKILLS_DIR), else
    # ../benji-state and ../aramb-skills. benji-state's own zip-and-push.sh does
    # the work (--no-push = build + validate, no OCI login/push); ARAMB_SKILLS_DIR
    # makes it bundle the local skills tree instead of cloning main.
    if state_build:
        bs_dir = _abs_ctx(os.environ.get("BENJI_STATE_DIR") or "../benji-state")
        sk_dir = _abs_ctx(os.environ.get("ARAMB_SKILLS_DIR") or "../aramb-skills")
        zip_sh = bs_dir / "scripts" / "zip-and-push.sh"
        if not zip_sh.is_file():
            _err(f"error: benji-state build script not found: {zip_sh}")
            raise SystemExit(2)
        if not sk_dir.is_dir():
            _err(f"error: aramb-skills checkout not found: {sk_dir}")
            raise SystemExit(2)
        out_dir = tempfile.mkdtemp()
        # COMMIT_SHA is required by the script (used only for the --no-push log
        # line here); use benji-state's HEAD so the label is meaningful.
        sha = s.run(["git", "-C", str(bs_dir), "rev-parse", "HEAD"],
                    capture=True, check=False).stdout.strip() or "local"
        s.log(f"building benji-state tarball from {bs_dir} (skills: {sk_dir})")
        s.run(["bash", str(zip_sh), "--no-push"],
              env={"COMMIT_SHA": sha, "ARAMB_SKILLS_DIR": str(sk_dir), "RUNNER_TEMP": out_dir})
        state_tarball = os.path.join(out_dir, "state.tar.gz")
        if not _abs_ctx(state_tarball).is_file():
            _err(f"error: benji-state build did not produce {state_tarball}")
            raise SystemExit(2)
    # Resolve a bare `--state` default against benji's checkout, so a benji
    # worktree's own archived state is what gets baked in.
    elif state_defaulted:
        state_tarball = f"{benji_ctx}/archives/benji-state.tar.gz"
        if not _abs_ctx(state_tarball).is_file():
            _err(f"error: state tarball not found: {state_tarball}")
            raise SystemExit(2)

    s.log("regenerating cache-mount Dockerfiles from upstream")
    s.run([s.REPO_DIR / "scripts" / "gen-build-cache.sh"])

    # Batched build — see the "Build concurrency" section in the module header.
    # --batch flag wins over BUILD_BATCH_SIZE env; default is 2 if neither is set.
    batch_size = int(batch_arg or os.environ.get("BUILD_BATCH_SIZE") or "2")
    if batch_size > 6:
        _err(f"error: batch size max is 6 (got {batch_size})")
        raise SystemExit(2)

    # Per-service resource ceilings (docker-compose.limits.yml). Applied by
    # default (via stacklib.compose_files); NO_LIMITS=1 skips them.
    if not os.environ.get("NO_LIMITS") and (s.REPO_DIR / "docker-compose.limits.yml").is_file():
        s.log("resource ceilings on (docker-compose.limits.yml; NO_LIMITS=1 to skip)")

    if services:
        target_services = list(services)
    else:
        target_services = s.compose("config", "--services", capture=True).stdout.split()

    # Filter to services that have a build: section (skip prebuilt images).
    cfg_services = s.compose_config().get("services", {})
    buildable = []
    for svc in target_services:
        build = cfg_services.get(svc, {}).get("build")
        if build is not None and build is not False:
            buildable.append(svc)

    if buildable:
        s.log(f"building {len(buildable)} services in batches of {batch_size}: {' '.join(buildable)}")
        build_env = {"DOCKER_BUILDKIT": "1", "COMPOSE_DOCKER_CLI_BUILD": "1"}
        for j in range(0, len(buildable), batch_size):
            batch = buildable[j:j + batch_size]
            print(f"    --- batch: {' '.join(batch)} ---")
            s.compose("build", *batch, env=build_env)

    # Build the full benji agent image locally from the benji checkout
    # (benji_ctx = the workspaces.yaml `benji:` override, else ../benji;
    # Dockerfile, target benji). Only runs when --agent is passed; otherwise
    # skipped entirely — brahmi stays on the pool path with no local build needed.
    # Reuses the brahmi image built above instead of pulling
    # ghcr.io/clode-labs/brahmi:main. The agent-base base image is pulled from
    # GHCR (requires a docker login with a token that can read the
    # agent-base-docker packages).
    if agent_build:
        # The clode-stack/ prefix is jumbo's local-dev allow-list (see
        # jumbo/internal/service/service_configuration_service.go's
        # skipImageValidation) — anything with this prefix skips the registry
        # HEAD check.
        benji_image = "clode-stack/benji:latest"
        s.log(f"building benji agent image: {benji_image} (Dockerfile --target benji) from {benji_ctx}")
        ctx = _abs_ctx(benji_ctx)
        s.docker(
            "build", "-f", ctx / "Dockerfile", "--target", "benji",
            "--build-arg", "BRAHMI_IMAGE=clode-brahmi:latest",
            "-t", benji_image, ctx,
            env={"DOCKER_BUILDKIT": "1"},
        )

        # --state: retag with the tarball as the baked state and the boot-time
        # OCI pull disabled (same pattern as ../benji/Dockerfile.local-overlay).
        if state_tarball:
            s.log(f"overlaying local state: {state_tarball} → {benji_image} (BENJI_STATE_PULL=false)")
            state_ctx = tempfile.mkdtemp()
            shutil.copy(_abs_ctx(state_tarball), os.path.join(state_ctx, "state.tar.gz"))
            with open(os.path.join(state_ctx, "Dockerfile"), "w") as f:
                f.write(
                    f"FROM {benji_image}\n"
                    "COPY state.tar.gz /opt/benji/state.tar.gz\n"
                    "ENV BENJI_STATE_PULL=false\n"
                )
            s.docker("build", "-t", benji_image, state_ctx, env={"DOCKER_BUILDKIT": "1"})
            shutil.rmtree(state_ctx)

        # Consumed by the docker-compose x-arambvm anchor
        # (AGENT_VM_IMAGE=${BENJI_IMAGE:-…}) and flips brahmi's provider to
        # the direct-EC2 path via mock-services. Also read by seed.sh's pool-manager
        # step so the svc_configs row uses the same tag.
        os.environ["BENJI_IMAGE"] = benji_image
        os.environ["AGENT_PROVIDER"] = "aramb-vm"
        s.log(f"brahmi will provision via aramb-vm (AGENT_PROVIDER=aramb-vm, AGENT_VM_IMAGE={benji_image})")

    # Build the brave-head browser image locally from the agent-base-docker
    # checkout (browser_ctx = the workspaces.yaml `agent-base-docker:` override,
    # else ../agent-base-docker; the brave-headed subdir is the build context and
    # the Dockerfile's own dir). Only runs when --browser is passed; otherwise
    # skipped entirely — pool-manager keeps the aramb-browser row's JSON default
    # image with no local build. The clode-stack/ tag matches the JSON default
    # (imagePullPolicy IfNotPresent), so pool-manager's DockerDeployer uses this
    # build instead of a registry pull. The louie build stage is pulled from GHCR
    # (requires a docker login with a token that can read the clode-labs packages).
    if browser_build:
        browser_ctx = f"{os.environ.get('AGENT_BASE_DOCKER_DIR') or '../agent-base-docker'}/brave-headed"
        ctx = _abs_ctx(browser_ctx)
        if not (ctx / "Dockerfile").is_file():
            _err(f"error: brave-head Dockerfile not found at {browser_ctx}/Dockerfile")
            raise SystemExit(2)
        browser_image = "clode-stack/brave-head:latest"
        s.log(f"building brave-head browser image: {browser_image} from {browser_ctx}")
        s.docker(
            "build", "-f", ctx / "Dockerfile", "-t", browser_image, ctx,
            env={"DOCKER_BUILDKIT": "1"},
        )

        # Read by seed.sh's pool-manager step so the aramb-browser svc_configs row
        # uses the same tag that was just built.
        os.environ["BROWSER_IMAGE"] = browser_image
        s.log(f"pool-manager will warm aramb-browser from {browser_image}")

    s.log(f"docker compose up -d {' '.join(target_services) or '(all)'}")
    s.compose("up", "-d", *services)

    s.log("starting per-service log tailers")
    s.run([s.REPO_DIR / "scripts" / "tail-logs.sh", *services])

    if not partial:
        s.log("running seeder")
        s.run([s.REPO_DIR / "scripts" / "seed.sh"])
    else:
        s.log("skipping seeder (partial bring-up — run ./stack.sh seed manually if needed)")

    print()
    s.log("stack ready")
    s.compose("ps", "--format", "table {{.Service}}\t{{.Status}}")

    # Re-print the override table so it lands in the final screenful, after all
    # the build/up output has streamed past.
    workspaces.print_workspace_table()

    # ── mode report ────────────────────────────────────────────────────────
    # Same discovery trick as seed.sh: what's actually running decides what
    # gets said — profiles, subsets, and future services need no edits here.
    running = s.compose("ps", "--services", "--status", "running,restarting,created",
                        capture=True, check=False).stdout.split()

    def has(svc):
        return svc in running

    print()
    if public_mode:
        # Healthy wildcard: the probe host falls through traefik's catch-all to
        # louie's HTTP proxy, which 404s an unknown tunnel name. 530 = the CF
        # wildcard DNS CNAME is gone (restore command in CLAUDE.md).
        probe = f"probe-{random.randint(0, 32767)}-{random.randint(0, 32767)}.srclode.online"
        code = s.run(
            ["curl", "-s", "-o", "/dev/null", "-w", "%{http_code}", "--max-time", "10",
             f"https://{probe}"],
            capture=True, check=False,
        ).stdout.strip() or "000"
        if code == "404":
            s.log(f"public edge healthy (https://{probe} → 404 via tunnel)")
        elif code == "530":
            s.log("WARNING: CF edge returned 530 — wildcard DNS CNAME missing; "
                  "see CLAUDE.md 'ingress rule ≠ DNS routing'")
        else:
            s.log(f"WARNING: unexpected {code} probing https://{probe} — check cloudflared logs")
    else:
        s.log("local mode (no --public) — everything above runs; only inbound-from-internet paths are off:")
        if has("notify"):
            print("    ⚠ notify: outbound email OK; Resend delivery webhooks won't arrive")
        if has("chil"):
            print("    ⚠ chil: Slack events + blob attachments OK (socket mode); OAuth install, "
                  "Telegram webhook, and kind=url artifact links for other workspace members need --public")
        if has("gitana"):
            print("    ⚠ gitana: GitHub App install/OAuth callbacks need --public")
        if has("toolkit-proxy"):
            print("    ⚠ toolkit-proxy: Composio connect callbacks need --public")
        if has("mcp-server"):
            print("    ℹ mcp-server: reachable by MCP clients on this host only")
        if has("louie"):
            print("    ℹ louie: tunnel URLs (*.tunnel.localhost:8080) resolve on this host only")
        print("    ingress: http://<svc>.localhost:8080 (traefik dashboard: http://traefik.localhost:8080)")
    # console-web is a static caddy build behind traefik (no dev server / HMR;
    # rebuild with `stack up console-web` to pick up code changes).
    if has("console-web"):
        print("    ▶ console-web: http://console.localhost:8080 (static build via traefik; rebuild to refresh)")


if __name__ == "__main__":
    main()
