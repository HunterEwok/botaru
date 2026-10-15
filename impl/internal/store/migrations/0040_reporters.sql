-- SPDX-License-Identifier: AGPL-3.0-only
-- The snapshot of the status views of a reporter task: the model and the digest table read the
-- same numbers (REP-2).
CREATE TABLE view_snapshots (
    task text PRIMARY KEY,
    body jsonb NOT NULL,
    created_at timestamptz NOT NULL
);
CREATE INDEX view_snapshots_created ON view_snapshots (created_at);
-- How often code repeats the notices to a non-principal in a bound chat (MSG-17).
ALTER TABLE chat_bindings ADD COLUMN notice_repeat text;
