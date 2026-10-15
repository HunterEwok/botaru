-- SPDX-License-Identifier: AGPL-3.0-only
-- Monitoring problems and scripts into the farm (MON-5, MON-10).

-- One row per accepted adapter event: the idempotency key system:ingress:monitor:<eventid>:<state>
-- or of a script list.
CREATE TABLE monitor_events (
    tenant text NOT NULL,
    key text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant, key)
);

-- Problems on catalog hosts: the alarm they hold and their state. An alarm resolves when its last
-- open problem on the host closes.
CREATE TABLE monitor_problems (
    tenant text NOT NULL,
    eventid text NOT NULL,
    host text NOT NULL,
    triggerid text NOT NULL,
    class text NOT NULL,
    alarm text NOT NULL,
    state text NOT NULL CHECK (state IN ('open', 'resolved')),
    opened_at timestamptz NOT NULL,
    resolved_at timestamptz,
    PRIMARY KEY (tenant, eventid)
);
CREATE INDEX monitor_problems_open ON monitor_problems (tenant, host, opened_at) WHERE state = 'open';

-- The latest script list of a catalog host from the adapter: scriptid, input kind, choices.
CREATE TABLE monitor_host_scripts (
    tenant text NOT NULL,
    host text NOT NULL,
    hash text NOT NULL,
    scripts jsonb NOT NULL,
    updated_at timestamptz NOT NULL,
    PRIMARY KEY (tenant, host)
);

-- A run of a monitoring script: the write call the model waits for and the monitor_script
-- delivery the adapter runs it by.
CREATE TABLE monitor_script_calls (
    delivery text PRIMARY KEY,
    toolcall text NOT NULL UNIQUE,
    tenant text NOT NULL,
    task text NOT NULL,
    host text NOT NULL,
    script text NOT NULL,
    bot_id text NOT NULL,
    created_at timestamptz NOT NULL
);
