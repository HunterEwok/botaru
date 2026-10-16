-- SPDX-License-Identifier: AGPL-3.0-only
-- The newest account report of each monitoring adapter worker of a tenant (MON-4, OBS-11,
-- spec/model/event-data/ext-monitor-account.schema.json): the role rights wider than the rule
-- allows and the hashes of the hosts its user sees and may run scripts on. A revoked worker's
-- row and a deleted tenant's rows are removed.
CREATE TABLE monitor_accounts (
    tenant text NOT NULL,
    worker text NOT NULL,
    report jsonb NOT NULL,
    checked_at timestamptz NOT NULL,
    PRIMARY KEY (tenant, worker)
);
CREATE INDEX monitor_accounts_worker ON monitor_accounts (worker);
