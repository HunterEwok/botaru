-- Governance v0.2 (GOV-9..GOV-32): since when the rules hold in a tenant, the promotion track
-- record and queue, the promotion cases and the incident attribution candidates.
CREATE TABLE governance_tenants (
    tenant      text PRIMARY KEY,
    since       timestamptz NOT NULL,     -- the audit record governance.enabled (GOV-14)
    case_after  timestamptz,              -- no case before: swap_cooldown after expired or "Not now" (GOV-31)
    swap_after  timestamptz,              -- no freeing of a seat before: swap_cooldown after the last one (GOV-14)
    updated_at  timestamptz NOT NULL
);

-- One row per evaluated success of a functionary: a trigger or only the track record (GOV-14).
CREATE TABLE promotion_track (
    proposal    text PRIMARY KEY,
    tenant      text NOT NULL,
    bot         text NOT NULL,
    at          timestamptz NOT NULL,
    trigger     boolean NOT NULL,
    reason      text
);
CREATE INDEX promotion_track_bot ON promotion_track (tenant, bot, at);

-- The promotion queue (spec/model/promotion-entry.schema.json): one entry per bot.
CREATE TABLE promotion_entries (
    tenant                 text NOT NULL,
    bot                    text NOT NULL,
    state                  text NOT NULL CHECK (state IN ('queued', 'waiting_eligibility', 'in_case', 'removed')),
    trigger_proposal       text NOT NULL,
    trigger_at             timestamptz NOT NULL,
    cooldown_until         timestamptz,
    wrong_candidate_count  int NOT NULL DEFAULT 0,
    removed_reason         text,
    updated_at             timestamptz NOT NULL,
    PRIMARY KEY (tenant, bot)
);

-- Promotion cases (spec/model/governance-case.schema.json): record is the case; holders and
-- upheld keep the holders of L at opening and the vetoes people upheld, for seat_changed and the
-- flag evicts_upheld_veto.
CREATE TABLE governance_cases (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    state         text NOT NULL,
    record        jsonb NOT NULL,
    vacancy       text,
    holders       jsonb NOT NULL DEFAULT '{}',
    upheld        jsonb NOT NULL DEFAULT '{}',
    nonce         text,
    presented_at  timestamptz,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL
);
CREATE UNIQUE INDEX governance_cases_open ON governance_cases (tenant) WHERE state IN ('opened', 'confirming');

-- Incident attribution candidates (spec/model/attribution.schema.json).
CREATE TABLE attributions (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    incident      text NOT NULL,
    proposal      text NOT NULL,
    state         text NOT NULL CHECK (state IN ('candidate', 'unknown', 'confirmed', 'rejected', 'disputed', 'canceled')),
    record        jsonb NOT NULL,
    nonce         text,
    presented_at  timestamptz,
    reminded_at   timestamptz,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    UNIQUE (incident, proposal)
);
CREATE INDEX attributions_proposal ON attributions (proposal);
