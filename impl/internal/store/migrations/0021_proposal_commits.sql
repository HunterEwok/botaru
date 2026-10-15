-- SPDX-License-Identifier: AGPL-3.0-only
-- The previous versions of manifests an apply replaced (PRP-19, PRP-20): a commit linked to a
-- proposal after it was applied gets its reverse edit from the version that commit replaced.
CREATE TABLE manifest_versions (
    kind         text NOT NULL,
    tenant       text NOT NULL,
    name         text NOT NULL,
    doc          jsonb NOT NULL,
    commit       text,
    replaced_by  text,
    replaced_at  timestamptz NOT NULL
);
CREATE INDEX manifest_versions_key ON manifest_versions (kind, tenant, name, replaced_at);
