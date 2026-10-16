-- SPDX-License-Identifier: AGPL-3.0-only
-- The drain deadline of a worker (LCY-4). The erasure step kind runtime of the runtime profile
-- store that a worker revoke clears (LCY-4, RT-1, RT-2) is in 0044_erasure_edges.
ALTER TABLE workers ADD COLUMN drain_deadline timestamptz;
