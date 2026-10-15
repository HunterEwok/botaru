-- SPDX-License-Identifier: AGPL-3.0-only
-- erase_match searches the edge stores too (SEC-11): a match step per sink and monitoring
-- adapter of the tenant gets the needles through claim(erasure) with op match and answers
-- with ids only. The request keeps the needles until the last step answered and shows its
-- card only then.
ALTER TABLE subject_requests ADD COLUMN needles text[];
CREATE TABLE subject_match_steps (
    id          text PRIMARY KEY,
    request     text NOT NULL REFERENCES subject_requests (id),
    tenant      text NOT NULL,
    store       text NOT NULL,           -- the key of the store, as in the erasure results
    kind        text NOT NULL CHECK (kind IN ('sink', 'monitor')),
    target      text NOT NULL,
    state       text NOT NULL CHECK (state IN ('pending', 'leased', 'done', 'failed', 'not_applicable')),
    reason      text,
    lease_no    int NOT NULL DEFAULT 0,
    run_at      timestamptz,
    updated_at  timestamptz NOT NULL,
    UNIQUE (request, store)
);
CREATE INDEX subject_match_steps_queue ON subject_match_steps (tenant, kind, target, run_at) WHERE state = 'pending';
