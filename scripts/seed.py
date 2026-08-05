#!/usr/bin/env python3
# clode-stack/seed.py — unified one-shot seeder for the local stack.
#
# Compose is the source of truth. This script discovers which services to
# seed by asking `docker compose ps` what's actually running under the
# current project — not from a hardcoded list in this file. Two consequences:
#
#   1. Profile-native. `./stack.sh up --profile skills` brings up
#      skills-registry, this script sees it, seeds it. `./stack.sh up`
#      (base only) leaves it out of scope, this script skips it silently.
#   2. Invocation-independent. `./stack.sh seed` run bare (no --profile
#      flag on the shell) still sees the same containers that are up, so
#      re-seeding after a wipe/config change works either way.
#
# Per-service DB names come from the running container's `DB_NAME` env var
# (docker inspect). To add a new service, set `DB_NAME` on it in compose —
# this script will create + backfill its DB with no code change here.
#
# What each in-scope service gets seeded with:
#   raksha           admin/pool-owner user + default service account
#   cha-ching        llm/cloud tier defaults + credit catalogue
#   jumbo            pool project + application + draft canvas          (needs raksha)
#   pool-manager     kairo svc_configs row(s) from data/*.json
#   skills-registry  admin JWT + skills import                          (needs raksha)
#
# Idempotency: SQL uses ON CONFLICT DO NOTHING/UPDATE; skills import is
# UPSERT by full_id.
#
# Provider tokens (CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_API_KEY, OPENAI_API_KEY,
# CODEX_OAUTH_TOKEN+REFRESH) are mang-proxy's concern — set them in
# ../mang-proxy/.env. The stack does not load or forward them.
#
# Run via `stack.sh up` (preferred) or directly after `docker compose up -d`.

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent / "lib"))
import stacklib as s

# ── identity ──────────────────────────────────────────────────────────
# Must mirror x-admin-ids in docker-compose.yml exactly.
ADMIN_USER_ID = "b2290247-c2af-44c0-9b2d-1e5c5a6a4894"
POOL_PROJECT_ID = "e26e56c1-7fd0-458c-a611-584d174651ef"
POOL_APPLICATION_ID = "ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c"
# Dedicated identities raksha uses for outbound calls to its upstream
# clients. Each row seeds a raksha users + organizations pair with
# id == same UUID, linked via org_members(owner) — see step 1b/1c below.
# Must match the matching *_ADMIN_ORG_ID / *_ADMIN_USER_ID in
# docker-compose.yml's x-admin-ids anchor (both share the UUID).
NOTIFY_ADMIN_ORG_ID = "2e93b5aa-1c4d-4f70-8e1a-9b3c5d7f2e4c"
CHACHING_ADMIN_ORG_ID = "0d44278f-d900-4b9d-bdc2-a8a64e91d422"

# All HTTP goes through the traefik ingress on host 8080 — *.localhost
# resolves to loopback natively (systemd-resolved / RFC 6761).
RAKSHA_URL = "http://raksha.localhost:8080"
SKILLS_URL = "http://skills-registry.localhost:8080"
MOCK_SERVICES_URL = "http://mock-services.localhost:8080"


def say(msg):
    sys.stdout.write("\n\033[1;36m[seed]\033[0m %s\n" % msg)
    sys.stdout.flush()


def ok(msg):
    sys.stdout.write("  \033[32m✓\033[0m %s\n" % msg)
    sys.stdout.flush()


def skip(msg):
    sys.stdout.write("  \033[33m·\033[0m %s\n" % msg)
    sys.stdout.flush()


def warn(msg):
    sys.stderr.write("  \033[33m!\033[0m %s\n" % msg)
    sys.stderr.flush()


# The baseline postgres container. `docker exec -i -e PGPASSWORD=postgres db
# psql -U postgres -v ON_ERROR_STOP=1` is the shape every psql call below takes.
DB = s.db_container()


def psql_argv(*extra):
    return [
        "docker", "exec", "-i", "-e", "PGPASSWORD=postgres", DB,
        "psql", "-U", "postgres", "-v", "ON_ERROR_STOP=1",
        *[str(e) for e in extra],
    ]


