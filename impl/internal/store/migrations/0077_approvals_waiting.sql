-- SPDX-License-Identifier: AGPL-3.0-only
-- An approval waiting for a busy resource (state approved, reason resource_busy, ORC-12) is the
-- same request as a pending one: one write is asked for once (APR-2).
DROP INDEX approvals_same;
CREATE UNIQUE INDEX approvals_same ON approvals (tenant, action, args_hash, initiator, coalesce(source_task, ''))
    WHERE state = 'pending' OR state = 'approved' AND reason = 'resource_busy';
