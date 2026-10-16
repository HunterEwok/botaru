-- SPDX-License-Identifier: AGPL-3.0-only
-- From v0.3 an operator of farm marks the notice manager_preventable of a restriction read with
-- P2 incident.ack on its incident; the repeats then stop while the restriction stays (GOV-47).
ALTER TABLE manager_restrictions ADD COLUMN read_at timestamptz, ADD COLUMN read_by text;
