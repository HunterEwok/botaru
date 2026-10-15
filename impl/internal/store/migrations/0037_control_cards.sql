-- SPDX-License-Identifier: AGPL-3.0-only
-- The control card (MGR-26, decision point control_card): a seat operation on a target that
-- came from a finding (or empty finding: a cancellation in distrust mode, MGR-22, or a role share
-- over the limit, the why) waits for a person with control.approve for an hour, and an approved one
-- applies through control.apply on behalf of that person. control_ops.origin keeps the target
-- origin, code or finding:<id>; approved_by names the person of a card.
CREATE TABLE control_cards (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    request     jsonb NOT NULL,
    finding     text NOT NULL,
    why         text NOT NULL CHECK (why IN ('finding', 'distrust', 'share_held')),
    incident    text,
    task        text,
    acting_bot  text,
    epoch       int,
    state       text NOT NULL CHECK (state IN ('pending', 'applied', 'rejected', 'expired', 'failed')),
    nonce       text NOT NULL,
    decided_by  text,
    control_op  text,
    reason      text,
    expires_at  timestamptz NOT NULL,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE INDEX control_cards_pending ON control_cards (expires_at) WHERE state = 'pending';
ALTER TABLE control_ops ADD COLUMN approved_by text;
