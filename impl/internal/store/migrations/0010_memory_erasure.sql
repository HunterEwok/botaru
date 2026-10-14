-- SPDX-License-Identifier: AGPL-3.0-only
-- Conversation sessions (C3) and person memory (C4) in the farm store (MEM-1..MEM-4), the
-- erasure log with a step per store (RET-4, RET-7) and a person's requests (SEC-11).

-- The session key is (tenant, role, subject, conversation): subjects is the set of principals
-- who wrote in the conversation, extended at the first activity of each (MEM-1).
CREATE TABLE sessions (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    role            text NOT NULL,
    conversation    text NOT NULL,
    subjects        text[] NOT NULL DEFAULT '{}',
    untrusted_seen  boolean NOT NULL DEFAULT false,   -- only a new conversation or forget clears it (INJ-13)
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (tenant, role, conversation)
);
CREATE INDEX sessions_subjects ON sessions USING gin (subjects);

-- content: deleted with its session.
CREATE TABLE session_entries (
    session     text NOT NULL REFERENCES sessions (id) ON DELETE CASCADE,
    uuid        text NOT NULL,
    seq         bigserial NOT NULL,
    summary     boolean NOT NULL DEFAULT false,
    content     jsonb NOT NULL,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (session, uuid)
);
CREATE INDEX session_entries_seq ON session_entries (session, seq);

-- memory: a person's records; active only after their decision (MEM-3).
CREATE TABLE memory_items (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    scope           text NOT NULL CHECK (scope IN ('person')),
    subject         text NOT NULL,
    role            text NOT NULL,
    key             text NOT NULL,
    content         text NOT NULL,
    content_hash    text NOT NULL,
    state           text NOT NULL CHECK (state IN ('proposed', 'active', 'discarded', 'superseded', 'forgotten')),
    personal_seen   boolean NOT NULL DEFAULT false,
    nonce           text NOT NULL,
    source_task     text,
    created_at      timestamptz NOT NULL,
    decide_by       timestamptz,
    decided_at      timestamptz,
    updated_at      timestamptz NOT NULL
);
CREATE UNIQUE INDEX memory_items_same ON memory_items (tenant, subject, role, content_hash) WHERE state IN ('proposed', 'active');
CREATE INDEX memory_items_space ON memory_items (tenant, subject, role) WHERE state = 'active';
CREATE INDEX memory_items_expiry ON memory_items (decide_by) WHERE state = 'proposed';

-- A session or memory handle names the object it opens.
ALTER TABLE handles ADD COLUMN ref text;

-- The erasure log (RET-7): the scope as recorded, the deadline, and the needles an edge store
-- matches by, kept only until the request closes (RET-4).
ALTER TABLE erasure_requests ADD COLUMN scope jsonb;
ALTER TABLE erasure_requests ADD COLUMN deadline_at timestamptz;
ALTER TABLE erasure_requests ADD COLUMN needles text[];
ALTER TABLE erasure_requests ADD COLUMN updated_at timestamptz;
ALTER TABLE erasure_requests ADD COLUMN closed_at timestamptz;
ALTER TABLE erasure_requests ADD COLUMN overdue_at timestamptz;   -- when its overdue alarm fired
UPDATE erasure_requests SET scope = jsonb_build_object('tenant', tenant, 'subject', subject) WHERE scope IS NULL AND subject IS NOT NULL;
UPDATE erasure_requests SET scope = jsonb_build_object('tenant', tenant) WHERE scope IS NULL;
UPDATE erasure_requests SET deadline_at = created_at + interval '30 days', updated_at = created_at WHERE deadline_at IS NULL;

-- One step per store of a request: a core store is erased by the loop itself, an edge store
-- (a sink or a monitoring adapter) gets an erasure work item through claim.
CREATE TABLE erasure_steps (
    id          text PRIMARY KEY,
    request     text NOT NULL REFERENCES erasure_requests (id),
    tenant      text NOT NULL,
    store       text NOT NULL,           -- the key of the store in the request results
    kind        text NOT NULL CHECK (kind IN ('core', 'channel', 'sink', 'monitor')),
    target      text NOT NULL,           -- the core store, or the channel, sink or adapter name
    state       text NOT NULL CHECK (state IN ('pending', 'leased', 'done', 'failed', 'not_applicable')),
    basis       text,
    reason      text,
    lease_no    int NOT NULL DEFAULT 0,
    run_at      timestamptz,
    updated_at  timestamptz NOT NULL,
    UNIQUE (request, store)
);
CREATE INDEX erasure_steps_queue ON erasure_steps (tenant, kind, target, run_at) WHERE state = 'pending';

-- A person's request about their own data (SEC-11); erase_match carries the found ids only.
CREATE TABLE subject_requests (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('erase', 'erase_match')),
    subject     text,                    -- the principal; none for a person who is not one
    author      text NOT NULL,
    state       text NOT NULL CHECK (state IN ('pending', 'approved', 'rejected', 'withdrawn')),
    nonce       text NOT NULL,
    found       jsonb,                   -- erase_match: the object ids found per store
    deadline_at timestamptz NOT NULL,
    reminded_at timestamptz,
    overdue_at  timestamptz,
    decided_by  text,
    decided_at  timestamptz,
    erasure     text,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE INDEX subject_requests_open ON subject_requests (tenant, deadline_at) WHERE state = 'pending';
