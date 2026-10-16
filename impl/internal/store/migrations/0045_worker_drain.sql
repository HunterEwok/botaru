-- SPDX-License-Identifier: AGPL-3.0-only
-- The drain deadline of a worker (LCY-4) and the erasure step of a runtime profile store that
-- a worker revoke clears (LCY-4, RT-1, RT-2).
ALTER TABLE workers ADD COLUMN drain_deadline timestamptz;
ALTER TABLE erasure_steps DROP CONSTRAINT erasure_steps_kind_check;
ALTER TABLE erasure_steps ADD CONSTRAINT erasure_steps_kind_check CHECK (kind IN ('core', 'channel', 'sink', 'monitor', 'runtime'));
