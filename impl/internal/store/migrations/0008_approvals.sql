-- SPDX-License-Identifier: AGPL-3.0-only
-- Action approvals (spec/lifecycles/approval.md, schema approval): the record the card is drawn
-- from (APR-3), decided once by a person (APR-4) and executed at most once (APR-7).

CREATE TABLE approvals (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    source_task   text,
    initiator     text NOT NULL,
    action        text NOT NULL,
    args_hash     text NOT NULL,
    state         text NOT NULL CHECK (state IN ('pending', 'approved', 'executing', 'executed', 'exec_failed',
                      'exec_unknown', 'rejected', 'expired', 'canceled')),
    record        jsonb NOT NULL,         -- the approval schema record without the state fields
    nonce         text NOT NULL,
    expires_at    timestamptz NOT NULL,
    reminded_at   timestamptz,            -- the reminder at half of the deadline (APR-19)
    remind_at     timestamptz,            -- the last "Remind approvers" press (APR-19)
    cards_at      timestamptz,            -- when the cards were planned
    decided_by    text,
    decided_at    timestamptz,
    decision      text,
    reason        text,
    action_task   text,
    toolcall      text,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL
);
CREATE INDEX approvals_pending ON approvals (tenant, expires_at) WHERE state = 'pending';
CREATE INDEX approvals_source ON approvals (source_task) WHERE source_task IS NOT NULL;
CREATE INDEX approvals_created ON approvals (tenant, created_at);
-- An identical pending request of the same initiator and source returns the existing approval
-- (APR-2).
CREATE UNIQUE INDEX approvals_same ON approvals (tenant, action, args_hash, initiator, coalesce(source_task, '')) WHERE state = 'pending';

-- Openings of "Details" of a card by a person, for details_opened of the decision event (APR-11).
CREATE TABLE card_views (
    subject_kind  text NOT NULL,
    subject_id    text NOT NULL,
    principal     text NOT NULL,
    opened_at     timestamptz NOT NULL,
    PRIMARY KEY (subject_kind, subject_id, principal)
);

-- The card a delivery shows: the subject of a decision card, an initiator copy or a card update.
ALTER TABLE deliveries ADD COLUMN subject_kind text;
ALTER TABLE deliveries ADD COLUMN subject_id text;
CREATE INDEX deliveries_subject ON deliveries (subject_kind, subject_id) WHERE subject_id IS NOT NULL;
