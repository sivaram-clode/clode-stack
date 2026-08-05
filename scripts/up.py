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
                                         #   Builds from BENJI_DIR if set, else ../benji.
    ./up.py --agent --state              # + bake <benji>/archives/benji-state.tar.gz into the agent
                                         #   image and skip the boot-time OCI state pull entirely
    ./up.py --agent --state=/path/to/state.tar.gz      # same, custom tarball
    ./up.py --state=build                # build state.tar.gz FRESH from ../benji-state +
                                         #   ../aramb-skills (benji-state/scripts/zip-and-push.sh
                                         #   --no-push) and bake that in. Implies --agent. Sources
                                         #   override via BENJI_STATE_DIR / ARAMB_SKILLS_DIR, else
                                         #   ../benji-state / ../aramb-skills.
    ./up.py --browser                    # + build the brave-head browser image
                                         #   (agent-base-docker/brave-headed Dockerfile) that
                                         #   pool-manager warms as the aramb-browser pool. Builds
                                         #   from AGENT_BASE_DOCKER_DIR if set, else
                                         #   ../agent-base-docker. Pair with
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
import json
import shutil
import random
import tempfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s  # noqa: E402


def _abs_ctx(ctx: str) -> Path:
    """Absolutize a context/artifact string against the canonical stack dir."""
    p = Path(ctx)
    return p if p.is_absolute() else (s.STACK_DIR / p)


def _err(msg: str) -> None:
    sys.stderr.write(msg + "\n")


# minio buckets the stack needs at boot: databend's storage engine, plus the
# blob stores for brahmi attachments, ikki session contexts, intervix
# recordings, and vova audio. Created here in `up` (previously a one-shot
# `minio-setup` compose container that lingered as Exited(0)) so the list lives
# in code and can grow bucket logic — extra buckets, policies, lifecycle rules.
MINIO_BUCKETS = ["databend", "brahmi-attachments", "ikki-session-contexts",
                 "intervix-recordings", "vova-audio"]
# databend reads its bucket over an anonymous-public policy.
MINIO_PUBLIC_BUCKETS = ["databend"]


def ensure_minio_buckets():
    """Bring minio up (waiting for healthy) and create the required buckets
    BEFORE the services that need them at boot (databend et al.) start. Uses a
    throwaway `mc` container (--rm — nothing lingers). Idempotent via
    `mb --ignore-existing`; safe to re-run every `up`."""
    s.log("minio: ensuring buckets (%s)" % ", ".join(MINIO_BUCKETS))
    s.compose("up", "-d", "--wait", "minio")
    mk = "\n".join(f"mc mb --ignore-existing local/{b}" for b in MINIO_BUCKETS)
    pub = "\n".join(f"mc anonymous set public local/{b}" for b in MINIO_PUBLIC_BUCKETS)
    script = ("set -e\n"
              "mc alias set local http://minio:9000 minioadmin minioadmin\n"
              f"{mk}\n{pub}\n")
    # minio/mc's ENTRYPOINT is `mc`, so override it to sh to run the script
    # (else `sh` is parsed as an mc subcommand).
    s.docker("run", "--rm", "--network", s.NET, "--entrypoint", "sh",
             "minio/mc:latest", "-c", script)


# pool-manager svc-config image repo -> how to build it locally, versioned, from
# the workspace. Driven by data/pool-manager-svc-configs.json: every ENABLED
# config whose image repo is here is built (if missing) and its versioned tag is
# pushed into svc_configs (and, for benji, the mock's RunInstances default) via
# the seeder's <override_env>. This replaces the --agent/--browser flags — the
# JSON's `enabled` is the opt-in; the flags only FORCE a rebuild.
def _svc_tag(svc):
    """The image tag for a service from its <SVC>_TAG env (set per-fork by wfork),
    else 'main'. brahmi->BRAHMI_TAG, agent-base-docker->AGENT_BASE_DOCKER_TAG, …"""
    return os.environ.get(svc.upper().replace("-", "_") + "_TAG") or "main"


POOL_BUILDS = {
    "clode-stack/benji": {
        "ctx": lambda: os.environ.get("BENJI_DIR") or "../benji",
        "target": "benji",
        # benji is built FROM the brahmi image (its "root"); use the RESPECTIVE
        # versioned brahmi (same BRAHMI_TAG the compose build tagged), and declare
        # the dependency so it's ensured-present before benji builds.
        "depends": [("brahmi", "clode-brahmi")],
        "build_args": lambda: [f"BRAHMI_IMAGE=clode-brahmi:{_svc_tag('brahmi')}"],
        "tag": lambda: os.environ.get("BENJI_TAG") or "main",
        "override_env": "BENJI_IMAGE", "stateable": True,
    },
    "clode-stack/brave-head": {
        "ctx": lambda: f"{os.environ.get('AGENT_BASE_DOCKER_DIR') or '../agent-base-docker'}/brave-headed",
        "target": None,
        "build_args": lambda: [],
        "tag": lambda: os.environ.get("AGENT_BASE_DOCKER_TAG") or "main",
        "override_env": "BROWSER_IMAGE", "stateable": False,
    },
}


