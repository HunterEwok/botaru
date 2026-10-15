-- SPDX-License-Identifier: AGPL-3.0-only
-- Facts of bot memory and their offers to a successor (MEM-6, MEM-7, MEM-9). A fact lives in
-- the space of one identity of one tenant; the model named only its kind, predicate and
-- subject, and the core wrote the window, the evidence and the value.
CREATE TABLE facts (
    id               text PRIMARY KEY,
    tenant           text NOT NULL,
    bot              text NOT NULL,
    kind             text NOT NULL,
    predicate        text NOT NULL,
    subject          jsonb NOT NULL,
    value            jsonb NOT NULL,
    unit             text,
    window_from      timestamptz NOT NULL,
    window_to        timestamptz NOT NULL,
    evidence         jsonb NOT NULL DEFAULT '[]',
    state            text NOT NULL CHECK (state IN ('active', 'stale', 'sealed')),
    untrusted_origin boolean NOT NULL,
    personal_seen    boolean NOT NULL DEFAULT false,
    data_mark        text NOT NULL,
    verified_at      timestamptz NOT NULL,
    uses             int NOT NULL DEFAULT 0,
    last_used_at     timestamptz,
    acting_bot       text,
    bot_epoch        int,
    inherited        jsonb,
    created_at       timestamptz NOT NULL
);
CREATE INDEX facts_space ON facts (tenant, bot, state);

CREATE TABLE fact_offers (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    from_bot        text NOT NULL,
    role            text NOT NULL,
    seat            text,
    facts           text[] NOT NULL,
    state           text NOT NULL CHECK (state IN ('open', 'closed')),
    recipient       text,
    recipient_state text NOT NULL CHECK (recipient_state IN ('waiting', 'copied', 'expired')),
    copied          text[],
    open_until      timestamptz NOT NULL,
    created_at      timestamptz NOT NULL,
    closed_at       timestamptz
);
CREATE INDEX fact_offers_open ON fact_offers (tenant, state, role, seat);
