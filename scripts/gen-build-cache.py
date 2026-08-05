#!/usr/bin/env python3
# gen-build-cache.py — pull each Go service's Dockerfile from ../<svc>/, inject
# BuildKit cache mounts on go-mod-download / go-build RUN lines, drop the
# result in ./build-cache/, and emit docker-compose.cache.yml that points each
# service's build.dockerfile at the transformed copy.
#
# Run every time before `docker compose up` (up.sh does this for you). The
# generated files are not source — they're rebuilt from upstream on every
# run, so the stack can never drift from the real Dockerfiles.
#
# LOCAL SEED INJECTION (convention-driven, no service→dir map). Any service
# with a matching seeds/<svc>-seed.sql gets one extra RUN injected before its
# `go build`: it appends the seed SQL onto the LAST *.up.sql in the service's
# migrations dir (auto-discovered — see _discover_mig_dir), so the seed rides
# the //go:embed into the binary and `<svc> migrate` plants the rows on a fresh
# database itself — before `serve`'s boot validation can crashloop (the
# fresh-stack deadlock). No new migration version is created, so upstream
# numbering is never disturbed; databases that already applied the last
# migration skip it (scripts/seed.sh remains the idempotent backstop for
# cleanup/reseed). The upstream repo's working tree is never touched — the
# append happens inside the image build. Adding a seed = drop the file.

import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s

# Build context per service: the <SVC>_DIR env var if set (wfork sets it per
# fork), else ../<svc>. The baseline always builds from main — no workspaces
# file; <SVC>_TAG likewise defaults to 'main' unless a fork sets it.

# Services whose default context is NOT ../<service-name>. The compose file
# is the source of truth for these defaults; the only current mismatch is
# mock-services, built from ./docker/mock-services (inside clode-stack).
_WS_DEFAULT_CTX = {
    "mock-services": "./docker/mock-services",
}


# service name → env var name (pool-manager → POOL_MANAGER_DIR)
def _ws_var(svc):
    return svc.upper().replace("-", "_") + "_DIR"


# service name → default build context
def _ws_base(svc):
    return _WS_DEFAULT_CTX.get(svc, f"../{svc}")


def _discover_mig_dir(ctx):
    """The migrations dir (relative to the build context) holding *.up.sql, found
    by convention so a build-time seed needs only a seeds/<svc>-seed.sql file — no
    hardcoded service→dir map. Prunes nested worktrees (.claude, .worktrees) and
    vendor trees so a checkout's own worktrees don't make the match ambiguous.
    Returns '' when there is no unique migrations dir (none, or more than one)."""
    dirs = set()
    for root, subs, files in os.walk(ctx):
        subs[:] = [d for d in subs
                   if d not in (".git", ".claude", ".worktrees", "node_modules", "vendor")]
        if os.path.basename(root) == "migrations" and any(f.endswith(".up.sql") for f in files):
            dirs.add(os.path.relpath(root, ctx))
    return dirs.pop() if len(dirs) == 1 else ""


def inject_mounts(src, dst, seed_file="", mig_dir=""):
    # Shared `id=` keys so every service reuses ONE go-mod / go-build cache.
    # Without `id`, BuildKit derives the cache key from target + Dockerfile path,
    # giving each service its own ~1.5 GB duplicate of glafa / aws-sdk / chi /
    # etc. — ~21 GB total across the stack. With the shared ids it collapses
    # to one pool (~2–3 GB). `sharing=locked` serializes writers so parallel
    # builds (--batch >1) don't race during `go mod download`.
    mounts = (
        "--mount=type=cache,target=/go/pkg/mod,id=clode-go-mod,sharing=locked "
        "--mount=type=cache,target=/root/.cache/go-build,id=clode-go-build,sharing=locked "
    )
    # Build-time seed append (see file header). Emitted immediately before the
    # FIRST `go build` RUN so the appended SQL is part of the //go:embed set.
    # Quoted heredoc delimiter keeps the SQL byte-literal; the seed file bans
    # a bare CLODE_SEED line.
    seed_block = ''
    if seed_file and mig_dir:
        seed_sql = open(seed_file).read().rstrip('\n')
        seed_block = (
            "# clode-stack: append local seed onto the last migration so `migrate`\n"
            "# itself seeds a fresh database (source: clode-stack/" + seed_file.split('/')[-2] + "/" + seed_file.split('/')[-1] + ")\n"
            'RUN cat >> "$(ls ' + mig_dir + '/*.up.sql | sort | tail -1)" <<\'CLODE_SEED\'\n'
            "\n" + seed_sql + "\nCLODE_SEED\n"
        )

    lines = open(src).read().split('\n')
    out, i, seed_done = [], 0, False
    while i < len(lines):
        line = lines[i]
        if re.match(r'^\s*RUN\b', line):
            block = [line]
            while block[-1].rstrip().endswith('\\') and i + 1 < len(lines):
                i += 1
                block.append(lines[i])
            joined = '\n'.join(block)
            is_build = re.search(r'\bgo\s+build\b', joined)
            if is_build and seed_block and not seed_done:
                out.append(seed_block)
                seed_done = True
            if re.search(r'\bgo\s+mod\s+download\b', joined) or is_build:
                block[0] = re.sub(r'^(\s*RUN\s+)', r'\1' + mounts, block[0], count=1)
            out.extend(block)
        else:
            out.append(line)
        i += 1
    if seed_block and not seed_done:
        sys.exit(f"gen-build-cache: no `go build` RUN found in {src} to anchor the seed injection")
    open(dst, 'w').write('\n'.join(out))


