-- SPDX-License-Identifier: AGPL-3.0-only
-- The C1 farm store of the reference implementation: the first slice of the core profile
-- (tasks, leases, deliveries, events, audit, usage, handles, principals, workers, manifests).
-- Times are UTC (TIM-1); every tenant record has exactly one tenant (TEN-1).

CREATE TABLE farm_meta (
    id              int PRIMARY KEY CHECK (id = 1),
    epoch           bigint NOT NULL,                 -- store epoch (RET-6)
    test_bench      boolean NOT NULL,                -- written at the first start, never changes (APR-9)
    created_at      timestamptz NOT NULL DEFAULT clock_timestamp(),
    clock_offset_us bigint NOT NULL DEFAULT 0 CHECK (clock_offset_us >= 0)  -- test bench clock only
);

-- The store clock is the master clock (TIM-1). On a test bench the offset moves it forward.
CREATE FUNCTION botaru_now() RETURNS timestamptz LANGUAGE sql VOLATILE AS $$
    SELECT clock_timestamp() + make_interval(secs => (SELECT clock_offset_us FROM farm_meta WHERE id = 1) / 1e6)
$$;

CREATE TABLE tenants (
    name        text PRIMARY KEY,
    kind        text NOT NULL CHECK (kind IN ('production', 'test')),
    created_at  timestamptz NOT NULL,
    retired     boolean NOT NULL DEFAULT false
);

-- Applied manifests: the set of the last commit per tenant plus global objects (tenant '').
CREATE TABLE manifests (
    kind        text NOT NULL,
    tenant      text NOT NULL DEFAULT '',
    name        text NOT NULL,
    doc         jsonb NOT NULL,
    hash        text NOT NULL,
    commit      text NOT NULL,
    applied_at  timestamptz NOT NULL,
    PRIMARY KEY (kind, tenant, name)
);

CREATE TABLE apply_status (
    id          int PRIMARY KEY CHECK (id = 1),
    commit      text NOT NULL,
    set_hash    text NOT NULL,
    applied_at  timestamptz NOT NULL
);

CREATE TABLE principals (
    tenant      text NOT NULL,
    id          text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('human', 'service', 'system')),
    state       text NOT NULL CHECK (state IN ('active', 'off', 'offboarded')),
    locale      text,
    created_by  text,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, id)
);

CREATE TABLE identities (
    tenant      text NOT NULL,
    identity    text NOT NULL,
    principal   text NOT NULL,
    PRIMARY KEY (tenant, identity),
    FOREIGN KEY (tenant, principal) REFERENCES principals (tenant, id)
);
CREATE INDEX identities_principal ON identities (tenant, principal);

CREATE TABLE memberships (
    tenant      text NOT NULL,
    principal   text NOT NULL,
    grp         text NOT NULL,
    state       text NOT NULL CHECK (state IN ('active', 'pending')),
    until       timestamptz,
    created_at  timestamptz NOT NULL,
    PRIMARY KEY (tenant, principal, grp),
    FOREIGN KEY (tenant, principal) REFERENCES principals (tenant, id)
);

-- Workers and their tokens; tokens are stored only as keyed hashes (spec/hashing.md).
CREATE TABLE workers (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    role        text NOT NULL,
    kind        text NOT NULL,
    labels      jsonb NOT NULL DEFAULT '{}',
    plugins     jsonb NOT NULL DEFAULT '[]',
    placement   text,
    host        text,
    target      text,
    state       text NOT NULL CHECK (state IN ('enrolled', 'active', 'blocked', 'draining', 'revoked')),
    token_hash  text NOT NULL UNIQUE,
    hello       jsonb,
    last_seen   timestamptz,
    created_at  timestamptz NOT NULL
);

-- The P2 secret of a channel driver: one per driver in a tenant (D-87 (3)).
CREATE TABLE driver_secrets (
    tenant      text NOT NULL,
    driver      text NOT NULL,
    hash        text NOT NULL UNIQUE,
    created_at  timestamptz NOT NULL,
    valid_until timestamptz,
    PRIMARY KEY (tenant, driver, hash)
);

CREATE TABLE tasks (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    type            text NOT NULL,
    state           text NOT NULL CHECK (state IN ('submitted', 'working', 'completed', 'failed', 'canceled')),
    reason          text,
    initiator       text NOT NULL,
    priority_class  int NOT NULL CHECK (priority_class BETWEEN 1 AND 5),
    idempotency_key text NOT NULL,
    body_hash       text NOT NULL,
    body            jsonb,                  -- content: deleted by task.purge (RET-3)
    result          jsonb,                  -- content: deleted by task.purge
    record          jsonb NOT NULL,         -- the other fields of task-record written by the core
    lease_no        int NOT NULL DEFAULT 0,
    attempts_spent  int NOT NULL DEFAULT 0,
    runs_counted    int NOT NULL DEFAULT 0,
    run_at          timestamptz,
    deadline_at     timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL,
    UNIQUE (tenant, idempotency_key)        -- IDM-1; the row stays as a tombstone after a purge
);
CREATE INDEX tasks_queue ON tasks (tenant, priority_class, created_at) WHERE state = 'submitted';

