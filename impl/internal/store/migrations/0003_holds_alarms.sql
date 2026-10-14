-- SPDX-License-Identifier: AGPL-3.0-only
-- Holds (spec/lifecycles/hold.md, HLD-1) and alarm episodes (spec/lifecycles/alarm.md, OBS-6).

-- A hold is a record of the tenant of its scope, of farm for the scopes farm and seat (TEN-1).
-- The scope, effects, reason and author never change; only state, until and the end do.
CREATE TABLE holds (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    scope_kind    text NOT NULL,
    scope_tenant  text NOT NULL DEFAULT '',   -- '' for the scope farm
    scope_name    text NOT NULL DEFAULT '',   -- '' for the scopes farm and tenant
    effects       text[] NOT NULL,
    reason        text NOT NULL,
    until         timestamptz,                -- NULL: until released
    author        text NOT NULL,
    author_kind   text NOT NULL CHECK (author_kind IN ('operator', 'tenant_admin', 'seat_holder', 'contain_auto')),
    source        text,
    seat_epoch    bigint,
    state         text NOT NULL CHECK (state IN ('active', 'expired', 'released')),
    created_at    timestamptz NOT NULL,
    released_by   text,
    ended_at      timestamptz
);
CREATE INDEX holds_active ON holds (scope_kind, scope_tenant, scope_name) WHERE state = 'active';
CREATE INDEX holds_tenant ON holds (tenant, created_at);

-- One row per alarm episode; at most one open episode per tenant and alarmkey (OBS-6).
CREATE TABLE alarm_episodes (
    id            bigserial PRIMARY KEY,
    tenant        text NOT NULL,
    alarmkey      text NOT NULL,
    alarm         text NOT NULL,              -- the event type alarm.<name>.vN
    state         text NOT NULL CHECK (state IN ('firing', 'resolved')),
    severity      text NOT NULL CHECK (severity IN ('info', 'warn', 'page')),
    subject       jsonb NOT NULL,
    vals          jsonb NOT NULL DEFAULT '{}',
    seq           int NOT NULL CHECK (seq BETWEEN 1 AND 2),
    repeat_count  int NOT NULL DEFAULT 0,
    opened_at     timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    resolved_at   timestamptz,
    notified_at   timestamptz                 -- the last alarm_notice of the episode (MON-13)
);
CREATE UNIQUE INDEX alarm_episodes_open ON alarm_episodes (tenant, alarmkey) WHERE state = 'firing';

-- The worker-liveness loop writes worker.stale once per silence (OBS-4).
ALTER TABLE workers ADD COLUMN stale_since timestamptz;

-- An alarm_notice delivery names its episode: it does not die by attempt count while the
-- episode is open, and the notices of one alarmkey go in order (MON-13).
ALTER TABLE deliveries ADD COLUMN alarmkey text;
ALTER TABLE deliveries ADD COLUMN episode bigint;

-- start_by of the type, written at enqueue; a task never claimed by then raises
-- alarm.task.unclaimed once (DSP-8), and the flag stays while that episode is open.
ALTER TABLE tasks ADD COLUMN start_by_at timestamptz;
ALTER TABLE tasks ADD COLUMN unclaimed_alarm boolean NOT NULL DEFAULT false;
CREATE INDEX tasks_start_by ON tasks (start_by_at) WHERE state = 'submitted' AND lease_no = 0 AND NOT unclaimed_alarm;
