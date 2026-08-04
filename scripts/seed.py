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
EC2MOCK_URL = "http://ec2mock.localhost:8080"


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

    # ── 1. raksha admin user + org + service account ─────────────────────
    # Post-slack-native-foundation schema: every resource is org-owned. The
    # admin org gets the same UUID as the admin user so pool-owner identity
    # is unambiguous — `service_accounts.org_id` FKs to `organizations(id)`
    # and `org_members` records the ownership (UNIQUE WHERE role='owner').
    if in_scope("raksha"):
        say("raksha: admin user + org + default service account")
        s.psql("raksha", args=["-v", "owner_id=%s" % ADMIN_USER_ID], sql="""INSERT INTO users (id, email, name)
VALUES (:'owner_id', 'admin@local', 'Admin / Pool Owner')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name)
VALUES (:'owner_id', 'Admin Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
VALUES (:'owner_id', :'owner_id', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default)
VALUES (:'owner_id', 'pool-manager-default', true)
ON CONFLICT DO NOTHING;
""")
        ok("raksha seeded")

        # ── 1b. raksha-notify identity ──────────────────────────────────────
        # Separate org so raksha's outbound token to notify (organization_id
        # claim) is attributable in notify's emails.created_by. Same
        # user_id == org_id convention as the admin identity above.
        #
        # Seeded even when notify itself isn't running: raksha's NOTIFY_ADMIN_*
        # env vars are always set (from x-admin-ids), and raksha validates the
        # user exists in `users` at boot — no row = fatal crashloop, regardless
        # of whether the notify service ever comes up.
        say("raksha: raksha-notify user + org + default service account")
        s.psql("raksha", args=["-v", "owner_id=%s" % NOTIFY_ADMIN_ORG_ID], sql="""INSERT INTO users (id, email, name)
VALUES (:'owner_id', 'raksha-notify@local', 'Raksha Notify')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name)
VALUES (:'owner_id', 'Raksha Notify Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
VALUES (:'owner_id', :'owner_id', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default)
VALUES (:'owner_id', 'raksha-notify-default', true)
ON CONFLICT DO NOTHING;
""")
        ok("raksha-notify identity seeded")

        # ── 1c. raksha-chaching identity ────────────────────────────────────
        # Mirrors 1b for the cha-ching upstream. Same rationale: raksha reads
        # CHACHING_ADMIN_ORG_ID + CHACHING_ADMIN_USER_ID at boot and validates
        # the user row exists (raksha/cmd/main.go:294) — no row = crashloop.
        # Seed unconditionally, since env vars are always set on raksha.
        say("raksha: raksha-chaching user + org + default service account")
        s.psql("raksha", args=["-v", "owner_id=%s" % CHACHING_ADMIN_ORG_ID], sql="""INSERT INTO users (id, email, name)
VALUES (:'owner_id', 'raksha-chaching@local', 'Raksha ChaChing')
ON CONFLICT (id) DO NOTHING;

INSERT INTO organizations (id, name)
VALUES (:'owner_id', 'Raksha ChaChing Org')
ON CONFLICT (id) DO NOTHING;

INSERT INTO org_members (org_id, user_id, role)
VALUES (:'owner_id', :'owner_id', 'owner')
ON CONFLICT (org_id, user_id) DO NOTHING;

INSERT INTO service_accounts (org_id, name, is_default)
VALUES (:'owner_id', 'raksha-chaching-default', true)
ON CONFLICT DO NOTHING;
""")
        ok("raksha-chaching identity seeded")

        # ── 1d. intervix OAuth 2.1 client (RFC 7591) ────────────────────────
        # Intervix drives its SPA sign-in through raksha's OAuth 2.1 code+PKCE
        # flow: intervix-web hits intervix's `/api/v1/auth/start`, intervix
        # composes a raksha /authorize URL using INTERVIX_OAUTH_CLIENT_ID +
        # INTERVIX_OAUTH_REDIRECT_URI, and raksha 302s the browser back to
        # the registered redirect_uri after consent. Silent refresh
        # (/api/v1/auth/refresh) also needs the client_id — with none set,
        # intervix short-circuits refresh with 502 (client_id must be
        # configured) and the SPA bounces to /login on the next expiry.
        #
        # Prod uses RFC 7591 Dynamic Client Registration (intervix/scripts/
        # register-oauth-client.sh POST /auth/oauth/register → random
        # client_id). For local dev we skip DCR and pin a known client_id
        # into the row directly so the compose env can hardcode it too. The
        # redirect_uri points at intervix-web's dev server (:3001) at the
        # /auth/callback route the SPA's AppRouter mounts.
        say("raksha: intervix OAuth client (client_id=intervix-local → http://localhost:3001/auth/callback)")
        s.psql("raksha", sql="""INSERT INTO oauth_clients (
  client_id,
  client_name,
  redirect_uris,
  grant_types,
  token_endpoint_auth_method,
  client_metadata
)
VALUES (
  'intervix-local',
  'intervix (local)',
  ARRAY['http://localhost:3001/auth/callback']::text[],
  ARRAY['authorization_code','refresh_token']::text[],
  'none',
  jsonb_build_object(
    'client_uri', 'http://localhost:3001',
    'scope',      'aramb',
    'contacts',   jsonb_build_array('ops@clode.space')
  )
)
ON CONFLICT (client_id) DO UPDATE
  SET client_name                = EXCLUDED.client_name,
      redirect_uris              = EXCLUDED.redirect_uris,
      grant_types                = EXCLUDED.grant_types,
      token_endpoint_auth_method = EXCLUDED.token_endpoint_auth_method,
      client_metadata            = EXCLUDED.client_metadata;
""")
        ok("intervix OAuth client seeded")

        # Kick raksha so it re-boots against the freshly-seeded user rows.
        # Its NOTIFY_ADMIN_USER_ID / CHACHING_ADMIN_USER_ID validation at
        # boot is what was crashlooping the container.
        say("raksha: restarting to pick up freshly-seeded identity rows")
        s.compose("restart", "raksha", capture=True)
        wait_healthy("raksha", RAKSHA_URL)
    else:
        skip("raksha not in scope — skipping admin/org/service-account seed")

    # ── 1d. cha-ching tier defaults + credit catalogue ────────────────────
    # Migration-seeded reference data (llm/cloud tier defaults, credit
    # products) lives in regular tables, so `cleanup --postgres` wipes it
    # while schema_migrations survives and migrations never re-run. Without
    # these rows every org intake half-fails: the org_tiers row lands but
    # the quota-seed step errors (no defaults row for DEFAULT_TIER), so no
    # cap ever reaches mang-proxy/jumbo. Same idempotent SQL the image
    # build appends to the last cha-ching migration.
    if in_scope("cha-ching"):
        say("cha-ching: tier defaults + credit catalogue")
        cha_sql = (s.REPO_DIR / "seeds" / "cha-ching-seed.sql").read_text()
        s.psql("chaching", sql=cha_sql, capture=True)
        ok("cha-ching reference data seeded")
    else:
        skip("cha-ching not in scope — skipping tier-defaults seed")

    # ── 2. jumbo project + application + draft canvas ─────────────────────
    # Schema post-mig-000032: polymorphic owner_type was DROPPED, every
    # resource is org-owned via a plain org_id column, and `created_by` was
    # renamed `created_by_member_id`. For the LOCAL pool project we treat
    # the admin user id as its own org id (no real raksha org needed — org_id
    # is just a UUID with no FK). Depends on raksha having seeded the admin.
    if in_scope("jumbo") and in_scope("raksha"):
        say("jumbo: pool project + application + draft canvas")
        s.psql(
            "jumbo",
            args=[
                "-v", "owner_id=%s" % ADMIN_USER_ID,
                "-v", "project_id=%s" % POOL_PROJECT_ID,
                "-v", "application_id=%s" % POOL_APPLICATION_ID,
            ],
            sql="""INSERT INTO projects (id, org_id, name, slug, created_by_member_id, is_default)
VALUES (:'project_id', :'owner_id', 'Pool Project', 'pool-project', :'owner_id', true)
ON CONFLICT (id) DO NOTHING;

INSERT INTO applications (id, project_id, org_id, name, slug, created_by_member_id)
VALUES (:'application_id', :'project_id', :'owner_id', 'Pool Application', 'pool-application', :'owner_id')
ON CONFLICT (id) DO NOTHING;

INSERT INTO canvas (application_id, org_id, body, is_draft, created_by_member_id, nodes, edges, viewport)
SELECT :'application_id', :'owner_id', '{}'::jsonb, true, :'owner_id',
       '[]'::jsonb, '[]'::jsonb, '{"x": 0, "y": 0, "zoom": 1}'::jsonb
WHERE NOT EXISTS (
  SELECT 1 FROM canvas
  WHERE application_id = :'application_id' AND is_draft = true AND is_deleted = false
);
""")
        ok("jumbo seeded")
    elif in_scope("jumbo"):
        skip("jumbo in scope but raksha isn't — skipping (admin identity would be dangling)")

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

    # ── 3b. ec2mock default image ────────────────────────────────────────
    # Push the kairo image from data/pool-manager-svc-configs.json to the
    # mock's admin API. Same source of truth as pool-manager's svc_configs
    # above — the two stay in lockstep because both derive from the same
    # jq path. The mock uses this as its RunInstances default: brahmi's
    # aramb-vm path sends a placeholder AGENT_VM_AMI_ID and the mock
    # substitutes the real docker image at launch. Gated on ec2mock being
    # in scope, so a stack running without it just skips this step.
    if in_scope("ec2mock"):
        KAIRO_JSON = s.REPO_DIR / "data" / "pool-manager-svc-configs.json"
        with open(KAIRO_JSON) as f:
            _cfgs = json.load(f)["configs"]
        _kairo_imgs = [c["settings"].get("image") for c in _cfgs if c["service_type"] == "kairo"]
        KAIRO_IMG = "\n".join("null" if i is None else str(i) for i in _kairo_imgs)
        if not KAIRO_IMG or KAIRO_IMG == "null":
            warn("no kairo image found in %s — ec2mock default_image not seeded" % KAIRO_JSON)
        else:
            say("ec2mock: pushing default_image=%s" % KAIRO_IMG)
            http, body = s.http(
                "PUT", "%s/_admin/config/default-image" % EC2MOCK_URL,
                data=json.dumps({"image": KAIRO_IMG}),
                headers={"Content-Type": "application/json"},
                timeout=None,
            )
            if http in (200, 204):
                ok("ec2mock default_image=%s" % KAIRO_IMG)
            else:
                warn("ec2mock PUT default-image: HTTP %s — %s" % (http, body))
    else:
        skip("ec2mock not in scope — skipping default_image seed")

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