def main():
    # ── discovery ─────────────────────────────────────────────────────────
    # Populate three maps ONCE from the live compose project. Every gate
    # below reads from these — no hardcoded service lists, no db_to_svc map.
    say("Discovery")

    # RUNNING[svc]=1 for every service with a live container. `--status` is a
    # stringArray in `docker compose ps` — it MUST be repeated per value, NOT
    # comma-separated. Compose v5.x silently returns zero rows for the CSV
    # form (matches nothing). Boot-time restart-loopers (waiting on their DB
    # to be backfilled below) show up in `restarting`; freshly-`docker
    # compose up`'d services may briefly be `created` before their process
    # starts. Cleanly-exited one-shots like minio-setup are excluded — they
    # don't need seeding.
    RUNNING = {}
    r = s.compose(
        "ps", "--services",
        "--status", "running",
        "--status", "restarting",
        "--status", "created",
        capture=True, check=False,
    )
    for svc in (r.stdout or "").splitlines():
        if svc:
            RUNNING[svc] = 1

    if len(RUNNING) == 0:
        warn("no compose services running under this project — nothing to seed")
        raise SystemExit(0)

    ok("in scope (%d): %s" % (len(RUNNING), "".join("%s " % k for k in RUNNING)))

    def is_running(svc):
        return svc in RUNNING

    # Alias — used at the seed-step gates so intent reads naturally.
    def in_scope(svc):
        return is_running(svc)

    # SVC_DB[svc]=<db-name> for every in-scope service whose container has
    # DB_NAME set. Reading from `docker inspect` is deliberately authoritative:
    # it survives however seed.sh was invoked (up.sh vs bare) and however the
    # compose env was merged (anchor merge, env_file, environment: override).
    SVC_DB = {}
    for svc in list(RUNNING):
        cid_out = s.compose("ps", "-q", svc, capture=True, check=False).stdout or ""
        cid_lines = cid_out.splitlines()
        cid = cid_lines[0] if cid_lines else ""
        if not cid:
            continue
        env_out = s.docker(
            "inspect", cid,
            "--format", "{{range .Config.Env}}{{println .}}{{end}}",
            capture=True, check=False,
        ).stdout or ""
        db = ""
        for line in env_out.splitlines():
            k, sep, v = line.partition("=")
            if k == "DB_NAME":
                db = v
                break
        if db:
            SVC_DB[svc] = db

    if len(SVC_DB) > 0:
        ok("db-owning services: %s" % "".join("%s→%s " % (svc, SVC_DB[svc]) for svc in SVC_DB))
    else:
        skip("no db-owning services in scope")

    # ── pre-flight ────────────────────────────────────────────────────────
    # Wait for the services the later steps actually depend on. Others fall
    # out of scope automatically.
    say("Pre-flight")

    def wait_healthy(name, url):
        if s.wait_healthy(url + "/health"):
            ok("%s reachable at %s" % (name, url))
        else:
            warn("%s not reachable at %s after 60s — continuing anyway" % (name, url))

    if in_scope("raksha"):
        wait_healthy("raksha", RAKSHA_URL)
    if in_scope("skills-registry"):
        wait_healthy("skills-registry", SKILLS_URL)

    # ── 0. ensure every per-service DB exists ─────────────────────────────
    # The postgres init script (db/init-multiple-dbs.sql) only runs the first
    # time the postgres volume is created; new services (or restoring against
    # a stale volume) will crash-loop in `migrate && serve` with "database
    # does not exist". This step backfills any missing DB, but ONLY for
    # services currently in scope — so removed/profile-gated services don't
    # leave dead DBs behind. Owner is `postgres` in both create paths.
    say("postgres: ensuring per-service DBs exist for in-scope services")
    created_dbs = []
    for svc in list(SVC_DB):
        dbname = SVC_DB[svc]
        exists = (s.psql(
            "postgres", args=[
                "-tAc",
                "SELECT 1 FROM pg_database WHERE datname='%s'" % dbname,
            ],
            capture=True, check=False,
        ).stdout or "")
        exists = "".join(exists.split())
        if exists == "1":
            skip("%s: present (%s)" % (dbname, svc))
        else:
            # CREATE DATABASE can't run inside a transaction; the double-quoted
            # identifier is safe for dashes (pool-manager) and underscores alike.
            s.psql("postgres", args=["-c", 'CREATE DATABASE "%s"' % dbname])
            ok("%s: created (%s)" % (dbname, svc))
            created_dbs.append(dbname)

    # If any DB was created above, the owning service crash-looped through
    # `migrate && serve` while the DB was missing. Bounce it so `migrate`
    # runs against the now-present DB before any downstream step talks to
    # it. Idempotent — bouncing a healthy container just cycles it briefly.
    if len(created_dbs) > 0:
        to_restart = []
        for db in created_dbs:
            for svc in SVC_DB:
                if SVC_DB[svc] == db:
                    to_restart.append(svc)
                    break
        if len(to_restart) > 0:
            say("kicking services whose DB was just created: %s" % " ".join(to_restart))
            s.compose("restart", *to_restart, capture=True)
            # Re-wait for the services later steps depend on.
            for svc in to_restart:
                if svc == "raksha":
                    wait_healthy("raksha", RAKSHA_URL)
                elif svc == "skills-registry":
                    wait_healthy("skills-registry", SKILLS_URL)

    # ── 1. static seeds: apply every seeds/<svc>-seed.sql to its own DB ────
    # SINGLE SOURCE, convention-driven. Each seeds/<svc>-seed.sql is the SAME
    # file gen-build-cache embeds onto that service's last migration, so
    # `<svc> migrate` seeds a fresh DB itself; applying it here is the
    # idempotent reseed backstop after a `cleanup` truncate (migrate no-ops on
    # an already-migrated schema). Onboarding a seeded service = drop the file,
    # no edit here. Each seed is single-DB and self-contained (literal UUIDs,
    # ON CONFLICT), so order is irrelevant. Services that boot-validate their
    # seeded rows (raksha) are bounced after a reseed.
    say("static seeds (seeds/<svc>-seed.sql → each service's own DB)")
    SEED_RESTART = {"raksha"}  # raksha crashloops until its identity rows exist
    for sf in sorted((s.REPO_DIR / "seeds").glob("*-seed.sql")):
        svc = sf.name[:-len("-seed.sql")]
        if svc not in SVC_DB:
            skip("%s: not in scope (or no DB_NAME) — skipping %s" % (svc, sf.name))
            continue
        s.psql(SVC_DB[svc], sql=sf.read_text(), capture=True)
        ok("%s seeded (%s → db %s)" % (svc, sf.name, SVC_DB[svc]))
        if svc in SEED_RESTART:
            say("%s: restarting to re-validate freshly-seeded rows" % svc)
            s.compose("restart", svc, capture=True)
            if svc == "raksha":
                wait_healthy("raksha", RAKSHA_URL)

    # ── 3. pool-manager svc_configs ───────────────────────────────────────
    # settings + vars come straight from production (metabase DB 12 →
    # clode.svc_configs WHERE service_type='kairo') and live in
    # data/pool-manager-svc-configs.json so the blob is auditable. The file
    # holds a `configs` array — one entry per service_type. Iterate + upsert.
    #
    # Some keys (settings.publicNet, settings.volumeMounts, settings.workspaceSize)
    # are k8s-only and pool-manager's DockerDeployer ignores them; they stay
    # in the row as-is for parity with prod.
    if in_scope("pool-manager"):
        KAIRO_JSON = s.REPO_DIR / "data" / "pool-manager-svc-configs.json"
        # up.sh's build flags export the tag they actually built:
        #   --agent   → BENJI_IMAGE=clode-stack/benji:latest    (kairo* rows)
        #   --browser → BROWSER_IMAGE=clode-stack/brave-head:latest (aramb-browser row)
        # When set, each overrides the JSON's settings.image on its own rows so the
        # svc_configs match the locally-built tag. Each is scoped by service_type so
        # one build flag never clobbers the other family's image. Unset → the JSON
        # default (already a clode-stack/* tag) wins.
        benji_image = os.environ.get("BENJI_IMAGE", "")
        browser_image = os.environ.get("BROWSER_IMAGE", "")
        if benji_image:
            say("pool-manager: overriding kairo* image with BENJI_IMAGE=%s (--agent-built)" % benji_image)
        if browser_image:
            say("pool-manager: overriding aramb-browser image with BROWSER_IMAGE=%s (--browser-built)" % browser_image)
        with open(KAIRO_JSON) as f:
            configs = json.load(f)["configs"]
        for cfg in configs:
            st = cfg["service_type"]
            if st.startswith("kairo") and benji_image:
                cfg["settings"]["image"] = benji_image
            if st == "aramb-browser" and browser_image:
                cfg["settings"]["image"] = browser_image
            settings_json = json.dumps(cfg["settings"], separators=(",", ":"))
            vars_json = json.dumps(cfg["vars"], separators=(",", ":"))
            hot = json.dumps(cfg["hot_count"])
            cold = json.dumps(cfg["cold_count"])
            maxc = json.dumps(cfg["max_concurrent_deployments"])
            ena = json.dumps(cfg["enabled"])
            img_val = cfg["settings"].get("image")
            img_log = "null" if img_val is None else str(img_val)
            say("pool-manager: %s svc_configs (image=%s, hot=%s, cold=%s, max=%s)" % (st, img_log, hot, cold, maxc))
            s.psql(
                "pool-manager",
                args=[
                    "-v", "st=%s" % st,
                    "-v", "settings=%s" % settings_json,
                    "-v", "vars_json=%s" % vars_json,
                    "-v", "hot=%s" % hot,
                    "-v", "cold=%s" % cold,
                    "-v", "maxc=%s" % maxc,
                    "-v", "ena=%s" % ena,
                ],
                sql="""INSERT INTO svc_configs (service_type, settings, vars, config_hash, hot_count, cold_count, max_concurrent_deployments, enabled)
VALUES (
  :'st',
  :'settings'::jsonb,
  :'vars_json'::jsonb,
  '',
  :hot, :cold, :maxc, :ena
)
ON CONFLICT (service_type) DO UPDATE
  SET settings = EXCLUDED.settings,
      vars = EXCLUDED.vars,
      hot_count = EXCLUDED.hot_count,
      cold_count = EXCLUDED.cold_count,
      max_concurrent_deployments = EXCLUDED.max_concurrent_deployments,
      enabled = EXCLUDED.enabled,
      updated_at = now();
""")
        ok("pool-manager seeded")
    else:
        skip("pool-manager not in scope — skipping svc_configs seed")

    # ── 3b. mock-services default image ────────────────────────────────────────
    # Push the kairo image from data/pool-manager-svc-configs.json to the
    # mock's admin API. Same source of truth as pool-manager's svc_configs
    # above — the two stay in lockstep because both derive from the same
    # jq path. The mock uses this as its RunInstances default: brahmi's
    # aramb-vm path sends a placeholder AGENT_VM_AMI_ID and the mock
    # substitutes the real docker image at launch. Gated on mock-services being
    # in scope, so a stack running without it just skips this step.
    if in_scope("mock-services"):
        KAIRO_JSON = s.REPO_DIR / "data" / "pool-manager-svc-configs.json"
        with open(KAIRO_JSON) as f:
            _cfgs = json.load(f)["configs"]
        # Prefer the locally-built versioned benji tag (up.py exports BENJI_IMAGE
        # when it builds the pool image) over the config's (now versionless) value
        # — a versionless ref would push as :latest and never match the local build.
        _kairo_imgs = [c["settings"].get("image") for c in _cfgs if c["service_type"] == "kairo"]
        _cfg_img = next((str(i) for i in _kairo_imgs if i), "")
        KAIRO_IMG = os.environ.get("BENJI_IMAGE") or _cfg_img
        if not KAIRO_IMG:
            warn("no kairo image found in %s — mock-services default_image not seeded" % KAIRO_JSON)
        else:
            say("mock-services: pushing default_image=%s" % KAIRO_IMG)
            http, body = s.http(
                "PUT", "%s/_admin/config/default-image" % MOCK_SERVICES_URL,
                data=json.dumps({"image": KAIRO_IMG}),
                headers={"Content-Type": "application/json"},
                timeout=None,
            )
            if http in (200, 204):
                ok("mock-services default_image=%s" % KAIRO_IMG)
            else:
                warn("mock-services PUT default-image: HTTP %s — %s" % (http, body))
    else:
        skip("mock-services not in scope — skipping default_image seed")

    # ── 4. skills-registry — skills import (direct DB) ────────────────────
    # Skills are parsed from ../aramb-skills and UPSERTed straight into the
    # skills_registry DB, so this step is raksha-independent (no JWT needed).
    if in_scope("skills-registry"):
        # Parse SKILL.md files under ../aramb-skills locally and UPSERT repos +
        # skills + skill_versions directly into skills_registry. Bypasses
        # /api/v1/me/import (which walks GitHub anonymously and gets rate-limited
        # at 60 req/hr). Idempotent via ON CONFLICT DO UPDATE. Column overrides
        # for category/tags/etc. live in data/skill-overrides.json.
        say("skills-registry: seeding skills from ../aramb-skills (direct DB)")
        aramb_skills = s.REPO_DIR.parent / "aramb-skills"
        if not aramb_skills.is_dir():
            warn("../aramb-skills not found — skipping skills seed")
        elif not shutil.which("python3"):
            warn("python3 not on PATH — skipping skills seed")
        else:
            try:
                sk_branch = s.run(
                    ["git", "-C", str(aramb_skills), "rev-parse", "--abbrev-ref", "HEAD"],
                    capture=True, check=False,
                ).stdout.strip() or "?"
            except Exception:
                sk_branch = "?"
            try:
                sk_commit = s.run(
                    ["git", "-C", str(aramb_skills), "rev-parse", "--short", "HEAD"],
                    capture=True, check=False,
                ).stdout.strip() or "?"
            except Exception:
                sk_commit = "?"
            if sk_branch != "main":
                warn("../aramb-skills is on '%s', not main — seeding HEAD=%s as-is" % (sk_branch, sk_commit))
            # Run the local skill-emitter and pipe its SQL straight into psql,
            # capturing psql's combined stdout+stderr for the summary/tail below.
            emit = subprocess.run(
                [str(s.REPO_DIR / "scripts" / "seed-skills-from-local.py")],
                cwd=str(s.REPO_DIR),
                env={**os.environ, "ADMIN_USER_ID": ADMIN_USER_ID},
                stdout=subprocess.PIPE, text=True,
            )
            sink = subprocess.run(
                psql_argv("-d", "skills_registry"),
                input=emit.stdout or "",
                stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True,
            )
            sk_out = sink.stdout or ""
            if emit.returncode == 0 and sink.returncode == 0:
                # A no-match grep must not be fatal here.
                summary = ""
                for line in sk_out.splitlines():
                    if line.startswith("-- summary"):
                        summary = line
                        break
                ok(summary or "seeded skills from ../aramb-skills @%s" % sk_commit)
            else:
                warn("skills seed failed — output below")
                for line in sk_out.splitlines()[-30:]:
                    sys.stderr.write("    %s\n" % line)

    # ── 5. nudge restart-loopers ──────────────────────────────────────────
    # Services that depend on a seeded raksha row (pool-manager) or a freshly
    # backfilled DB (skills-registry) may still be restart-looping from boot.
    # They'd recover on their own via restart: unless-stopped, but a nudge
    # shaves ~30s off "stack is ready". Only kick services that are actually
    # in scope — no dangling references.
    kick = []
    if in_scope("pool-manager"):
        kick.append("pool-manager")
    if in_scope("skills-registry"):
        kick.append("skills-registry")
    if len(kick) > 0:
        say("post-seed: kick any restart-loopers (%s)" % " ".join(kick))
        s.compose("restart", *kick, capture=True)
        ok("restart issued")

    say("done")


if __name__ == "__main__":
    main()
