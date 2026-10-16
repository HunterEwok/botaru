-- SPDX-License-Identifier: AGPL-3.0-only
-- The monthly audit sample of the commission (DIG-5): one draw per tenant and month, with the
-- seed and the drawn proposal ids recorded at the time of the choice, the population T and the
-- size N.
CREATE TABLE audit_samples (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    month       text NOT NULL,
    seed        text NOT NULL,
    population  int NOT NULL,
    size        int NOT NULL,
    drawn       jsonb NOT NULL,
    created_at  timestamptz NOT NULL,
    UNIQUE (tenant, month)
);
