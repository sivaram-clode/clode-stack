-- gen-skills-registry-seed.sql
-- Emits a full, idempotent clode-stack seed of every PUBLIC skill in the dev
-- Skills Registry DB plus the repos they reference. Run against the DEV DB
-- (schema `clode`) and redirect stdout into the seed file:
--
--   psql "$DEV_SKILLS_DSN" --no-psqlrc -Aqt \
--     -f scripts/gen-skills-registry-seed.sql > seeds/skills-registry-seed.sql
--
-- Why a generator (not a static dump committed here): the full public set is
-- ~8.3k skills / ~9.5 MB — too large to route through the Metabase MCP that is
-- the assistant's only dev-read path. Postgres does all literal escaping, so
-- this is safe for the arbitrary text / arrays in the data.
--
-- Design notes:
--   * PUBLIC only  -> is_private = false  (visible to everyone, per request)
--   * repos first  -> skills.repo_id has an FK to repos(id)
--   * search_tsv   -> GENERATED ALWAYS on skills; intentionally NOT inserted
--                     (Postgres recomputes it on insert)
--   * is_featured  -> exists on dev but NOT in this repo's skills-registry
--                     migrations; omitted so the seed loads on the local schema
--   * bare table names -> resolve via the local stack's search_path (public)
--   * idempotent   -> ON CONFLICT (id) DO NOTHING, so it rides any migration
--                     and re-applies cleanly after a cleanup/reseed.

SET search_path = clode;

WITH lines(k, seq, txt) AS (
  ---------------------------------------------------------------- header
            SELECT 0, 0, '-- clode-stack seed for skills-registry — AUTO-GENERATED from dev.'
  UNION ALL SELECT 0, 1, '-- Full PUBLIC mirror: every is_private=false skill + the repos it references.'
  UNION ALL SELECT 0, 2, '-- Regenerate: psql "$DEV_SKILLS_DSN" --no-psqlrc -Aqt -f scripts/gen-skills-registry-seed.sql > seeds/skills-registry-seed.sql'
  UNION ALL SELECT 0, 3, '-- Idempotent (ON CONFLICT DO NOTHING). skills.search_tsv is generated → omitted.'
  UNION ALL SELECT 0, 4, ''
  UNION ALL SELECT 0, 5, '-- repos (parent of the skills.repo_id foreign key)'

  ---------------------------------------------------------------- repos
  UNION ALL
  SELECT 1, 0, format(
    'INSERT INTO repos (id,owner,name,url,license,stars,forks,owner_name,owner_url,owner_avatar_url,skills_path,last_parsed_at,created_at,updated_at) '
    || 'VALUES (%L,%L,%L,%L,%L,%s,%s,%L,%L,%L,%L,%L,%L,%L) ON CONFLICT (id) DO NOTHING;',
    id, owner, name, url, license, stars, forks, owner_name, owner_url,
    owner_avatar_url, skills_path, last_parsed_at, created_at, updated_at)
  FROM repos r
  WHERE r.id IN (SELECT DISTINCT repo_id FROM skills
                 WHERE is_private = false AND repo_id IS NOT NULL)

  ---------------------------------------------------------------- skills
  UNION ALL SELECT 2, -2, ''
  UNION ALL SELECT 2, -1, '-- skills (public only)'
  UNION ALL
  SELECT 2, 0, format(
    'INSERT INTO skills (id,owner_id,slug,full_id,name,description,license,compatibility,allowed_tools,source_type,source_url,status,download_count,star_count,created_at,updated_at,published_at,repo_id,author_name,author_url,author_avatar_url,author_slug,category,tags,is_private) '
    || 'VALUES (%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L,%L) ON CONFLICT (id) DO NOTHING;',
    id, owner_id, slug, full_id, name, description, license, compatibility,
    allowed_tools, source_type, source_url, status, download_count, star_count,
    created_at, updated_at, published_at, repo_id, author_name, author_url,
    author_avatar_url, author_slug, category, tags, is_private)
  FROM skills
  WHERE is_private = false
)
SELECT txt FROM lines ORDER BY k, seq;
