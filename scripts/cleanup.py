#!/usr/bin/env python3
# clode-stack/cleanup.sh — truncate data sources without dropping volumes.
#
# Sits between `./stack.sh seed` (additive setup) and `./stack.sh wipe`
# (full teardown). Use this when you want fresh state for a test run but
# don't want to wait through a full down/up cycle (image pulls, build
# cache restore, every-service migrate).
#
# Sources are opt-in via flags. Default (no source flag) = everything.
#
#   --postgres            TRUNCATE every user table in every per-service DB.
#                          Preserves migration-tracker tables so each
#                          service still thinks its schema is up to date.
#   --redis               FLUSHDB on logical DB 0 of the shared redis —
#                          clears raksha token registry + brahmi cluster
#                          routing only.
#   --redis-mang          FLUSHDB on logical DB 1 of the shared redis —
#                          mang-proxy's encrypted platform-key rows live
#                          here, isolated from --redis. After this you
#                          must re-export provider tokens and `./seed.sh`
#                          (or `--reseed`) to restore them.
#   --databend            Reset mang-proxy's analytics store to empty. Drops
#                          BOTH sides of the databend split-brain together
#                          (S3 bucket + raft meta volume) so mang-proxy's
#                          re-migration lands on a fresh snapshot chain.
#   --minio               Empty the `brahmi-attachments` MinIO bucket. The
#                          bucket is preserved (minio-setup recreates its
#                          contents lazily). Databend's own bucket is
#                          NOT touched here — that's --databend's job so
#                          the meta volume can be dropped in the same step.
#   --agents              docker rm -f every agent container + docker
#                          volume rm every mock-services-owned named volume.
#                          Container match set:
#                            (a) label aws.mock.instance-id (mock-services's
#                                aramb-vm containers, image-agnostic)
#                            (b) ancestor ∈ {$BENJI_IMAGE (the aramb-vm agent
#                                image), .configs[].settings.image from
#                                data/pool-manager-svc-configs.json} (aramb-vm
#                                instances + pool-manager LOCAL_MODE kairos,
#                                survivors before the label sweep)
#                          Both filters scoped to network=clode.
#                          Volume match: label aws.mock.owned=true.
#   -a, --all             All of the above. Default if no source flag.
#
# Modifiers:
#   --reseed              Run ./seed.sh after cleanup. Idempotent — safe
#                          to combine with any source flag.
#   -n, --dry-run         Print every SQL / docker command without
#                          executing it.
#   -y, --yes             Skip the confirmation prompt.
#   -h, --help            This help.
#
# Positional args (optional): names of per-service DBs to limit
# --postgres to. Without positional args, the script enumerates every
# non-template DB from postgres at runtime (excluding the bootstrap
# `postgres` DB) so newly-added services are cleaned without editing
# this script. e.g.: `./cleanup.sh --postgres jumbo brahmi`.
#
# Examples:
#   ./cleanup.sh                          # truncate everything, prompt first
#   ./cleanup.sh -y                       # ... and don't prompt
#   ./cleanup.sh --postgres               # only postgres tables
#   ./cleanup.sh --postgres jumbo         # only jumbo's DB
#   ./cleanup.sh --redis-mang --reseed    # clear mang keys, then re-seed
#   ./cleanup.sh --agents --minio         # agents + brahmi attachments
#   ./cleanup.sh -n                       # dry-run preview of full cleanup

import argparse
import re
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s  # noqa: E402
import agent_sweep  # noqa: E402

# ── defaults ──────────────────────────────────────────────────────────
DRY_RUN = 0

# Known migration-tracker tables across the Go/JS ecosystems we ship.
# Preserved so a TRUNCATE doesn't fool a service into rerunning every
# migration on next boot.
KEEP_TABLES = "('schema_migrations','migrations','goose_db_version','atlas_schema_revisions','knex_migrations','knex_migrations_lock')"


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


# ── helpers ───────────────────────────────────────────────────────────
def say(msg):
    print(f"\n\033[1;36m[clean]\033[0m {msg}")


def ok(msg):
    if DRY_RUN:
        return
    print(f"  \033[32m✓\033[0m {msg}")


def skip(msg):
    print(f"  \033[33m·\033[0m {msg}")


def warn(msg):
    print(f"  \033[33m!\033[0m {msg}", file=sys.stderr)


# `run_compose` either prints the command (dry-run) or executes it.
def run_compose(display, compose_args):
    if DRY_RUN:
        print(f"  \033[2m$\033[0m {display}")
    else:
        s.compose(*compose_args, capture=True)


