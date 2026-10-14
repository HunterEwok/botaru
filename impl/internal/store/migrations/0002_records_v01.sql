-- SPDX-License-Identifier: AGPL-3.0-only
-- Fields that a v0.1 core writes into its records so that v0.2 needs no rewrite (D-94, D-98).

-- Events carry the tree root and the direct cause (OBS-2).
ALTER TABLE events ADD COLUMN correlationid text;
ALTER TABLE events ADD COLUMN causationid text;

-- The run fingerprint of an attempt with the hashes of its parts (PRV-1), and what the core
-- resolved for the attempt at claim: the catalog version and the enum_from values of the
-- result schema, filtered by the task's data mark (OUT-1).
ALTER TABLE leases ADD COLUMN fingerprint text;
ALTER TABLE leases ADD COLUMN attempt jsonb;

-- The usage line names the fingerprint of the attempt it belongs to (PRV-1).
ALTER TABLE spend ADD COLUMN fingerprint text;

-- The code texts moved to the skeleton catalog under new keys (DLV-9). Notices that are not
-- sent yet get the new keys; the core fills their new parameters at start. Sent deliveries and
-- the audit chain keep what was written.
UPDATE deliveries SET body = jsonb_set(body, '{text_key}', '"notice.ai"')
    WHERE state IN ('planned', 'sending') AND body->>'text_key' = 'ai_notice';
UPDATE deliveries SET body = jsonb_set(body, '{text_key}', '"notice.privacy"')
    WHERE state IN ('planned', 'sending') AND body->>'text_key' = 'privacy_notice';

-- Erasure requests (spec/model/deletion-request.schema.json). This version records the request
-- that offboarding creates (LCY-8); the erasure work on the edge stores comes later (RET-7).
CREATE TABLE erasure_requests (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    subject     text,                   -- the principal of the scope (tenant, subject)
    reason      text NOT NULL,
    author      jsonb NOT NULL,
    state       text NOT NULL CHECK (state IN ('requested', 'running', 'done', 'failed')),
    results     jsonb,
    created_at  timestamptz NOT NULL
);
CREATE INDEX erasure_requests_open ON erasure_requests (tenant, created_at) WHERE state IN ('requested', 'running');
