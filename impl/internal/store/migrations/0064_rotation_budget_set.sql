-- SPDX-License-Identifier: AGPL-3.0-only
-- Worker token rotation (IDN-4) and the temporary narrowing of a limit by P2 budget.set (MAN-5).

-- After worker.rotate the previous token stays valid until prev_valid_until (IDN-4).
ALTER TABLE workers ADD COLUMN prev_token_hash text UNIQUE;
ALTER TABLE workers ADD COLUMN prev_valid_until timestamptz;

-- A temporary lower limit of a tenant window or budget line, always with an expiry; the limits
-- themselves live only in the tenant manifest (MAN-5). One row per (tenant, line).
CREATE TABLE budget_overrides (
    tenant      text NOT NULL,
    line        text NOT NULL,
    limit_cents bigint NOT NULL CHECK (limit_cents >= 0),
    until       timestamptz NOT NULL,
    set_by      text NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, line)
);
