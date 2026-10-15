-- SPDX-License-Identifier: AGPL-3.0-only
-- Script runs (spec/lifecycles/script.md): the accepted draft and the fields the core computed
-- at acceptance are in record and never change (SCR-5); only the state, the reason, the steps
-- and the card fields move. A run waits for its initiator's "Run" button as the decision
-- subject script with a nonce (SCR-6).
CREATE TABLE script_runs (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    anchor_task     text,
    initiator       text NOT NULL,
    state           text NOT NULL CHECK (state IN ('proposed', 'running', 'paused', 'completed', 'failed', 'canceled', 'rejected')),
    reason          text,
    plan_hash       text NOT NULL,
    record          jsonb NOT NULL,
    idempotency_key text,
    nonce           text NOT NULL,
    expires_at      timestamptz NOT NULL,
    deadline_at     timestamptz,
    stopped         boolean NOT NULL DEFAULT false,
    tree_left       int NOT NULL DEFAULT 0,
    progress_at     timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE UNIQUE INDEX script_runs_key ON script_runs (tenant, idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE INDEX script_runs_live ON script_runs (tenant, state) WHERE state IN ('proposed', 'running', 'paused');
-- One row per step in the order of the draft; a step has at most one task, campaign, approval
-- or gate (SCR-8). A confirm step is its own decision subject script_gate with a nonce.
CREATE TABLE script_steps (
    run         text NOT NULL REFERENCES script_runs (id),
    n           int NOT NULL,
    id          text NOT NULL,
    kind        text NOT NULL,
    state       text NOT NULL CHECK (state IN ('pending', 'waiting', 'released', 'ok', 'failed', 'unknown', 'declined', 'skipped', 'canceled')),
    reason      text,
    task        text,
    campaign    text,
    approval    text,
    gate        text,
    nonce       text,
    expires_at  timestamptz,
    untrusted   jsonb,
    fields      jsonb,
    released_at timestamptz,
    ended_at    timestamptz,
    PRIMARY KEY (run, n),
    UNIQUE (run, id)
);
CREATE INDEX script_steps_task ON script_steps (task) WHERE task IS NOT NULL;
CREATE INDEX script_steps_campaign ON script_steps (campaign) WHERE campaign IS NOT NULL;
CREATE INDEX script_steps_approval ON script_steps (approval) WHERE approval IS NOT NULL;
CREATE UNIQUE INDEX script_steps_gate ON script_steps (gate) WHERE gate IS NOT NULL;
-- The reserve of a running run is one task-unit spend line with the run and no task; it
-- shrinks as steps are released and goes at the end (SCR-8).
ALTER TABLE spend ADD COLUMN script text;
