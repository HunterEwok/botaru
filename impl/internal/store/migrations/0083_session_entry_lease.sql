-- SPDX-License-Identifier: AGPL-3.0-only
-- The attempt that appended a session entry, so that session.load leaves out what an attempt
-- appended whose result the core did not accept (MEM-11). Older entries have none and stay visible.
ALTER TABLE session_entries ADD COLUMN task text, ADD COLUMN lease_no int;
