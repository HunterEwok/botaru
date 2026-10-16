-- SPDX-License-Identifier: AGPL-3.0-only
-- Cards, holds and delivery preferences of v0.2 (APR-15, APR-16, HLD-8, DLV-10, REP-10, MON-9,
-- CAP-4).

-- The votes of an approval with approvals: 2, each press separately (APR-16), and the pause of
-- a pending approval under a pause_action hold: when the current pause began and how long the
-- approval was pending under such holds in total, at most 7 days (HLD-8).
ALTER TABLE approvals ADD COLUMN votes jsonb NOT NULL DEFAULT '[]';
ALTER TABLE approvals ADD COLUMN paused_since timestamptz;
ALTER TABLE approvals ADD COLUMN paused_total_s bigint NOT NULL DEFAULT 0;

-- A person's notification preferences (Principal.prefs, DLV-10).
ALTER TABLE principals ADD COLUMN prefs jsonb;

-- The first delivery of a non-urgent card of a subject to any recipient: the subject's expiry
-- counts from it and is rewritten once (APR-15).
CREATE TABLE card_first_deliveries (
    subject_kind text NOT NULL,
    subject_id   text NOT NULL,
    delivered_at timestamptz NOT NULL,
    PRIMARY KEY (subject_kind, subject_id)
);

-- Cards of non-urgent decisions above a person's daily limit, counted per tenant and day
-- without names (APR-15).
CREATE TABLE card_excess (
    tenant text NOT NULL,
    day    date NOT NULL,
    count  int NOT NULL,
    PRIMARY KEY (tenant, day)
);

-- When a delivery was handed to its executor; the card limit and the digest pace count by it
-- (APR-15, REP-10). The expiry of the subject of a deferred card orders the deferred cards.
ALTER TABLE deliveries ADD COLUMN handed_at timestamptz;
ALTER TABLE deliveries ADD COLUMN card_expires_at timestamptz;
-- A delivery collected into a person's daily digest names the digest delivery (DLV-10).
ALTER TABLE deliveries ADD COLUMN digest_of text;
-- A digest that waits for the pace of its chat; only such a digest is replaced by a newer one
-- (REP-10), never one that a person's preferences deferred (DLV-10).
ALTER TABLE deliveries ADD COLUMN paced boolean NOT NULL DEFAULT false;
CREATE INDEX deliveries_handed ON deliveries (tenant, recipient, handed_at) WHERE handed_at IS NOT NULL;

-- The digest pace of a chat binding (REP-10).
ALTER TABLE chat_bindings ADD COLUMN digest_max_per_hour int;

-- A maintenance window of the monitoring system on a node: the hold it set, and whether a
-- person released it, so the same window sets no new hold (MON-9).
CREATE TABLE maintenance_windows (
    node           text NOT NULL,
    maintenance_id text NOT NULL,
    tenant         text NOT NULL,
    hold           text NOT NULL,
    released       boolean NOT NULL DEFAULT false,
    created_at     timestamptz NOT NULL,
    ended_at       timestamptz,            -- the last ended event; the row is removed 30 days later
    PRIMARY KEY (node, maintenance_id)
);

-- The outcomes of the last slots of a schedule, for its auto-stop (CAP-4).
ALTER TABLE schedule_state ADD COLUMN failed_slots int NOT NULL DEFAULT 0;
