#!/usr/bin/env python3
# clode-stack/wipe.sh — destroy the stack down to a fresh-clone equivalent.
#
# What this drops:
#   • every clode-stack compose container + anonymous/named volume
#   • every image referenced by a compose service (built + pulled base)
#     — SKIPPED with --keep-pulled (images stay so the next `up` needn't re-pull/rebuild)
#   • every out-of-compose agent container attached to the `clode`
#     network (pool-manager LOCAL_MODE kairos + mock-services aramb-vm `i-<hex>`s)
#   • every within-network fork (clode.wfork) container + its fork-specific
#     images (clode-stack/<svc>:<name>, clode-console-web-<name>)
#     — the fork IMAGES are kept with --keep-pulled (the containers still go)
#   • every named volume mock-services owns (label aws.mock.owned=true)
#   • generated build-cache/*.Dockerfile + docker-compose.{cache,images}.yml
#
# What this PRESERVES by default:
#   • the working tree (source files, configs)
#   • ./logs/service/<svc>/ files (down.py's pruning still applies)
#   • the BuildKit cache — so the next build is fast. Pass --prune-cache to also
#     clear it (GLOBAL: affects every project on this daemon; cache mounts are
#     anonymous and can't be filtered to just this project).
#
# Flags:
#   -y, --yes      Skip the confirmation prompt (CI or scripted teardown).
#   -n, --dry-run  Print every command that would run; touch nothing.
#   --keep-pulled  KEEP all Docker images — pulled bases (postgres, redis, minio,
#                  databend, …), built service images, and the kairo/benji agent
#                  tiers — so the next `up` skips the re-download + rebuild. Still
#                  removes containers, ALL volumes (clean data), and build
#                  artifacts; this is a fast data-clean teardown, not a full nuke.
#   --prune-cache  ALSO prune the global BuildKit cache (slow next rebuild).
#   -h, --help     This message.

import argparse
import json
import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s  # noqa: E402
import agent_sweep  # noqa: E402


def usage():
    # Mirror bash `sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'`: print from
    # line 2 through the first blank line, stripping a leading `# ` (or `#`).
    lines = Path(__file__).read_text().splitlines()
    out = []
    for line in lines[1:]:
        out.append(re.sub(r"^# ?", "", line))
        if line == "":
            break
    print("\n".join(out))


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        sys.stderr.write(f"wipe.sh: {message}\n")
        sys.exit(2)


