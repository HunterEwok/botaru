-- SPDX-License-Identifier: AGPL-3.0-only
-- The newest account report of each tenant's monitoring adapter (MON-4, OBS-11,
-- spec/model/event-data/ext-monitor-account.schema.json): the role rights wider than the rule
-- allows and the hashes of the hosts its user sees and may run scripts on.
CREATE TABLE monitor_accounts (
    tenant text PRIMARY KEY,
    report jsonb NOT NULL,
    checked_at timestamptz NOT NULL
);
