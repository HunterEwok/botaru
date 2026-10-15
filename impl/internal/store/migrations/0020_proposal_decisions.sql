-- SPDX-License-Identifier: AGPL-3.0-only
-- The proposal digest and the decisions of people (DIG-1, DIG-2, DIG-3, PRP-4, PRP-18,
-- PRP-21): the card nonce and the time a proposal was shown, and one row per digest slot of a
-- tenant, so a second copy of the proposal loop does not send the same digest twice.
ALTER TABLE proposals ADD COLUMN nonce text;
ALTER TABLE proposals ADD COLUMN presented_at timestamptz;

CREATE TABLE proposal_digests (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    slot_at     timestamptz NOT NULL,
    urgent      boolean NOT NULL,
    body        jsonb NOT NULL,
    created_at  timestamptz NOT NULL,
    UNIQUE (tenant, slot_at, urgent)
);
