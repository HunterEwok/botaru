-- SPDX-License-Identifier: AGPL-3.0-only
-- Usage lines of model calls written before 0014 have no provider: a reconciliation of such a
-- day would see no farm spend and correct the whole provider sum on top of it (BUD-10). The
-- provider comes from the model profile in force at the time of the line, the tenant's own
-- before the farm-wide one. Only the applied version of a profile is known for sure, so a line
-- gets it only when that version was already applied at the line's time; an older line, or one
-- whose tenant profile came later, keeps no provider, and the reconciliation refuses its day
-- (conflict) rather than guess.
UPDATE spend s SET provider = lower(p.provider)
    FROM (SELECT s2.id,
            CASE WHEN t.name IS NOT NULL THEN CASE WHEN t.applied_at <= s2.time THEN t.doc->'spec'->>'provider' END
                 WHEN f.applied_at <= s2.time THEN f.doc->'spec'->>'provider' END AS provider
          FROM spend s2
          LEFT JOIN manifests t ON t.kind = 'ModelProfile' AND t.tenant = s2.tenant AND t.name = s2.profile
          LEFT JOIN manifests f ON f.kind = 'ModelProfile' AND f.tenant = '' AND f.name = s2.profile
          WHERE s2.provider IS NULL AND s2.profile IS NOT NULL AND s2.unit <> 'correction') p
    WHERE s.id = p.id AND p.provider IS NOT NULL;

INSERT INTO spend_workspaces (tenant, provider, first_at)
    SELECT tenant, provider, min(time) FROM spend WHERE provider IS NOT NULL AND unit <> 'correction' GROUP BY tenant, provider
    ON CONFLICT (tenant, provider) DO UPDATE SET first_at = least(spend_workspaces.first_at, EXCLUDED.first_at);
