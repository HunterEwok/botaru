-- SPDX-License-Identifier: AGPL-3.0-only
-- The last health report of a channel about its external system, with the time of its last
-- successful poll (OBS-10, spec/model/health-report.schema.json: component channel_upstream).
ALTER TABLE workers ADD COLUMN upstream jsonb;

-- An agent runtime that keeps sessions or memory itself is searched by a match step (RT-5).
ALTER TABLE subject_match_steps DROP CONSTRAINT subject_match_steps_kind_check;
ALTER TABLE subject_match_steps ADD CONSTRAINT subject_match_steps_kind_check CHECK (kind IN ('channel', 'sink', 'monitor', 'runtime'));
