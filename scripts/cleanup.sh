#!/usr/bin/env bash
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
#                          volume rm every ec2mock-owned named volume.
#                          Container match set:
#                            (a) label aws.mock.instance-id (ec2mock's
#                                aramb-vm containers, image-agnostic)
#                            (b) ancestor ∈ {.configs[].settings.image
#                                from data/pool-manager-svc-configs.json,
#                                \$BENJI_IMAGE, ec2mock's live
#                                default-image} (pool-manager LOCAL_MODE
#                                kairos, ec2mock survivors before label
#                                sweep)
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

set -euo pipefail
cd "$(dirname "$0")/.."

# shellcheck source=lib/agent-sweep.sh
source scripts/lib/agent-sweep.sh

# ── defaults ──────────────────────────────────────────────────────────
DO_POSTGRES=0
DO_REDIS=0
DO_REDIS_MANG=0
DO_DATABEND=0
DO_MINIO=0
DO_AGENTS=0
RESEED=0
DRY_RUN=0
YES=0
DB_FILTER=()

usage() { sed -n '2,/^$/p' "$0" | sed 's/^# \{0,1\}//'; }

# Enumerate every per-service DB from postgres at runtime. Excludes
# template DBs and the bootstrap `postgres` DB. Sourced fresh on every
# run so a new service added to init-multiple-dbs.sql / seed.sh shows up
# here without a second edit. Empty result = postgres unreachable; the
# caller treats that as "nothing to clean" via the DBS guard below.
list_all_dbs() {
  docker compose exec -T -e PGPASSWORD=postgres db \
    psql -U postgres -tAc \
    "SELECT datname FROM pg_database WHERE datistemplate = false AND datname <> 'postgres' ORDER BY datname" \
    2>/dev/null | tr -d '\r' | grep -v '^$' || true
}

# ── arg parsing ───────────────────────────────────────────────────────
while (( $# > 0 )); do
  case "$1" in
    --postgres)    DO_POSTGRES=1 ;;
    --redis)       DO_REDIS=1 ;;
    --redis-mang)  DO_REDIS_MANG=1 ;;
    --databend)    DO_DATABEND=1 ;;
    --minio)       DO_MINIO=1 ;;
    --agents)      DO_AGENTS=1 ;;
    -a|--all)      DO_POSTGRES=1; DO_REDIS=1; DO_REDIS_MANG=1; DO_DATABEND=1; DO_MINIO=1; DO_AGENTS=1 ;;
    --reseed)      RESEED=1 ;;
    -n|--dry-run)  DRY_RUN=1 ;;
    -y|--yes)      YES=1 ;;
    -h|--help)     usage; exit 0 ;;
    --)            shift; while (( $# > 0 )); do DB_FILTER+=("$1"); shift; done; break ;;
    -*)            echo "unknown option: $1" >&2; usage; exit 2 ;;
    *)             DB_FILTER+=("$1") ;;
  esac
  shift
done

# If no source flag was set, default to everything.
if (( DO_POSTGRES==0 && DO_REDIS==0 && DO_REDIS_MANG==0 && DO_DATABEND==0 && DO_MINIO==0 && DO_AGENTS==0 )); then
  DO_POSTGRES=1; DO_REDIS=1; DO_REDIS_MANG=1; DO_DATABEND=1; DO_MINIO=1; DO_AGENTS=1
fi

