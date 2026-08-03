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
# LOCAL SEED INJECTION. Services listed in SEED_MIGRATION_DIRS with a
# matching seeds/<svc>-seed.sql get one extra RUN injected before their
# `go build`: it appends the seed SQL onto the LAST *.up.sql in the
# service's migrations dir, so the seed rides the //go:embed into the
# binary and `<svc> migrate` plants the rows on a fresh database itself —
# before `serve`'s boot validation can crashloop (the fresh-stack
# deadlock). No new migration version is created, so upstream numbering
# is never disturbed; databases that already applied the last migration
# skip it (they were seeded by scripts/seed.sh, which remains the
# idempotent backstop for cleanup/reseed). The upstream repo's working
# tree is never touched — the append happens inside the image build.

import os
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s

# Workspace overrides: point a service's build context at a git worktree
# instead of ../<svc>. resolve_workspaces exports <SVC>_DIR (the same var the
# compose build.context interpolates); we read it below to find each
# Dockerfile. Called directly here too so `./stack.sh build-cache` honors
# workspaces.yaml even when up.sh didn't export the vars first.
#
# Prefer the workspaces.py module when present (the future home of the
# resolver); otherwise fall back to the <SVC>_DIR env vars already exported
# by up.sh via scripts/lib/workspaces.sh.
try:
    import workspaces as _ws  # scripts/lib/workspaces.py
except ImportError:
    _ws = None
if _ws is not None and hasattr(_ws, "resolve_workspaces"):
    if _ws.resolve_workspaces() not in (None, 0):
        sys.exit(2)

# Services whose default context is NOT ../<service-name>. The compose file
# is the source of truth for these defaults; the only current mismatch is
# ec2mock, built from ../ec2-docker-mock.
_WS_DEFAULT_CTX = {
    "ec2mock": "../ec2-docker-mock",
}


# service name → env var name (pool-manager → POOL_MANAGER_DIR)
def _ws_var(svc):
    return svc.upper().replace("-", "_") + "_DIR"


# service name → default build context
def _ws_base(svc):
    return _WS_DEFAULT_CTX.get(svc, f"../{svc}")


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


def main():
    os.chdir(s.REPO_DIR)  # cd "$(dirname "$0")/.."

    ROOT = os.getcwd()
    OUT_DIR = "build-cache"
    OVERRIDE = "docker-compose.cache.yml"
    SERVICES = ["raksha", "jumbo", "brahmi", "pool-manager", "chil", "cha-ching",
                "toolkit-proxy", "mang-proxy", "skills-registry", "narnia", "narnia-workers"]

    # service → migrations dir (relative to the service's build context) for
    # local-seed injection. Add a line here + seeds/<svc>-seed.sql to give
    # another service a build-time seed.
    SEED_MIGRATION_DIRS = {
        "raksha": "db/migrations",
        "cha-ching": "internal/db/migrations",
    }

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
        mig_dir = SEED_MIGRATION_DIRS.get(svc, "")
        if mig_dir and os.path.isfile(seed_file):
            inject_mounts(src, dst, seed_file, mig_dir)
            print(f"    + {svc}: local seed appended to last migration ({seed_file})")
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


if __name__ == "__main__":
    main()
