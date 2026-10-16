-- SPDX-License-Identifier: AGPL-3.0-only
-- Script links (TRN-7, HLD-9): an attribution candidate of kind script_run names a script run
-- instead of a proposal (spec/model/attribution.v2.schema.json); a run keeps the targets fixed
-- at "Run" and when it started and ended, for the rule script_target.
ALTER TABLE attributions
    ADD COLUMN kind text NOT NULL DEFAULT 'change',
    ADD COLUMN script_run text,
    ALTER COLUMN proposal DROP NOT NULL,
    ADD CONSTRAINT attributions_script_run CHECK ((kind = 'script_run') = (script_run IS NOT NULL) AND (script_run IS NULL OR proposal IS NULL));
CREATE UNIQUE INDEX attributions_script_link ON attributions (incident, script_run) WHERE script_run IS NOT NULL;

ALTER TABLE script_runs
    ADD COLUMN targets text[],
    ADD COLUMN started_at timestamptz,
    ADD COLUMN ended_at timestamptz;
CREATE INDEX script_runs_started ON script_runs (tenant, started_at) WHERE started_at IS NOT NULL;
