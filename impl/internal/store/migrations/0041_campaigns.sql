-- SPDX-License-Identifier: AGPL-3.0-only
-- Campaigns (spec/lifecycles/campaign.md): the frozen plan is in record (template, rollout,
-- then, mark, untrusted_seen, reserve, enqueued_by) and never changes; only the state, the
-- reasons, the counters of campaign_items and the card fields move (ORC-1, ORC-4). A campaign
-- with a card is its own decision subject with a nonce (ORC-2).
CREATE TABLE campaigns (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    parent_task     text,
    initiator       text NOT NULL,
    state           text NOT NULL CHECK (state IN ('proposed', 'running', 'paused', 'stopping', 'completed', 'failed', 'canceled', 'rejected')),
    pause_reason    text,
    stop_reason     text,
    plan_hash       text NOT NULL,
    record          jsonb NOT NULL,
    idempotency_key text,
    nonce           text NOT NULL,
    card            boolean NOT NULL,
    expires_at      timestamptz,
    decided_by      text,
    deadline_at     timestamptz,
    gate_at         timestamptz,
    progress_at     timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE UNIQUE INDEX campaigns_key ON campaigns (tenant, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX campaigns_live ON campaigns (tenant, state) WHERE state IN ('proposed', 'running', 'paused', 'stopping');
CREATE TABLE campaign_items (
    campaign    text NOT NULL REFERENCES campaigns (id),
    n           int NOT NULL,
    resource    text NOT NULL,
    args        jsonb NOT NULL,
    args_hash   text NOT NULL,
    state       text NOT NULL CHECK (state IN ('pending', 'released', 'ok', 'failed', 'unknown', 'skipped', 'canceled')),
    task        text,
    wait        text,
    untrusted   boolean NOT NULL DEFAULT false,
    summary     jsonb,
    released_at timestamptz,
    ended_at    timestamptz,
    PRIMARY KEY (campaign, n)
);
CREATE INDEX campaign_items_task ON campaign_items (task) WHERE task IS NOT NULL;
-- The reserve of a campaign is one task-unit spend line with the campaign and no task; it
-- shrinks as items are released and goes at the end (ORC-8).
ALTER TABLE spend ADD COLUMN campaign text;
-- The write call of a campaign item carries its number: the plugin key is <campaign>:<n> (ORC-3).
ALTER TABLE toolcalls ADD COLUMN campaign_item int;