def build_pool_images(force=frozenset(), state_tarball=""):
    """Build the agent images the pool config asks for — from the JSON, not flags.

    Reads data/pool-manager-svc-configs.json; for every ENABLED config whose
    image repo is locally buildable (POOL_BUILDS), build it VERSIONED from the
    workspace (:<branch|main>, never :latest) if it's missing, then export the
    seeder's <override_env> so the svc_config row (and the mock's default image)
    use that exact local tag. A versionless config image can't resolve to a local
    versioned build on its own (docker reads bare repo as :latest), so the tag is
    injected here. `force` (repos, from --agent/--browser) rebuilds even if present.
    """
    cfg_path = s.REPO_DIR / "data" / "pool-manager-svc-configs.json"
    if not cfg_path.is_file():
        return
    configs = json.loads(cfg_path.read_text()).get("configs") or []
    want = {(c.get("settings") or {}).get("image", "").rsplit(":", 1)[0]
            for c in configs if c.get("enabled")}
    want |= set(force)
    for repo in sorted(r for r in want if r in POOL_BUILDS):
        rec = POOL_BUILDS[repo]
        image = f"{repo}:{rec['tag']()}"
        ctx = _abs_ctx(rec["ctx"]())
        df = ctx / "Dockerfile"
        if not df.is_file():
            _err(f"error: {repo}: Dockerfile not found at {df} (checkout/workspace missing?)")
            raise SystemExit(2)
        # Ensure the images this one is built FROM exist first (benji FROM the
        # respective clode-brahmi:<tag>). It's a local-only image, so a missing
        # FROM would try to pull and fail — build it via compose (same tag).
        for dep_svc, dep_repo in rec.get("depends", []):
            dep_img = f"{dep_repo}:{_svc_tag(dep_svc)}"
            if s.docker("image", "inspect", dep_img, capture=True, check=False).returncode != 0:
                s.log(f"  {repo}: dependency {dep_img} missing — building {dep_svc}")
                s.compose("build", dep_svc,
                          env={"DOCKER_BUILDKIT": "1", "COMPOSE_DOCKER_CLI_BUILD": "1"})
        present = s.docker("image", "inspect", image, capture=True, check=False).returncode == 0
        if present and repo not in force:
            s.log(f"pool image {image} present — reusing (--agent/--browser forces a rebuild)")
        else:
            args = []
            for a in rec["build_args"]():
                args += ["--build-arg", a]
            if rec["target"]:
                args += ["--target", rec["target"]]
            s.log(f"building pool image {image} from {ctx}")
            s.docker("build", "-f", df, *args, "-t", image, ctx, env={"DOCKER_BUILDKIT": "1"})
        # --state overlay (benji only): bake the tarball in + disable the OCI pull.
        if rec["stateable"] and state_tarball:
            s.log(f"overlaying local state: {state_tarball} -> {image} (BENJI_STATE_PULL=false)")
            sc = tempfile.mkdtemp()
            shutil.copy(_abs_ctx(state_tarball), os.path.join(sc, "state.tar.gz"))
            with open(os.path.join(sc, "Dockerfile"), "w") as f:
                f.write(f"FROM {image}\nCOPY state.tar.gz /opt/benji/state.tar.gz\nENV BENJI_STATE_PULL=false\n")
            s.docker("build", "-t", image, sc, env={"DOCKER_BUILDKIT": "1"})
            shutil.rmtree(sc)
        os.environ[rec["override_env"]] = image
        s.log(f"  {repo} -> {image} (svc_configs override via {rec['override_env']})")


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

    # --state (any form) bakes a state tarball into the benji image, so it forces
    # a benji build (via the force-set below) regardless of what the pool config
    # enables — no separate --agent needed.
    if state_build or state_tarball or state_defaulted:
        agent_build = True
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

    # Baseline always builds from main — each service's build context is its main
    # sibling checkout (../<svc>). Feature branches run as parallel forks (wfork),
    # which set their own <SVC>_DIR per fork; the baseline never reads a
    # workspaces file. A one-off local override is still possible with an explicit
    # env var (e.g. BENJI_DIR=/path stack up).

    # benji isn't a compose service; --agent builds it from BENJI_DIR (default
    # ../benji) — the one place that consumes it.
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

    s.log("regenerating cache-mount + image-tag overlays from upstream")
    s.run([sys.executable, s.REPO_DIR / "scripts" / "gen-build-cache.py"])

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

    # Build the agent images the pool config asks for (from the JSON's `enabled`,
    # not flags) — benji for kairo*, brave-head for aramb-browser, each versioned
    # from its workspace and built only if missing. --agent/--browser just FORCE a
    # rebuild of the corresponding image. Both bases pull from GHCR (needs a
    # docker login that can read the clode-labs / agent-base-docker packages);
    # disable a config in the JSON to opt out. Runs after the compose build so the
    # brahmi image benji builds FROM already exists.
    force = set()
    if agent_build:
        force.add("clode-stack/benji")
    if browser_build:
        force.add("clode-stack/brave-head")
    build_pool_images(force=force, state_tarball=state_tarball)
    if os.environ.get("AGENT_PROVIDER") == "aramb-vm":
        s.log("brahmi will provision via aramb-vm (AGENT_PROVIDER=aramb-vm, "
              f"AGENT_VM_IMAGE={os.environ.get('BENJI_IMAGE')})")

    # Create minio buckets before the services that need them at boot start
    # (replaces the minio-setup one-shot). Only when minio is actually in scope.
    _minio_users = {"minio", "databend", "brahmi", "intervix", "vova", "ikki"}
    if not services or _minio_users.intersection(services):
        ensure_minio_buckets()

    s.log(f"docker compose up -d {' '.join(target_services) or '(all)'}")
    s.compose("up", "-d", *services)

    s.log("starting per-service log tailers")
    s.run([sys.executable, s.REPO_DIR / "scripts" / "tail-logs.py", *services])

    if not partial:
        s.log("running seeder")
        s.run([sys.executable, s.REPO_DIR / "scripts" / "seed.py"])
    else:
        s.log("skipping seeder (partial bring-up — run ./stack.sh seed manually if needed)")

    print()
    s.log("stack ready")
    s.compose("ps", "--format", "table {{.Service}}\t{{.Status}}")

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
