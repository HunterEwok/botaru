-- SPDX-License-Identifier: AGPL-3.0-only
-- Indexes for queries that per-second loops and every enqueue run. Task rows are never deleted,
-- so without them the deadline loop and the queue ceiling read every task the store holds, and
-- the observe and stall loops read every spend row for each task they look at.
CREATE INDEX spend_task ON spend (task) WHERE task IS NOT NULL;
CREATE INDEX tasks_live_deadline ON tasks (deadline_at) WHERE state IN ('submitted', 'working');
CREATE INDEX tasks_live_tenant ON tasks (tenant) WHERE state IN ('submitted', 'working');
CREATE INDEX tasks_tenant_created ON tasks (tenant, created_at);
