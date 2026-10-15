-- SPDX-License-Identifier: AGPL-3.0-only
-- Bot messages (spec/lifecycles/message.md): one row per inbox entry, the copies of an all
-- delivery being entries of their own (MSG-14). seq is the number the core assigns, the order of
-- an inbox. envelope holds the core fields and the closed fields, never the note: the note lives
-- only in its own column of retention class content and is cleared when the entry ends
-- (MSG-3, MSG-13). An entry staged by an attempt names it by sender_task and sender_lease; an
-- entry handed to a reading attempt names it by reader_task and reader_lease and carries the
-- opaque handle of that hand-out (MSG-9).
CREATE TABLE messages (
    seq           bigserial PRIMARY KEY,
    id            text NOT NULL,
    tenant        text NOT NULL,
    inbox_kind    text NOT NULL CHECK (inbox_kind IN ('role', 'group', 'worker')),
    inbox_name    text NOT NULL,
    type          text NOT NULL,
    kind          text NOT NULL CHECK (kind IN ('report', 'state')),
    delivery      text NOT NULL CHECK (delivery IN ('one', 'all', 'state')),
    state         text NOT NULL CHECK (state IN ('staged', 'queued', 'leased', 'acked', 'discarded', 'dead', 'expired',
                                                 'current', 'stale', 'replaced', 'removed')),
    reason        text,
    envelope      jsonb NOT NULL,
    note          text,
    from_role     text NOT NULL,
    from_worker   text,
    sender_task   text,
    sender_lease  int,
    idem_key      text NOT NULL,
    data_mark     text NOT NULL,
    untrusted     boolean NOT NULL,
    hops          int NOT NULL,
    handouts      int NOT NULL DEFAULT 0,
    reader_task   text,
    reader_lease  int,
    handle        text UNIQUE,
    nacked        boolean NOT NULL DEFAULT false,
    expires_at    timestamptz,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL,
    UNIQUE (tenant, idem_key, inbox_kind, inbox_name)
);
CREATE INDEX messages_inbox ON messages (tenant, inbox_kind, inbox_name, seq) WHERE state IN ('staged', 'queued', 'leased', 'current', 'stale');
CREATE INDEX messages_sender ON messages (sender_task, sender_lease) WHERE state = 'staged';
CREATE INDEX messages_reader ON messages (reader_task, reader_lease) WHERE state = 'leased';
CREATE INDEX messages_rate ON messages (tenant, from_role, type, created_at);
CREATE INDEX messages_expiry ON messages (expires_at) WHERE state IN ('queued', 'current');
-- One live state value per (inbox, sending worker, type) (MSG-14).
CREATE UNIQUE INDEX messages_state_value ON messages (tenant, inbox_kind, inbox_name, from_worker, type) WHERE kind = 'state' AND state IN ('current', 'stale');
-- Ended entries leave the store after the period of class content (RET-2, MSG-12).
CREATE INDEX messages_ended ON messages (updated_at) WHERE state NOT IN ('staged', 'queued', 'leased', 'current');

-- One event per period: a refusal by rate, fullness or sender share raises its event once per
-- (key, period) (MSG-15).
CREATE TABLE message_marks (
    tenant  text NOT NULL,
    key     text NOT NULL,
    until   timestamptz NOT NULL,
    PRIMARY KEY (tenant, key)
);
