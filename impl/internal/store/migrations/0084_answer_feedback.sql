-- SPDX-License-Identifier: AGPL-3.0-only
-- The optional "Useful" / "Not needed" card under a bot's answer to a person (INS-7) shares the
-- feedback records of the manager's notices.
ALTER TABLE manager_feedback DROP CONSTRAINT manager_feedback_kind_check;
ALTER TABLE manager_feedback ADD CONSTRAINT manager_feedback_kind_check CHECK (kind IN ('manager_attention', 'postmortem_report', 'task_answer'));
CREATE INDEX manager_feedback_task ON manager_feedback (tenant, kind, created_at) WHERE task IS NOT NULL;
