-- SPDX-License-Identifier: AGPL-3.0-only
-- Observation series (OBV-1 .. OBV-7): points that only code writes, rollups that code
-- computes, and the read cursors of the observe loop.

-- A point of a series: a number or a value of a closed list, never text. The key makes the
-- writing idempotent, so the same records give the same points (OBV-5). worker and role are
-- the producer of the measured work, for the self influence of a proposal (PRP-22).
CREATE TABLE obs_points (
    tenant      text NOT NULL,
    series      text NOT NULL,
    subject     text NOT NULL,
    at          timestamptz NOT NULL,
    num         double precision,
    enum        text,
    source      text NOT NULL CHECK (source IN ('core', 'tool', 'job')),
    data_mark   text NOT NULL,
    worker      text,
    role        text,
    key         text NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, series, key),
    CHECK ((num IS NULL) <> (enum IS NULL))
);
CREATE INDEX obs_points_read ON obs_points (tenant, series, subject, at);
CREATE INDEX obs_points_at ON obs_points (at);

-- A rollup of one period of a series (hour or day), kept longer than the points (OBV-4).
CREATE TABLE obs_rollups (
    tenant       text NOT NULL,
    series       text NOT NULL,
    subject      text NOT NULL,
    rollup       text NOT NULL,
    period_start timestamptz NOT NULL,
    value        double precision NOT NULL,
    samples      int NOT NULL,
    data_mark    text NOT NULL,
    updated_at   timestamptz NOT NULL,
    PRIMARY KEY (tenant, series, subject, rollup, period_start)
);
CREATE INDEX obs_rollups_at ON obs_rollups (period_start);

-- Where the observe loop read up to, by source of records.
CREATE TABLE obs_cursor (
    name  text PRIMARY KEY,
    at    timestamptz NOT NULL,
    -- After a full batch the cursor is a keyset position (at, last_id) read strictly after;
    -- otherwise the next pass reads again from at minus the look-back.
    last_id text NOT NULL DEFAULT '',
    full_batch boolean NOT NULL DEFAULT false
);

-- The last good backup by tenant and catalog resource, from run.finished.v1 of a backup type
-- (OBV-6, OBV-7).
CREATE TABLE obs_backups (
    tenant      text NOT NULL,
    subject     text NOT NULL,
    last_good   timestamptz NOT NULL,
    PRIMARY KEY (tenant, subject)
);

CREATE INDEX events_observed ON events (time) WHERE type IN ('run.finished.v1', 'schedule.skipped');
CREATE INDEX tasks_ended ON tasks (updated_at) WHERE state IN ('completed', 'failed', 'canceled');
CREATE INDEX leases_closed ON leases (closed_at) WHERE kind = 'attempt' AND closed_at IS NOT NULL;