# Enumerate every per-service DB from postgres at runtime. Excludes
# template DBs and the bootstrap `postgres` DB. Sourced fresh on every
# run so a new service added to init-multiple-dbs.sql / seed.sh shows up
# here without a second edit. Empty result = postgres unreachable; the
# caller treats that as "nothing to clean" via the DBS guard below.
def list_all_dbs():
    r = s.compose(
        "exec", "-T", "-e", "PGPASSWORD=postgres", "db",
        "psql", "-U", "postgres", "-tAc",
        "SELECT datname FROM pg_database WHERE datistemplate = false AND datname <> 'postgres' ORDER BY datname",
        capture=True, check=False,
    )
    out = []
    for line in r.stdout.splitlines():
        line = line.replace("\r", "")
        if line != "":
            out.append(line)
    return out


class _Parser(argparse.ArgumentParser):
    def error(self, message):
        sys.stderr.write(f"{message}\n")
        usage()
        sys.exit(2)


def main():
    global DRY_RUN

    # ── arg parsing ───────────────────────────────────────────────────────
    parser = _Parser(add_help=False)
    parser.add_argument("--postgres", action="store_true")
    parser.add_argument("--redis", action="store_true")
    parser.add_argument("--redis-mang", dest="redis_mang", action="store_true")
    parser.add_argument("--databend", action="store_true")
    parser.add_argument("--minio", action="store_true")
    parser.add_argument("--agents", action="store_true")
    parser.add_argument("-a", "--all", dest="all", action="store_true")
    parser.add_argument("--reseed", action="store_true")
    parser.add_argument("-n", "--dry-run", dest="dry_run", action="store_true")
    parser.add_argument("-y", "--yes", action="store_true")
    parser.add_argument("-h", "--help", dest="help", action="store_true")
    parser.add_argument("db_filter", nargs="*")

    # Split on `--` like bash: everything after the first `--` is a literal
    # DB_FILTER positional (even if it looks like a flag); everything before
    # is parsed normally, with positionals free to interleave with flags.
    argv = sys.argv[1:]
    if "--" in argv:
        i = argv.index("--")
        head, tail = argv[:i], argv[i + 1:]
    else:
        head, tail = argv, []
    args = parser.parse_intermixed_args(head)

    if args.help:
        usage()
        sys.exit(0)

    do_postgres = args.postgres
    do_redis = args.redis
    do_redis_mang = args.redis_mang
    do_databend = args.databend
    do_minio = args.minio
    do_agents = args.agents
    reseed = args.reseed
    DRY_RUN = 1 if args.dry_run else 0
    yes = args.yes
    db_filter = list(args.db_filter) + tail

    # -a/--all
    if args.all:
        do_postgres = do_redis = do_redis_mang = do_databend = do_minio = do_agents = True

    # If no source flag was set, default to everything.
    if not (do_postgres or do_redis or do_redis_mang or do_databend or do_minio or do_agents):
        do_postgres = do_redis = do_redis_mang = do_databend = do_minio = do_agents = True

    if db_filter:
        dbs = list(db_filter)
    elif do_postgres:
        dbs = list_all_dbs()
        if not dbs:
            warn("postgres returned no databases (container down? init still running?) — skipping --postgres")
            do_postgres = False
            dbs = []
    else:
        dbs = []

    # Auto-couple: truncating pool-manager's DB strands every kairo container
    # it launched (its svc_deployments row is gone). Same for brahmi's DB
    # and the aramb-vm containers that mock-services spawned — brahmi's
    # gateway_deployments / vm_pool_slots rows tracked them and the row is
    # gone. Force the container sweep on in either case even if --agents
    # wasn't passed.
    if do_postgres and any(d == "pool-manager" or d == "brahmi" for d in dbs):
        do_agents = True

    # ── preview / confirm ─────────────────────────────────────────────────
    say("Cleanup plan")
    if do_postgres:
        print(f"  • postgres: TRUNCATE … RESTART IDENTITY CASCADE on every non-migration table in: {' '.join(dbs)}")
    if do_redis:
        print("  • redis DB 0: FLUSHDB (raksha tokens + brahmi routing)")
    if do_redis_mang:
        print("  • redis DB 1: FLUSHDB — mang-proxy platform keys will be gone")
    if do_databend:
        print("  • databend: wipe MinIO bucket + drop databend_data volume + restart databend + mang-proxy")
    if do_minio:
        print("  • minio: empty brahmi-attachments bucket (bucket kept; databend bucket handled by --databend)")
    if do_agents:
        print("  • agents: docker rm -fv agent containers (by label, image, and kairo- name) + docker volume rm mock-services-owned volumes (see --agents in help for match set)")
    if reseed:
        print("  • after: scripts/seed.py")
    if DRY_RUN:
        print("  (dry-run — nothing will be executed)")

    if not yes and not DRY_RUN:
        ans = input("Proceed? [y/N] ")
        if not re.match(r"^[Yy]", ans):
            print("aborted")
            sys.exit(0)

    # ── postgres ──────────────────────────────────────────────────────────
    if do_postgres:
        say("Postgres: truncating user tables")
        # One PL/pgSQL DO block per DB. format()+%I quoting handles the dashed
        # pool-manager DB name safely. RESTART IDENTITY rewinds sequences so
        # serial IDs don't keep climbing across cleanups.
        truncate_sql = (
            "DO $$\n"
            "DECLARE r RECORD;\n"
            "BEGIN\n"
            "  FOR r IN\n"
            "    SELECT tablename FROM pg_tables\n"
            "    WHERE schemaname = 'public'\n"
            f"      AND tablename NOT IN {KEEP_TABLES}\n"
            "  LOOP\n"
            "    EXECUTE format('TRUNCATE TABLE %I RESTART IDENTITY CASCADE', r.tablename);\n"
            "  END LOOP;\n"
            "END\n"
            "$$;\n"
        )

        for db in dbs:
            if DRY_RUN:
                print(f'  \033[2m$\033[0m psql -d {db} -c "<TRUNCATE all non-migration tables CASCADE>"')
                continue
            r = s.psql(db, args=["-c", truncate_sql], capture=True, check=False)
            if r.returncode == 0:
                ok(f"{db}: truncated")
            else:
                warn(f"{db}: TRUNCATE failed (DB missing or service hasn't migrated yet?)")

        # raksha's admin/bot identity rows are BOOT-FATAL (serve validates them
        # at startup) and their embedded ride-along seed only fires on databases
        # that haven't applied the last migration — which a truncate deliberately
        # is not (schema_migrations is in KEEP_TABLES). Without this, a plain
        # `cleanup --postgres` leaves raksha crashlooping until someone reseeds,
        # and the next `up` hangs on its health gate. Re-apply the seed directly;
        # it's the same idempotent SQL gen-build-cache.sh bakes into the image.
        if "raksha" in dbs and not DRY_RUN:
            seed_path = s.REPO_DIR / "seeds" / "raksha-seed.sql"
            try:
                rc = s.psql("raksha", sql=seed_path.read_text(), capture=True, check=False).returncode
            except OSError:
                rc = 1
            if rc == 0:
                ok("raksha: boot-critical identity seed re-applied (seeds/raksha-seed.sql)")
            else:
                warn("raksha: identity seed re-apply failed — raksha will crashloop until './stack.sh seed' runs")

        # cha-ching's migration-seeded reference data (tier quota defaults +
        # credit catalogue) sits in regular tables, so the truncate above wipes
        # it while schema_migrations survives — migrations never re-run and
        # every subsequent org intake fails its quota-seed step (org_tiers row
        # lands, org_llm/cloud_quotas never do). Re-apply the same idempotent
        # SQL gen-build-cache.sh bakes into the image.
        if "chaching" in dbs and not DRY_RUN:
            seed_path = s.REPO_DIR / "seeds" / "cha-ching-seed.sql"
            try:
                rc = s.psql("chaching", sql=seed_path.read_text(), capture=True, check=False).returncode
            except OSError:
                rc = 1
            if rc == 0:
                ok("chaching: tier-defaults + credit-catalogue seed re-applied (seeds/cha-ching-seed.sql)")
            else:
                warn("chaching: seed re-apply failed — org intakes will half-fail until './stack.sh seed' runs")

    # ── redis DB 0 (raksha + brahmi) ──────────────────────────────────────
    if do_redis:
        say("Redis DB 0: FLUSHDB")
        run_compose(
            "docker compose exec -T redis redis-cli -a clode-redis-local -n 0 FLUSHDB >/dev/null",
            ("exec", "-T", "redis", "redis-cli", "-a", "clode-redis-local", "-n", "0", "FLUSHDB"),
        )
        ok("redis DB 0 flushed")

    # ── redis DB 1 (mang-proxy) ───────────────────────────────────────────
    if do_redis_mang:
        say("Redis DB 1: FLUSHDB")
        run_compose(
            "docker compose exec -T redis redis-cli -a clode-redis-local -n 1 FLUSHDB >/dev/null",
            ("exec", "-T", "redis", "redis-cli", "-a", "clode-redis-local", "-n", "1", "FLUSHDB"),
        )
        ok("redis DB 1 flushed — re-export provider tokens + ./stack.sh seed (or pass --reseed)")

    # ── agent containers + volumes (delegated to shared lib) ──────────────
    # Every "agent" container in this stack lives outside compose:
    #   - pool-manager LOCAL_MODE spawns kairo containers via the docker
    #     socket and tracks them in pool-manager.svc_deployments.
    #   - mock-services spawns aramb-vm containers (name = i-<hex>) and tracks
    #     them in brahmi.gateway_deployments / vm_pool_slots + labels every
    #     container with aws.mock.instance-id and every volume with
    #     aws.mock.owned=true.
    # Both classes disappear from tracking on a --postgres cleanup and become
    # orphans. Sweep containers by (label ∪ image ∩ network=clode) and volumes
    # by aws.mock.owned label. Both filters live in scripts/lib/agent_sweep.py
    # so wipe.py sees the same source of truth.
    if do_agents:
        say("agents: containers on the `clode` network + mock-services-owned volumes")
        imgs = agent_sweep.agent_images()
        if imgs:
            print(f"  images: {' '.join(imgs)}")
        agent_sweep.sweep_agent_containers("1" if DRY_RUN else "0")
        agent_sweep.sweep_agent_volumes("1" if DRY_RUN else "0")
        ok("agent sweep complete")

    # ── databend ──────────────────────────────────────────────────────────
    # Databend's write path is copy-on-write: to append to a table it first
    # reads the current snapshot from S3, then writes a new one on top. Two
    # persistent stores must agree for that to work — the raft meta store at
    # `/var/lib/databend/meta` (mounted as the `databend_data` volume) tracks
    # which snapshot each table's HEAD points at, and MinIO's `databend`
    # bucket holds the actual snapshot / segment / parquet objects.
    #
    # If we wiped ONLY the bucket, the raft meta would still reference
    # snapshot IDs that no longer exist and every subsequent flush would 404
    # on `s3.GetObject → NoSuchKey`. mang-proxy's `CREATE DATABASE / TABLE
    # IF NOT EXISTS` migration is idempotent so it can't recover from a
    # drifted catalog — the DDL succeeds silently while writes still miss.
    # The prior version of this block did exactly that.
    #
    # Drop BOTH sides together: wipe the S3 bucket AND remove the
    # databend_data volume that holds the meta. On restart databend boots
    # empty, mang-proxy re-runs migrations, and the fresh catalog points at
    # a fresh snapshot chain that only references objects the empty bucket
    # is about to receive.
    if do_databend:
        say("Databend: resetting bucket + meta together")
        # Discover the actual named-volume name once. Compose prefixes named
        # volumes with its project name (`clode_databend_data` today); pull it
        # from `docker compose config` so a project-name change here doesn't
        # silently no-op the volume rm.
        project_name = s.compose_config().get("name")
        meta_vol = f"{project_name or 'clode'}_databend_data"
        if DRY_RUN:
            print("  \033[2m$\033[0m docker compose rm -sf databend mang-proxy")
            print("  \033[2m$\033[0m mc rm --recursive --force local/databend/")
            print(f"  \033[2m$\033[0m docker volume rm -f {meta_vol}")
            print("  \033[2m$\033[0m docker compose up -d databend mang-proxy")
        else:
            # Stop + rm the containers first so the meta volume detaches cleanly;
            # `docker volume rm` refuses while a container still references it.
            s.compose("rm", "-sf", "databend", "mang-proxy", capture=True)
            s.compose(
                "run", "--rm", "--entrypoint", "sh", "minio-setup",
                "-c", "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && mc rm --recursive --force local/databend/ >/dev/null 2>&1 || true",
                capture=True,
            )
            s.docker("volume", "rm", "-f", meta_vol, capture=True, check=False)
            s.compose("up", "-d", "databend", "mang-proxy", capture=True)
            ok("databend meta + bucket wiped in sync; mang-proxy will re-migrate on boot")

    # ── minio (brahmi-attachments) ────────────────────────────────────────
    # brahmi-attachments backs uploaded files (screenshots, artifacts) for
    # brahmi's Deployment.* endpoints. It's the ONLY user-writeable bucket
    # we ship — every other bucket is managed elsewhere:
    #   - databend/ is data, not attachments, and is coupled to the raft meta
    #     volume; --databend handles it in sync.
    # The bucket itself is preserved so the next boot's minio-setup step is
    # a no-op ("bucket exists"). We just empty its objects.
    if do_minio:
        say("MinIO: emptying brahmi-attachments (bucket kept)")
        if DRY_RUN:
            print("  \033[2m$\033[0m mc rm --recursive --force local/brahmi-attachments/")
        else:
            s.compose(
                "run", "--rm", "--entrypoint", "sh", "minio-setup",
                "-c", "mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && mc rm --recursive --force local/brahmi-attachments/ >/dev/null 2>&1 || true",
                capture=True,
            )
            ok("brahmi-attachments emptied")

    # ── reseed ────────────────────────────────────────────────────────────
    if reseed:
        if DRY_RUN:
            print()
            print("  \033[2m$\033[0m scripts/seed.py")
        else:
            say("Re-seeding")
            s.run([s.REPO_DIR / "scripts" / "seed.py"], cwd=s.REPO_DIR)

    say("done")


if __name__ == "__main__":
    main()