-- One row per lease of a work item (FEN-1): an attempt (work_id = task id) or a delivery.
CREATE TABLE leases (
    work_id     text NOT NULL,
    lease_no    int NOT NULL CHECK (lease_no >= 1),
    kind        text NOT NULL CHECK (kind IN ('attempt', 'toolcall', 'delivery', 'erasure')),
    tenant      text NOT NULL,
    worker_id   text NOT NULL REFERENCES workers (id),
    epoch       bigint NOT NULL,
    state       text NOT NULL CHECK (state IN ('active', 'closed', 'released', 'expired', 'revoked', 'preempted')),
    lease_until timestamptz NOT NULL,
    reason      text,
    run_id      text,
    outcome     jsonb,                      -- the accepted outcome, so that a repeat gets the same answer (IDM-3)
    created_at  timestamptz NOT NULL,
    closed_at   timestamptz,
    PRIMARY KEY (work_id, lease_no)
);
CREATE UNIQUE INDEX leases_one_active ON leases (work_id) WHERE state = 'active';
CREATE INDEX leases_expiry ON leases (lease_until) WHERE state = 'active';

CREATE TABLE deliveries (
    id              text PRIMARY KEY,
    seq             bigserial NOT NULL UNIQUE,
    tenant          text NOT NULL,
    task            text,
    kind            text NOT NULL,
    addr            text NOT NULL,          -- the address from the closed list (DLV-6)
    target_kind     text NOT NULL CHECK (target_kind IN ('chat', 'subject', 'sink', 'monitor')),
    target_id       text NOT NULL,
    exec_kind       text NOT NULL CHECK (exec_kind IN ('channel', 'sink', 'monitor')),
    exec_name       text NOT NULL,          -- the channel, sink or adapter name from enroll (TEN-3)
    recipient       text,                   -- the principal the delivery is for, if any
    state           text NOT NULL CHECK (state IN ('planned', 'sending', 'delivered', 'withheld', 'dead')),
    body            jsonb,                  -- content: deleted by task.purge
    data_mark       text,
    dataorigin      text NOT NULL CHECK (dataorigin IN ('model', 'code')),
    notice_version  text,
    external_id     text,
    attempts        int NOT NULL DEFAULT 0,
    lease_no        int NOT NULL DEFAULT 0,
    reason          text,
    run_at          timestamptz,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE INDEX deliveries_queue ON deliveries (tenant, exec_kind, exec_name, seq) WHERE state = 'planned';

-- First contact of a principal with a channel: ai_notice and privacy_notice (DLV-12, DLV-13).
CREATE TABLE contacts (
    tenant              text NOT NULL,
    channel             text NOT NULL,
    principal           text NOT NULL,
    ai_notice_delivery  text,
    ai_notice_at        timestamptz,        -- when ai_notice was delivered
    privacy_version     text,
    created_at          timestamptz NOT NULL,
    PRIMARY KEY (tenant, channel, principal)
);

-- CloudEvents event log; metadata only (AUD-2). The core sets source and id (IDM-4).
CREATE TABLE events (
    seq         bigserial PRIMARY KEY,
    id          text NOT NULL UNIQUE,
    tenant      text NOT NULL,
    type        text NOT NULL,
    source      text NOT NULL,
    subject     text,
    time        timestamptz NOT NULL,
    data        jsonb NOT NULL DEFAULT '{}'
);
CREATE INDEX events_tenant ON events (tenant, seq);

-- Audit chains per tenant and epoch (AUD-6); append-only (AUD-3).
CREATE TABLE audit_heads (
    tenant      text NOT NULL,
    epoch       bigint NOT NULL,
    seq         bigint NOT NULL,
    hash        text NOT NULL,
    PRIMARY KEY (tenant, epoch)
);
CREATE TABLE audit (
    tenant      text NOT NULL,
    epoch       bigint NOT NULL,
    seq         bigint NOT NULL,
    at          timestamptz NOT NULL,
    record      jsonb NOT NULL,
    hash        text NOT NULL,
    PRIMARY KEY (tenant, epoch, seq)
);
CREATE FUNCTION audit_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit is append-only';
END
$$;
CREATE TRIGGER audit_no_update BEFORE UPDATE OR DELETE ON audit
    FOR EACH ROW EXECUTE FUNCTION audit_append_only();

-- Usage lines and reserves in micro-euros (BUD-2, BUD-3, BUD-9, D-87 (1)).
CREATE TABLE spend (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    task            text,
    root            text,
    subject         text,
    role            text,
    worker          text,
    line            text NOT NULL,
    unit            text NOT NULL,
    profile         text,
    state           text NOT NULL CHECK (state IN ('reserved', 'committed', 'estimated')),
    eur_micros      bigint NOT NULL CHECK (eur_micros >= 0),
    tokens          jsonb,
    request_id      text,
    time            timestamptz NOT NULL
);
CREATE INDEX spend_tenant_time ON spend (tenant, time);
CREATE INDEX spend_root ON spend (root);

-- Handles: bearer credentials bound to a lease and the store epoch (SEC-3); stored as hashes.
CREATE TABLE handles (
    hash        text PRIMARY KEY,
    kind        text NOT NULL CHECK (kind IN ('model', 'tools', 'session', 'memory')),
    tenant      text NOT NULL,
    work_id     text NOT NULL,
    lease_no    int NOT NULL,
    epoch       bigint NOT NULL,
    worker_id   text NOT NULL,
    slot        text,
    profile     text,
    created_at  timestamptz NOT NULL
);
CREATE INDEX handles_lease ON handles (work_id, lease_no);