if (( ${#DB_FILTER[@]} > 0 )); then
  DBS=("${DB_FILTER[@]}")
elif (( DO_POSTGRES )); then
  mapfile -t DBS < <(list_all_dbs)
  if (( ${#DBS[@]} == 0 )); then
    warn "postgres returned no databases (container down? init still running?) — skipping --postgres"
    DO_POSTGRES=0
    DBS=()
  fi
else
  DBS=()
fi

# Auto-couple: truncating pool-manager's DB strands every kairo container
# it launched (its svc_deployments row is gone). Same for brahmi's DB
# and the aramb-vm containers that ec2mock spawned — brahmi's
# gateway_deployments / vm_pool_slots rows tracked them and the row is
# gone. Force the container sweep on in either case even if --agents
# wasn't passed.
if (( DO_POSTGRES )) && printf '%s\n' "${DBS[@]}" | grep -qxE 'pool-manager|brahmi'; then
  DO_AGENTS=1
fi

# ── helpers ───────────────────────────────────────────────────────────
say()  { printf '\n\033[1;36m[clean]\033[0m %s\n' "$*"; }
ok()   { (( DRY_RUN )) && return 0; printf '  \033[32m✓\033[0m %s\n' "$*"; }
skip() { printf '  \033[33m·\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*" >&2; }

# `run` either prints the command (dry-run) or executes it via bash -c so
# pipelines and redirections survive verbatim.
run() {
  if (( DRY_RUN )); then
    printf '  \033[2m$\033[0m %s\n' "$*"
  else
    bash -c "$*"
  fi
}

PSQL=(docker compose exec -T -e PGPASSWORD=postgres db psql -U postgres -v ON_ERROR_STOP=1)

# Known migration-tracker tables across the Go/JS ecosystems we ship.
# Preserved so a TRUNCATE doesn't fool a service into rerunning every
# migration on next boot.
KEEP_TABLES="('schema_migrations','migrations','goose_db_version','atlas_schema_revisions','knex_migrations','knex_migrations_lock')"

# ── preview / confirm ─────────────────────────────────────────────────
say "Cleanup plan"
(( DO_POSTGRES   )) && echo "  • postgres: TRUNCATE … RESTART IDENTITY CASCADE on every non-migration table in: ${DBS[*]}"
(( DO_REDIS      )) && echo "  • redis DB 0: FLUSHDB (raksha tokens + brahmi routing)"
(( DO_REDIS_MANG )) && echo "  • redis DB 1: FLUSHDB — mang-proxy platform keys will be gone"
(( DO_DATABEND   )) && echo "  • databend: wipe MinIO bucket + drop databend_data volume + restart databend + mang-proxy"
(( DO_MINIO      )) && echo "  • minio: empty brahmi-attachments bucket (bucket kept; databend bucket handled by --databend)"
(( DO_AGENTS     )) && echo "  • agents: docker rm -fv agent containers (by label, image, and kairo- name) + docker volume rm ec2mock-owned volumes (see --agents in help for match set)"
(( RESEED        )) && echo "  • after: scripts/seed.sh"
(( DRY_RUN       )) && echo "  (dry-run — nothing will be executed)"

if (( !YES && !DRY_RUN )); then
  read -rp "Proceed? [y/N] " ans
  [[ "$ans" =~ ^[Yy] ]] || { echo "aborted"; exit 0; }
fi

# ── postgres ──────────────────────────────────────────────────────────
if (( DO_POSTGRES )); then
  say "Postgres: truncating user tables"
  # One PL/pgSQL DO block per DB. format()+%I quoting handles the dashed
  # pool-manager DB name safely. RESTART IDENTITY rewinds sequences so
  # serial IDs don't keep climbing across cleanups.
  read -r -d '' TRUNCATE_SQL <<SQL || true
DO \$\$
DECLARE r RECORD;
BEGIN
  FOR r IN
    SELECT tablename FROM pg_tables
    WHERE schemaname = 'public'
      AND tablename NOT IN ${KEEP_TABLES}
  LOOP
    EXECUTE format('TRUNCATE TABLE %I RESTART IDENTITY CASCADE', r.tablename);
  END LOOP;
END
\$\$;
SQL

  for db in "${DBS[@]}"; do
    if (( DRY_RUN )); then
      printf '  \033[2m$\033[0m psql -d %s -c "<TRUNCATE all non-migration tables CASCADE>"\n' "$db"
      continue
    fi
    if "${PSQL[@]}" -d "$db" -c "$TRUNCATE_SQL" >/dev/null 2>&1; then
      ok "$db: truncated"
    else
      warn "$db: TRUNCATE failed (DB missing or service hasn't migrated yet?)"
    fi
  done

  # raksha's admin/bot identity rows are BOOT-FATAL (serve validates them
  # at startup) and their embedded ride-along seed only fires on databases
  # that haven't applied the last migration — which a truncate deliberately
  # is not (schema_migrations is in KEEP_TABLES). Without this, a plain
  # `cleanup --postgres` leaves raksha crashlooping until someone reseeds,
  # and the next `up` hangs on its health gate. Re-apply the seed directly;
  # it's the same idempotent SQL gen-build-cache.sh bakes into the image.
  if [[ " ${DBS[*]} " == *" raksha "* ]] && (( ! DRY_RUN )); then
    if "${PSQL[@]}" -d raksha < seeds/raksha-seed.sql >/dev/null 2>&1; then
      ok "raksha: boot-critical identity seed re-applied (seeds/raksha-seed.sql)"
    else
      warn "raksha: identity seed re-apply failed — raksha will crashloop until './stack.sh seed' runs"
    fi
  fi

  # cha-ching's migration-seeded reference data (tier quota defaults +
  # credit catalogue) sits in regular tables, so the truncate above wipes
  # it while schema_migrations survives — migrations never re-run and
  # every subsequent org intake fails its quota-seed step (org_tiers row
  # lands, org_llm/cloud_quotas never do). Re-apply the same idempotent
  # SQL gen-build-cache.sh bakes into the image.
  if [[ " ${DBS[*]} " == *" chaching "* ]] && (( ! DRY_RUN )); then
    if "${PSQL[@]}" -d chaching < seeds/cha-ching-seed.sql >/dev/null 2>&1; then
      ok "chaching: tier-defaults + credit-catalogue seed re-applied (seeds/cha-ching-seed.sql)"
    else
      warn "chaching: seed re-apply failed — org intakes will half-fail until './stack.sh seed' runs"
    fi
  fi
fi

# ── redis DB 0 (raksha + brahmi) ──────────────────────────────────────
if (( DO_REDIS )); then
  say "Redis DB 0: FLUSHDB"
  run "docker compose exec -T redis redis-cli -a clode-redis-local -n 0 FLUSHDB >/dev/null"
  ok "redis DB 0 flushed"
fi

# ── redis DB 1 (mang-proxy) ───────────────────────────────────────────
if (( DO_REDIS_MANG )); then
  say "Redis DB 1: FLUSHDB"
  run "docker compose exec -T redis redis-cli -a clode-redis-local -n 1 FLUSHDB >/dev/null"
  ok "redis DB 1 flushed — re-export provider tokens + ./stack.sh seed (or pass --reseed)"
fi

# ── agent containers + volumes (delegated to shared lib) ──────────────
# Every "agent" container in this stack lives outside compose:
#   - pool-manager LOCAL_MODE spawns kairo containers via the docker
#     socket and tracks them in pool-manager.svc_deployments.
#   - ec2mock spawns aramb-vm containers (name = i-<hex>) and tracks
#     them in brahmi.gateway_deployments / vm_pool_slots + labels every
#     container with aws.mock.instance-id and every volume with
#     aws.mock.owned=true.
# Both classes disappear from tracking on a --postgres cleanup and become
# orphans. Sweep containers by (label ∪ image ∩ network=clode) and volumes
# by aws.mock.owned label. Both filters live in scripts/lib/agent-sweep.sh
# so wipe.sh sees the same source of truth.
if (( DO_AGENTS )); then
  say "agents: containers on the \`clode\` network + ec2mock-owned volumes"
  mapfile -t _imgs < <(agent_images)
  if (( ${#_imgs[@]} > 0 )); then
    printf '  images: %s\n' "${_imgs[*]}"
  fi
  sweep_agent_containers "$DRY_RUN"
  sweep_agent_volumes    "$DRY_RUN"
  ok "agent sweep complete"
fi

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
if (( DO_DATABEND )); then
  say "Databend: resetting bucket + meta together"
  # Discover the actual named-volume name once. Compose prefixes named
  # volumes with its project name (`clode_databend_data` today); pull it
  # from `docker compose config` so a project-name change here doesn't
  # silently no-op the volume rm.
  project_name=$(docker compose config --format json 2>/dev/null \
    | sed -n 's/.*"name" *: *"\([^"]*\)".*/\1/p' | head -1)
  meta_vol="${project_name:-clode}_databend_data"
  if (( DRY_RUN )); then
    printf '  \033[2m$\033[0m docker compose rm -sf databend mang-proxy\n'
    printf '  \033[2m$\033[0m mc rm --recursive --force local/databend/\n'
    printf '  \033[2m$\033[0m docker volume rm -f %s\n' "$meta_vol"
    printf '  \033[2m$\033[0m docker compose up -d databend mang-proxy\n'
  else
    # Stop + rm the containers first so the meta volume detaches cleanly;
    # `docker volume rm` refuses while a container still references it.
    docker compose rm -sf databend mang-proxy >/dev/null
    docker compose run --rm --entrypoint sh minio-setup \
      -c 'mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && mc rm --recursive --force local/databend/ >/dev/null 2>&1 || true' \
      >/dev/null
    docker volume rm -f "$meta_vol" >/dev/null 2>&1 || true
    docker compose up -d databend mang-proxy >/dev/null
    ok "databend meta + bucket wiped in sync; mang-proxy will re-migrate on boot"
  fi
fi

# ── minio (brahmi-attachments) ────────────────────────────────────────
# brahmi-attachments backs uploaded files (screenshots, artifacts) for
# brahmi's Deployment.* endpoints. It's the ONLY user-writeable bucket
# we ship — every other bucket is managed elsewhere:
#   - databend/ is data, not attachments, and is coupled to the raft meta
#     volume; --databend handles it in sync.
# The bucket itself is preserved so the next boot's minio-setup step is
# a no-op ("bucket exists"). We just empty its objects.
if (( DO_MINIO )); then
  say "MinIO: emptying brahmi-attachments (bucket kept)"
  if (( DRY_RUN )); then
    printf '  \033[2m$\033[0m mc rm --recursive --force local/brahmi-attachments/\n'
  else
    docker compose run --rm --entrypoint sh minio-setup \
      -c 'mc alias set local http://minio:9000 minioadmin minioadmin >/dev/null && mc rm --recursive --force local/brahmi-attachments/ >/dev/null 2>&1 || true' \
      >/dev/null
    ok "brahmi-attachments emptied"
  fi
fi

# ── reseed ────────────────────────────────────────────────────────────
if (( RESEED )); then
  if (( DRY_RUN )); then
    echo
    printf '  \033[2m$\033[0m scripts/seed.sh\n'
  else
    say "Re-seeding"
    scripts/seed.sh
  fi
fi

say "done"
