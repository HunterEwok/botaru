-- SPDX-License-Identifier: AGPL-3.0-only
-- Incidents (spec/lifecycles/incident.md, schema incident): only the core opens one, from a code
-- alarm episode and a catalog class (MGR-7, MGR-8). An incident keeps the episode that opened or
-- reopened it, so one episode opens at most one incident of a class.
CREATE TABLE incidents (
    id             text PRIMARY KEY,
    tenant         text NOT NULL,
    class          text NOT NULL,
    subject_kind   text NOT NULL,
    subject_id     text NOT NULL DEFAULT '',
    alarmkey       text NOT NULL,
    episode        bigint NOT NULL,
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
CREATE UNIQUE INDEX incidents_episode ON incidents (class, episode);
CREATE UNIQUE INDEX incidents_live ON incidents (tenant, class, subject_kind, subject_id)
    WHERE state IN ('open', 'contained', 'mitigating', 'reopened');
CREATE INDEX incidents_tenant ON incidents (tenant, opened_at);
