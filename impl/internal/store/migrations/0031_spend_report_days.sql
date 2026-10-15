-- SPDX-License-Identifier: AGPL-3.0-only
-- A provider's report of a day, received even when it has no usage: the spend gap after a
-- restore closes on it, so an idle provider day does not keep the gap open (RET-6, BUD-10).
CREATE TABLE spend_report_days (
    provider    text NOT NULL,
    day         date NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (provider, day)
);
INSERT INTO spend_report_days (provider, day, created_at)
    SELECT provider, day, max(created_at) FROM spend_reconciliations GROUP BY provider, day;
