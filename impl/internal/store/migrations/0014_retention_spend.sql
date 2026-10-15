-- SPDX-License-Identifier: AGPL-3.0-only
-- Retention (RET-2), reconciliation with the provider report (BUD-10) and the core drain
-- (LCY-6).

-- A usage line of a model call names its provider, so a reconciliation finds the lines of a
-- workspace (tenant, provider). A correction line has unit 'correction', eur_micros 0 and the
-- signed difference in correction_eur_micros. An estimated line counts as zero in the limits
-- once reconciled or released by time.
ALTER TABLE spend ADD COLUMN provider text;
ALTER TABLE spend ADD COLUMN correction_eur_micros bigint;
ALTER TABLE spend ADD COLUMN reconciled_at timestamptz;
ALTER TABLE spend ADD COLUMN released_at timestamptz;
CREATE INDEX spend_estimated_open ON spend (time) WHERE state = 'estimated' AND reconciled_at IS NULL AND released_at IS NULL;

-- One row per reconciled workspace day; a second request for the day replaces it.
CREATE TABLE spend_reconciliations (
    tenant              text NOT NULL,
    provider            text NOT NULL,
    day                 date NOT NULL,
    provider_micros     bigint NOT NULL,
    farm_micros         bigint NOT NULL,
    correction_micros   bigint NOT NULL,
    report_id           text,
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant, provider, day)
);

-- The core drain (LCY-6): while drain_until is set, claim answers not_ready. A core start
-- clears it.
ALTER TABLE farm_meta ADD COLUMN drain_until timestamptz;

-- The workspaces with usage lines and their first line, so the budget loop reads no usage
-- lines to find them.
CREATE TABLE spend_workspaces (
    tenant      text NOT NULL,
    provider    text NOT NULL,
    first_at    timestamptz NOT NULL,
    PRIMARY KEY (tenant, provider)
);
INSERT INTO spend_workspaces (tenant, provider, first_at)
    SELECT tenant, lower(provider), min(time) FROM spend WHERE provider IS NOT NULL GROUP BY tenant, lower(provider);

-- The retention loop finds content past its period by these.
CREATE INDEX tasks_content_age ON tasks (updated_at) WHERE body IS NOT NULL OR result IS NOT NULL;
CREATE INDEX deliveries_content_age ON deliveries (updated_at) WHERE body IS NOT NULL;
CREATE INDEX toolcalls_content_age ON toolcalls (updated_at) WHERE args <> '{}' OR output IS NOT NULL;
CREATE INDEX sessions_age ON sessions (updated_at);
CREATE INDEX files_stored_expiry ON files (expires_at) WHERE state = 'stored';
