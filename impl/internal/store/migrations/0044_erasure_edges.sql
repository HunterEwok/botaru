-- SPDX-License-Identifier: AGPL-3.0-only
-- Erase work for every edge store with content (RET-11): a channel with a cache or saved
-- attachments matches and erases, and every agent runtime that ran an attempt of the tenant
-- purges by the scope. The erasure of an erase_match records the ids each store found, so a
-- replay after a restore erases by these ids and never searches again (RET-6, RET-11).
ALTER TABLE erasure_steps DROP CONSTRAINT erasure_steps_kind_check;
ALTER TABLE erasure_steps ADD CONSTRAINT erasure_steps_kind_check CHECK (kind IN ('core', 'channel', 'sink', 'monitor', 'runtime'));
ALTER TABLE subject_match_steps DROP CONSTRAINT subject_match_steps_kind_check;
ALTER TABLE subject_match_steps ADD CONSTRAINT subject_match_steps_kind_check CHECK (kind IN ('channel', 'sink', 'monitor'));
-- found: the ids per store key of an erase_match, with an empty list for a store whose match
-- found nothing; a store that could not be searched is absent.
ALTER TABLE erasure_requests ADD COLUMN found jsonb;
CREATE INDEX leases_worker_kind ON leases (worker_id, kind);
