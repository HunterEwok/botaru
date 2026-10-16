-- SPDX-License-Identifier: AGPL-3.0-only
-- The tenant lifecycle past active (LCY-1, spec/lifecycles/tenant.md). suspended is derived from
-- a hold of the tenant scope with the reason suspended and has no row here. A row in the state
-- deleted is the tombstone of the tenant id: it stays forever and the name is not used again
-- (RET-3).
CREATE TABLE tenant_states (
    tenant        text PRIMARY KEY,
    state         text NOT NULL CHECK (state IN ('offboarding', 'deleted')),
    hold          text NOT NULL,
    offboarded_at timestamptz NOT NULL,
    export_until  timestamptz NOT NULL,
    deleted_at    timestamptz,
    erasure       text
);
