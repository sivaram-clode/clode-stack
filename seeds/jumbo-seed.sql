-- clode-stack local seed for jumbo: the pool project + application + draft
-- canvas the local pool uses. owner id == org id for the local pool (org_id is
-- a plain UUID, no FK); the UUIDs are the x-admin-ids constants in
-- docker-compose.yml — keep them in sync if that anchor rotates. Idempotent.
-- gen-build-cache appends this onto jumbo's last migration, so `jumbo migrate`
-- seeds a fresh DB itself (baseline + fresh forks alike).

INSERT INTO projects (id, org_id, name, slug, created_by_member_id, is_default)
VALUES ('e26e56c1-7fd0-458c-a611-584d174651ef', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894',
        'Pool Project', 'pool-project', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894', true)
ON CONFLICT (id) DO NOTHING;

INSERT INTO applications (id, project_id, org_id, name, slug, created_by_member_id)
VALUES ('ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c', 'e26e56c1-7fd0-458c-a611-584d174651ef',
        'b2290247-c2af-44c0-9b2d-1e5c5a6a4894', 'Pool Application', 'pool-application',
        'b2290247-c2af-44c0-9b2d-1e5c5a6a4894')
ON CONFLICT (id) DO NOTHING;

INSERT INTO canvas (application_id, org_id, body, is_draft, created_by_member_id, nodes, edges, viewport)
SELECT 'ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c', 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894',
       '{}'::jsonb, true, 'b2290247-c2af-44c0-9b2d-1e5c5a6a4894',
       '[]'::jsonb, '[]'::jsonb, '{"x": 0, "y": 0, "zoom": 1}'::jsonb
WHERE NOT EXISTS (
  SELECT 1 FROM canvas
  WHERE application_id = 'ad6e3042-9ec5-4e6f-81e6-b49b2c96b43c' AND is_draft = true AND is_deleted = false
);
