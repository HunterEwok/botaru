-- SPDX-License-Identifier: AGPL-3.0-only
-- Tool calls (spec/lifecycles/toolcall.md, schema toolcall): a read call of an attempt made
-- through P3 tools/call and taken by a toolhost through P1 claim(toolcall).

CREATE TABLE toolcalls (
    id                text PRIMARY KEY,
    tenant            text NOT NULL,
    task              text NOT NULL,
    attempt_lease_no  int,                   -- the lease of the attempt that made a read call
    tool              text NOT NULL,         -- <plugin>/<tool>
    access            text NOT NULL CHECK (access IN ('read', 'write')),
    args              jsonb NOT NULL,
    args_hash         text,
    approval_id       text,
    grant_id          text,
    target_plugin     text NOT NULL,
    target_placement  text NOT NULL,
    target_host       text,                  -- NULL for a tool with target: any
    target_resource   text,                  -- the catalog resource of the target value, if any
    resolved          jsonb NOT NULL DEFAULT '{}',
    untrusted_output  boolean NOT NULL,
    state             text NOT NULL CHECK (state IN ('requested', 'denied', 'queued', 'leased', 'done', 'error', 'exec_unknown', 'abandoned')),
    reason            text,
    start_by          timestamptz,
    lease_no          int NOT NULL DEFAULT 0,
    worker_id         text,
    output            jsonb,
    created_at        timestamptz NOT NULL,
    updated_at        timestamptz NOT NULL,
    ended_at          timestamptz
);
CREATE INDEX toolcalls_queued ON toolcalls (tenant, target_plugin, created_at) WHERE state = 'queued';
CREATE INDEX toolcalls_open ON toolcalls (task, attempt_lease_no) WHERE state IN ('queued', 'leased');
CREATE INDEX toolcalls_task ON toolcalls (task, created_at);