def main():
    parser = _Parser(add_help=False)
    parser.add_argument("-y", "--yes", action="store_true")
    parser.add_argument("-n", "--dry-run", dest="dry_run", action="store_true")
    parser.add_argument("-h", "--help", dest="help", action="store_true")
    # BuildKit cache is KEPT by default (fast rebuilds; the prune is global and
    # hits every project). Opt in with --prune-cache for the rare deep clean.
    parser.add_argument("--prune-cache", dest="prune_cache", action="store_true")
    # Images are DROPPED by default (full nuke). Opt in with --keep-pulled to
    # keep every image (pulled bases + built + agent tiers) so the next `up`
    # skips the re-download/rebuild — the teardown still drops containers and
    # ALL volumes, so you get clean data without the slow image reacquire.
    parser.add_argument("--keep-pulled", dest="keep_pulled", action="store_true")
    args = parser.parse_args()

    if args.help:
        usage()
        sys.exit(0)

    yes = args.yes
    dry = args.dry_run

    # Confirmation. --yes skips; --dry-run also skips (nothing destructive).
    if not yes and not dry:
        print(
            "==> wipe will destroy:\n"
            "    • all clode-stack containers + within-network forks (clode.wfork), volumes (named + anonymous)\n"
            + ("    • images are KEPT (pulled bases + built + kairo/benji + fork) — because --keep-pulled\n"
               if args.keep_pulled
               else "    • all images for this stack (built + pulled bases + kairo agent) + fork images\n")
            + "    • every mock-services-owned volume (aws.mock.owned=true)\n"
            "    • generated build-cache/*.Dockerfile + docker-compose.{cache,images}.yml\n"
            + ("    • the GLOBAL BuildKit cache (every project on this daemon) — because --prune-cache\n"
               if args.prune_cache
               else "    • BuildKit cache is KEPT (rebuilds stay fast; pass --prune-cache to also clear it)\n")
            + "    (run `stack wipe -n` to see the exact commands first)"
        )
        ans = input("Proceed? [y/N] ")
        if not re.match(r"^[Yy]$", ans):
            print("aborted (no changes made)")
            sys.exit(0)

    # ── project name (needed for volume straggler filter) ──────────────────
    PROJECT = s.compose_config()["name"]

    # Include every profile so `compose down` sees profile-gated services.
    os.environ["COMPOSE_PROFILES"] = s.compose_profiles()

    # ── stage 1: release the `clode` network from non-compose containers ───
    # `docker compose down` can't drop the `clode` network while
    # out-of-compose containers are attached. Kill the mock-services aramb-vm and
    # pool-manager LOCAL_MODE kairo classes here first via the shared lib.
    print("==> sweeping non-compose agent containers on the `clode` network")
    agent_sweep.sweep_agent_containers("1" if dry else "0")

    # ── stage 1b: within-network forks (wfork — clode.wfork=1, outside compose) ─
    # Remove fork containers + their FORK-SPECIFIC images (branch builds
    # clode-stack/<svc>:<name> and clode-console-web-<name>) — but NOT baseline
    # mirror images (clode-<svc>:<branch|main>) or the agent tiers, which are shared.
    # Fork logical DBs (<svc>_<name>) vanish with the postgres volume in stage 2.
    fork_ids = s.containers("label=clode.wfork=1")
    if fork_ids:
        imgs = s.docker("inspect", "--format", "{{.Config.Image}}", *fork_ids,
                        capture=True, check=False).stdout.split()
        _tiers = {"latest", "dev", "vm", "voice", "slim"}
        fork_imgs = sorted({i for i in imgs
                            if i.startswith("clode-console-web-")
                            or (i.startswith("clode-stack/") and i.rsplit(":", 1)[-1] not in _tiers)})
        # --keep-pulled keeps every image (incl. these fork builds) — drop only
        # the containers so a re-`up` of the fork needn't rebuild.
        if args.keep_pulled:
            fork_imgs = []
        print(f"==> removing {len(fork_ids)} within-network fork container(s) (clode.wfork)")
        if dry:
            print(f"  \033[2m$\033[0m docker rm -f  # {len(fork_ids)} fork container(s)")
            if fork_imgs:
                print(f"  \033[2m$\033[0m docker rmi {' '.join(fork_imgs)}")
        else:
            s.docker("rm", "-f", *fork_ids, capture=True)
            if fork_imgs:
                s.docker("rmi", *fork_imgs, capture=True, check=False)

    # Backstop for anything the image/label sweep above missed (a legacy
    # kairo-pmlocal-* container from an older stack, a bare `docker run`
    # on the bridge, …). Compose-owned containers (clode-*) stay: `compose
    # down` will drop them next.
    if s.docker("network", "inspect", "clode", capture=True, check=False).returncode == 0:
        r = s.docker(
            "network", "inspect", "clode",
            "--format", '{{range .Containers}}{{.Name}}{{"\\n"}}{{end}}',
            capture=True, check=False,
        )
        netleft = [c for c in r.stdout.splitlines() if c != "" and not c.startswith("clode-")]
        if netleft:
            print("==> removing other non-compose containers on the clode network:")
            for c in netleft:
                print(f"    {c}")
            if dry:
                print(f"  \033[2m$\033[0m docker rm -f  # {len(netleft)} container(s)")
            else:
                s.docker("rm", "-f", *netleft, capture=True)

    # ── stage 2: compose down [--rmi all] -v ───────────────────────────────
    # --rmi all drops every image referenced by a service in the compose file
    # (both locally built `clode-*` and pulled bases like postgres, redis,
    # minio, databend, cloudflared, mc). --keep-pulled omits --rmi so those
    # images survive; -v still drops the volumes (clean data), so the next `up`
    # recreates containers from the kept images without re-pull/rebuild.
    rmi = [] if args.keep_pulled else ["--rmi", "all"]
    pretty = " ".join(["docker compose down", *rmi, "-v --remove-orphans"])
    print(f"==> {pretty}")
    if dry:
        print(f"  \033[2m$\033[0m {pretty}")
    else:
        s.compose("down", *rmi, "-v", "--remove-orphans")

    # ── stage 3: kairo agent image(s) — not declared as compose services ───
    # Pool-manager pulls them at runtime; `--rmi all` doesn't reach them.
    # Read every image tag from the svc_configs blob and drop each explicitly.
    # --keep-pulled keeps them too (they're the heaviest pulls of all).
    kairo_cfg = s.REPO_DIR / "data" / "pool-manager-svc-configs.json"
    if kairo_cfg.is_file() and not args.keep_pulled:
        try:
            data = json.loads(kairo_cfg.read_text())
        except Exception:
            data = {}
        imgs = []
        for c in data.get("configs", []):
            img = (c.get("settings") or {}).get("image")
            if img:
                imgs.append(img)
        kairo_images = sorted(set(imgs))
        for img in kairo_images:
            if s.docker("image", "inspect", img, capture=True, check=False).returncode == 0:
                print(f"==> removing kairo agent image: {img}")
                if dry:
                    print(f"  \033[2m$\033[0m docker rmi -f {img}")
                else:
                    s.docker("rmi", "-f", img, capture=True, check=False)

    # ── stage 4: mock-services-owned volumes ─────────────────────────────────────
    # These are named volumes carrying $BENJI_HOME for each mock instance.
    # `--rmi all -v` catches ONLY anonymous volumes bound to compose services;
    # the named ones mock-services creates outside compose stay behind.
    print(f"==> sweeping mock-services-owned volumes (label {agent_sweep.MOCK_SERVICES_VOLUME_LABEL})")
    agent_sweep.sweep_agent_volumes("1" if dry else "0")

    # ── stage 5: compose-project volume stragglers ─────────────────────────
    # Anonymous volumes from older runs whose containers are already gone but
    # still carry the compose-project label.
    r = s.docker(
        "volume", "ls", "-q",
        "--filter", f"label=com.docker.compose.project={PROJECT}",
        capture=True, check=False,
    )
    stragglers = s.lines(r)
    if stragglers:
        print(f"==> removing {PROJECT} volume stragglers:")
        for v in stragglers:
            print(f"    {v}")
        if dry:
            print(f"  \033[2m$\033[0m docker volume rm  # {len(stragglers)} volume(s)")
        else:
            s.docker("volume", "rm", *stragglers)

    # ── stage 6: generated build artifacts ─────────────────────────────────
    # gen-build-cache.sh recreates these on every `up`, but a wipe should leave
    # the working tree clean.
    if not dry:
        bc = s.REPO_DIR / "build-cache"
        if bc.is_dir():
            for f in bc.glob("*.Dockerfile"):
                f.unlink()
        (s.REPO_DIR / "docker-compose.cache.yml").unlink(missing_ok=True)
        (s.REPO_DIR / "docker-compose.images.yml").unlink(missing_ok=True)
    else:
        print("  \033[2m$\033[0m rm build-cache/*.Dockerfile docker-compose.cache.yml docker-compose.images.yml")

    # ── stage 7: BuildKit cache (global) — OPT-IN ─────────────────────────────
    # gen-build-cache injects `--mount=type=cache,target=/go/pkg/mod` and
    # `target=/root/.cache/go-build` into every service's Dockerfile, so this is
    # where the heavy reusable state lives — and it makes rebuilds fast. The
    # cache mounts are anonymous in BuildKit's storage, so a prune can't filter
    # to just this project; it clears EVERY BuildKit cache on the daemon. So it's
    # opt-in: default wipe keeps caches (you get a clean container/image view and
    # a fast rebuild). Pass --prune-cache for the rare full reclaim.
    if not args.prune_cache:
        print("==> BuildKit cache kept (fast rebuild). Pass --prune-cache to also clear it (global).")
    else:
        print("==> pruning BuildKit cache (global — affects every project on this docker daemon)")
        if dry:
            print("  \033[2m$\033[0m docker builder prune -af")
        else:
            s.docker("builder", "prune", "-af", capture=True, check=False)


if __name__ == "__main__":
    main()
