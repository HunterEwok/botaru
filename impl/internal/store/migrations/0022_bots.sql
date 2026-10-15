-- SPDX-License-Identifier: AGPL-3.0-only
-- Bot identities and commission seats (IDN-8..IDN-12, GOV-33): an identity with its binding to
-- a worker or a seat and the binding epoch; a seat with its holder and the seat epoch. Every
-- attempt lease, usage line and fact carries the acting identity and its epoch.
CREATE TABLE bots (
    id           text PRIMARY KEY,
    tenant       text NOT NULL,
    role         text NOT NULL,
    state        text NOT NULL CHECK (state IN ('active', 'suspended', 'unbound', 'retired', 'erased')),
    mode         text NOT NULL CHECK (mode IN ('worker', 'seat')),
    worker_id    text,
    seat         text,
    epoch        int NOT NULL CHECK (epoch >= 1),
    record       jsonb NOT NULL DEFAULT '{}',
    retired      jsonb,                     -- reason, at, successor; kept in the tombstone
    unbound_at   timestamptz,
    sealed_until timestamptz,
    created_at   timestamptz NOT NULL,
    updated_at   timestamptz NOT NULL
);
-- One identity holds at most one worker, and a worker has one identity at any moment.
CREATE UNIQUE INDEX bots_one_per_worker ON bots (worker_id) WHERE worker_id IS NOT NULL AND state IN ('active', 'suspended');
CREATE INDEX bots_tenant ON bots (tenant, state);

CREATE TABLE seats (
    tenant       text NOT NULL,
    id           text NOT NULL,
    kind         text NOT NULL CHECK (kind IN ('commission', 'farm_manager')),
    lens         text,
    holder       text,
    epoch        int NOT NULL DEFAULT 0,
    state        text NOT NULL CHECK (state IN ('vacant', 'held', 'stopped', 'removed')),
    stop_reasons jsonb NOT NULL DEFAULT '[]',
    decide_until timestamptz,
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant, id)
);

ALTER TABLE leases ADD COLUMN acting_bot text;
ALTER TABLE leases ADD COLUMN bot_epoch int;
ALTER TABLE leases ADD COLUMN seat text;
ALTER TABLE leases ADD COLUMN seat_epoch int;
CREATE INDEX leases_acting_bot ON leases (acting_bot) WHERE state = 'active' AND acting_bot IS NOT NULL;
ALTER TABLE spend ADD COLUMN acting_bot text;
ALTER TABLE spend ADD COLUMN bot_epoch int;
