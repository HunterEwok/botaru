-- SPDX-License-Identifier: AGPL-3.0-only
-- The daily work of the farm manager in a tenant (MGR-41): one row per tenant and local day once
-- the core enqueued the review manager.review.v1 and sent the numbers of the day, so a second
-- copy of the loop or a restart does neither twice.
CREATE TABLE manager_days (
    tenant      text NOT NULL,
    day         date NOT NULL,
    review      text,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, day)
);
