-- SPDX-License-Identifier: AGPL-3.0-only
-- The reply of a code command that changes state without a task (/grant, /ungrant), by the
-- key of its activity: a retried update gets the same reply and changes nothing again (DLV-5,
-- IDM-3).
CREATE TABLE activity_outcomes (
    tenant     text NOT NULL,
    key        text NOT NULL,
    reply      jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant, key)
);
