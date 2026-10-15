-- SPDX-License-Identifier: AGPL-3.0-only
-- Improvement proposals (INS-3, PRP-1 .. PRP-14): the record that only the core writes, by
-- spec/model/proposal-record.schema.json. The columns repeat the record fields that the core
-- filters and locks by; the record is the whole truth.
CREATE TABLE proposals (
    id           text PRIMARY KEY,
    tenant       text NOT NULL,
    role         text NOT NULL,
    worker       text NOT NULL,
    task         text NOT NULL,
    fingerprint  text NOT NULL,
    kind         text NOT NULL,
    state        text NOT NULL,
    record       jsonb NOT NULL,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL,
    state_at     timestamptz NOT NULL
);

CREATE INDEX proposals_open ON proposals (tenant, fingerprint) WHERE state IN ('screened', 'in_review', 'recommended', 'presented', 'parked');
CREATE INDEX proposals_role ON proposals (tenant, role, created_at);
CREATE INDEX proposals_declined ON proposals (tenant, fingerprint, state_at) WHERE state = 'declined';
