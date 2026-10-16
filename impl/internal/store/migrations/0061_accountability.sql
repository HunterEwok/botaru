-- SPDX-License-Identifier: AGPL-3.0-only
-- Accountability from v0.3 (COM-15..17, PRP-24, GOV-36..43, IDN-15): since when a tenant runs
-- on a core that claims v0.3, the review forecasts and their scores, link candidates to rejected
-- proposals and the departures they lead to.

-- The first apply of a tenant on a v0.3 core (governance.v03_enabled) and the first apply with
-- commission.accountability on (commission.accountability_on); ready_sent_at is the last
-- accountability_ready notice (GOV-36).
CREATE TABLE governance_v03 (
    tenant                   text PRIMARY KEY,
    since                    timestamptz NOT NULL,
    accountability_on_since  timestamptz,
    ready_sent_at            timestamptz,
    updated_at               timestamptz NOT NULL
);

-- One row per lens review written on a v0.3 core after governance_v03_since (COM-16): the
-- forecast, the warning, and the seat holder it was written for; a missing or invalid review
-- of a seat holder has no forecast and counts as a missed one (GOV-41).
CREATE TABLE forecasts (
    task        text PRIMARY KEY,
    tenant      text NOT NULL,
    proposal    text NOT NULL,
    lens        text NOT NULL,
    executor    text NOT NULL CHECK (executor IN ('seat', 'fallback_role', 'shadow')),
    bot         text,
    seat        text,
    seat_epoch  int,
    state       text NOT NULL CHECK (state IN ('done', 'invalid', 'missing')),
    forecast    jsonb,
    score       int,
    veto        boolean NOT NULL DEFAULT false,
    disagrees   boolean NOT NULL DEFAULT false,
    written_at  timestamptz NOT NULL,
    -- The last pass of the scoring that looked at the forecast.
    checked_at  timestamptz
);
CREATE INDEX forecasts_proposal ON forecasts (proposal);
CREATE INDEX forecasts_tenant ON forecasts (tenant, written_at);

-- The scores of a forecast by code (COM-16): one per source of the outcome (the trial, a
-- decline as too risky, a confirmed attribution, a blind sample answer, a resubmission), with
-- the weight 1/inclusion_prob of a sample answer.
CREATE TABLE forecast_scores (
    task         text NOT NULL,
    source       text NOT NULL,
    ref          text NOT NULL DEFAULT '',
    tenant       text NOT NULL,
    proposal     text NOT NULL,
    lens         text NOT NULL,
    outcome      text NOT NULL,
    brier        double precision NOT NULL,
    weight       double precision NOT NULL DEFAULT 1,
    attribution  text,
    scored_at    timestamptz NOT NULL,
    PRIMARY KEY (task, source, ref)
);
CREATE INDEX forecast_scores_lens ON forecast_scores (tenant, lens, scored_at);

-- What a candidate links an incident to (attribution.v2): an executed change, a proposal the
-- commission rejected (GOV-38) or a script run. The column and its script_run part come with
-- 0060_script_links; this closes the list of kinds, and only a script link has no proposal.
ALTER TABLE attributions ADD CONSTRAINT attributions_kind
    CHECK (kind IN ('change', 'rejected_proposal', 'script_run') AND (kind = 'script_run' OR proposal IS NOT NULL));

-- A bot returned to the head of the queue after a canceled link waits for no min_tenure (GOV-42).
ALTER TABLE promotion_entries ADD COLUMN skip_tenure boolean NOT NULL DEFAULT false;

-- A holder that answers for a confirmed link leaves its seat at most once per tenant in 30 days;
-- the other cases wait in order (GOV-42).
CREATE TABLE link_departures (
    id           bigserial PRIMARY KEY,
    tenant       text NOT NULL,
    attribution  text NOT NULL,
    seat         text NOT NULL,
    bot          text NOT NULL,
    state        text NOT NULL CHECK (state IN ('waiting', 'done', 'skipped', 'returned', 'canceled')),
    reason       text,
    created_at   timestamptz NOT NULL,
    done_at      timestamptz,
    updated_at   timestamptz NOT NULL,
    UNIQUE (attribution, seat)
);
CREATE INDEX link_departures_tenant ON link_departures (tenant, state, created_at);
