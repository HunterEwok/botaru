-- SPDX-License-Identifier: AGPL-3.0-only
-- Incidents (spec/lifecycles/incident.md, schema incident): only the core opens one, from a code
-- alarm episode and a catalog class (MGR-7, MGR-8). An incident keeps the episode that opened or
-- reopened it as the current one, and every episode and alarm key it has adopted, so one episode
-- belongs to at most one incident of a class.
CREATE TABLE incidents (
    id             text PRIMARY KEY,
    tenant         text NOT NULL,
    class          text NOT NULL,
    subject_kind   text NOT NULL,
    subject_id     text NOT NULL DEFAULT '',
    alarmkey       text NOT NULL,            -- the alarm key that opened it (informational)
    episode        bigint NOT NULL,          -- the current episode
    episodes       bigint[] NOT NULL,        -- every episode the incident adopted
    alarmkeys      text[] NOT NULL,          -- every alarm key it adopted: one of them reopens it
    state          text NOT NULL CHECK (state IN ('open', 'contained', 'mitigating', 'resolved', 'reopened', 'closed', 'dismissed')),
    record         jsonb NOT NULL,           -- the other fields of the incident record
    since          timestamptz NOT NULL,     -- opening or the last reopening: the unhandled timer starts here
    handled        boolean NOT NULL DEFAULT false, -- an operation or a person's incident.ack since `since`
    unhandled      boolean NOT NULL DEFAULT false, -- alarm.incident.unhandled is firing for it
    implicated_at  timestamptz NOT NULL,
    opened_at      timestamptz NOT NULL,
    resolved_at    timestamptz,
    ended_at       timestamptz,
    updated_at     timestamptz NOT NULL
);
CREATE INDEX incidents_episodes ON incidents USING gin (episodes);
CREATE UNIQUE INDEX incidents_live ON incidents (tenant, class, subject_kind, subject_id)
    WHERE state IN ('open', 'contained', 'mitigating', 'reopened');
CREATE INDEX incidents_tenant ON incidents (tenant, opened_at);

-- When an episode's numbers last changed: a repeat carries the latest numbers.
ALTER TABLE alarm_episodes ADD COLUMN vals_at timestamptz;

-- Every change of a firing episode's numbers, so detect.for counts from when the conditions of a
-- class last started to hold without a break, even when the numbers change between loop passes
-- (MGR-8).
CREATE TABLE alarm_values (
    episode  bigint NOT NULL,
    at       timestamptz NOT NULL,
    vals     jsonb NOT NULL,
    PRIMARY KEY (episode, at)
);

-- An episode that already fires at the upgrade counts its numbers from the upgrade, never from
-- its opening: its old numbers were not kept up to date.
UPDATE alarm_episodes SET vals_at = now() WHERE state = 'firing';
INSERT INTO alarm_values (episode, at, vals) SELECT id, now(), vals FROM alarm_episodes WHERE state = 'firing';