def gen_image_overlay():
    """Write docker-compose.images.yml pinning every buildable service's image to
    <repo>:<tag> — tag = <SVC>_TAG (set per-fork by wfork) else 'main'. Never
    :latest, so a tag always names the branch baked into the image. Covers ALL
    buildable services (not
    just the cache-mount set), so COMPOSE_PROFILES is widened to see profiled
    ones too."""
    os.environ["COMPOSE_PROFILES"] = s.compose_profiles()
    full = s.compose_config()
    project, cfg = full["name"], full["services"]
    out = ["# AUTO-GENERATED by gen-build-cache — do not edit.\n",
           "# Pins each buildable image to :<branch|workspace|main>, never :latest.\n",
           "services:\n"]
    n = 0
    for svc in sorted(cfg):
        v = cfg[svc]
        if not v.get("build"):
            continue
        img = v.get("image") or ""
        repo = img.rsplit(":", 1)[0] if img else f"{project}-{svc}"
        tag = os.environ.get(svc.upper().replace("-", "_") + "_TAG") or "main"
        out.append(f"  {svc}:\n    image: {repo}:{tag}\n")
        n += 1
    with open("docker-compose.images.yml", "w") as fh:
        fh.writelines(out)
    return n


def main():
    os.chdir(s.REPO_DIR)  # cd "$(dirname "$0")/.."

    ROOT = os.getcwd()
    OUT_DIR = "build-cache"
    OVERRIDE = "docker-compose.cache.yml"
    SERVICES = ["raksha", "jumbo", "brahmi", "pool-manager", "chil", "cha-ching",
                "toolkit-proxy", "mang-proxy", "skills-registry", "narnia", "narnia-workers"]

    # Build-time seed injection is now convention-driven: drop a
    # seeds/<svc>-seed.sql and it's appended onto that service's last migration
    # (dir auto-discovered), so `<svc> migrate` seeds a fresh DB itself — no map.

    os.makedirs(OUT_DIR, exist_ok=True)

    with open(OVERRIDE, "w") as fh:
        fh.write("# AUTO-GENERATED by gen-build-cache.sh — do not edit.\n")
        fh.write("# Regenerated every ./stack.sh up from ../<svc>/Dockerfile.\n")
        fh.write("services:\n")

    for svc in SERVICES:
        # Build context honors a workspace override (<SVC>_DIR), else ../<svc>.
        ctx_var = _ws_var(svc)
        ctx = os.environ.get(ctx_var) or _ws_base(svc)
        src = f"{ctx}/Dockerfile"
        dst = f"{OUT_DIR}/{svc}.Dockerfile"
        if not os.path.isfile(src):
            print(f"warn: {src} not found, skipping")
            continue
        seed_file = f"seeds/{svc}-seed.sql"
        if os.path.isfile(seed_file):
            mig_dir = _discover_mig_dir(ctx)
            if mig_dir:
                inject_mounts(src, dst, seed_file, mig_dir)
                print(f"    + {svc}: seed appended to {mig_dir}/ last migration ({seed_file})")
            else:
                print(f"warn: {svc}: {seed_file} present but no unique migrations dir under {ctx} — seed NOT applied")
                inject_mounts(src, dst)
        else:
            inject_mounts(src, dst)
        # dockerfile is resolved by compose RELATIVE TO the build context. With a
        # worktree override the context can sit at any depth, so compute the
        # relative hop from the context back to the generated Dockerfile rather
        # than assuming the sibling-of-clode-stack layout. For the default
        # context (../$svc) this yields the historical ../clode-stack/... path.
        dockerfile_rel = os.path.relpath(
            os.path.realpath(os.path.join(ROOT, OUT_DIR, f"{svc}.Dockerfile")),
            os.path.realpath(ctx),
        )
        with open(OVERRIDE, "a") as fh:
            fh.write(f"  {svc}:\n")
            fh.write("    build:\n")
            fh.write(f"      dockerfile: {dockerfile_rel}\n")

    s.log(f"regenerated {OUT_DIR}/ + {OVERRIDE} from upstream Dockerfiles ({len(SERVICES)} services)")

    # Pin every buildable image to a branch/workspace/main tag (never :latest).
    n = gen_image_overlay()
    s.log(f"pinned {n} image tags in docker-compose.images.yml (branch/workspace/main, never :latest)")


if __name__ == "__main__":
    main()
