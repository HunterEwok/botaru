-- SPDX-License-Identifier: AGPL-3.0-only
-- Usage lines of model calls written before 0014 have no provider: a reconciliation of such a
-- day would see no farm spend and correct the whole provider sum on top of it (BUD-10). The
-- provider comes from the applied model profile of the line, the tenant's own before the
-- farm-wide one.
UPDATE spend s SET provider = lower(coalesce(
        (SELECT m.doc->'spec'->>'provider' FROM manifests m WHERE m.kind = 'ModelProfile' AND m.tenant = s.tenant AND m.name = s.profile),
        (SELECT m.doc->'spec'->>'provider' FROM manifests m WHERE m.kind = 'ModelProfile' AND m.tenant = '' AND m.name = s.profile)))
    WHERE s.provider IS NULL AND s.profile IS NOT NULL AND s.unit <> 'correction';

INSERT INTO spend_workspaces (tenant, provider, first_at)
    SELECT tenant, provider, min(time) FROM spend WHERE provider IS NOT NULL AND unit <> 'correction' GROUP BY tenant, provider
    ON CONFLICT (tenant, provider) DO UPDATE SET first_at = least(spend_workspaces.first_at, EXCLUDED.first_at);
