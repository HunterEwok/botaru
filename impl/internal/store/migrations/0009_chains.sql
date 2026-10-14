-- SPDX-License-Identifier: AGPL-3.0-only
-- Task trees and continuations (PRM-6, CNT-1..CNT-5) and the stall of an attempt (CNT-4).

-- The tasks of a tree and the children of a task are counted at enqueue.
CREATE INDEX tasks_root ON tasks (tenant, (record->>'root'));
CREATE INDEX tasks_parent ON tasks (tenant, (record->>'parent'));
-- The open follow-up window of a conversation (CNT-5).
CREATE INDEX tasks_followup ON tasks (tenant, initiator, (record->>'conversation')) WHERE record ? 'followup_until';

-- The last progress an attempt reported in heartbeat and when it changed (CNT-4).
ALTER TABLE leases ADD COLUMN progress double precision;
ALTER TABLE leases ADD COLUMN progress_at timestamptz;
-- When a call's reserve was settled: a finished model call is progress too (CNT-4).
ALTER TABLE spend ADD COLUMN settled_at timestamptz;
