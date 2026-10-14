-- SPDX-License-Identifier: AGPL-3.0-only
-- The state of each pair (tenant workspace, model pool) as P4 sees it (GW-10, GW-11, GW-13):
-- open is the circuit after a series of failures, probing is one probe call after the pause,
-- rate is a wait for the provider rate limit, config waits for a fix by apply. No row is a
-- closed circuit.

CREATE TABLE provider_pairs (
    tenant      text NOT NULL,
    pool        text NOT NULL,
    state       text NOT NULL CHECK (state IN ('closed', 'open', 'probing', 'rate', 'config')),
    failures    int NOT NULL DEFAULT 0,
    pause_s     int NOT NULL DEFAULT 0,
    until       timestamptz,
    updated_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, pool)
);

-- The 80% threshold of a limit gives one event per period (BUD-4).
CREATE TABLE budget_thresholds (
    tenant      text NOT NULL,
    scope       text NOT NULL,
    period      timestamptz NOT NULL,
    PRIMARY KEY (tenant, scope, period)
);
