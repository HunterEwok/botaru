-- SPDX-License-Identifier: AGPL-3.0-only
-- Model grants and takeovers (MGR-17, MGR-18, MGR-21, spec/lifecycles/grant.md): a request of
-- an executor attempt for a stronger model profile for one task, decided by the farm manager
-- seat in a batch or by a person, and a takeover of a task on the seat profile after a person's
-- takeover_confirm card. Money is in micro-euros; the API shows euro cents. note is the
-- untrusted note of the request (class content); the seat never sees it.
CREATE TABLE model_grants (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    task            text NOT NULL,
    lease_no        int,
    role            text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('grant', 'takeover')),
    line            text NOT NULL CHECK (line IN ('grant', 'takeover')),
    profile         text NOT NULL,
    request         jsonb NOT NULL DEFAULT '{}',
    note            text,
    attempts        int NOT NULL DEFAULT 1,
    attempts_left   int NOT NULL DEFAULT 0,
    estimate_micros bigint NOT NULL DEFAULT 0,
    cap_micros      bigint NOT NULL DEFAULT 0,
    expires_at      timestamptz,
    decided_by      jsonb,
    decided_at      timestamptz,
    untrusted       boolean NOT NULL DEFAULT false,
    state           text NOT NULL CHECK (state IN ('requested', 'screened', 'pending', 'approved', 'active', 'consumed', 'denied', 'expired', 'revoked')),
    reason          text,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
-- One grant request per task (MGR-17); a takeover grant belongs to the new task.
CREATE UNIQUE INDEX model_grants_one_request ON model_grants (task) WHERE kind = 'grant';
CREATE INDEX model_grants_live ON model_grants (tenant, state) WHERE state IN ('pending', 'approved', 'active');
-- The calls under a grant draw on the rest of its cap_eur (MGR-18, GW-2).
ALTER TABLE spend ADD COLUMN grant_id text;
CREATE INDEX spend_grant ON spend (grant_id) WHERE grant_id IS NOT NULL;

-- The takeover request of a seat attempt and its takeover_confirm card (MGR-21).
CREATE TABLE takeovers (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    task        text NOT NULL,
    type        text NOT NULL,
    seat_task   text NOT NULL,
    lease_no    int NOT NULL,
    acting_bot  text,
    epoch       int,
    profile     text NOT NULL,
    estimate_micros bigint NOT NULL,
    state       text NOT NULL CHECK (state IN ('staged', 'pending', 'approved', 'rejected', 'expired', 'dropped')),
    nonce       text NOT NULL,
    decided_by  text,
    new_task    text,
    grant_id    text,
    reason      text,
    expires_at  timestamptz,
    created_at  timestamptz NOT NULL,
    updated_at  timestamptz NOT NULL
);
CREATE UNIQUE INDEX takeovers_one_per_attempt ON takeovers (seat_task, lease_no);
CREATE INDEX takeovers_pending ON takeovers (expires_at) WHERE state = 'pending';

-- Bot escalations to the farm manager (MGR-5, MGR-6, COM-13, spec/lifecycles/escalation.md).
-- staged is the record of a live attempt before its complete publishes it. fields are the
-- closed fields; note is untrusted (class content) and only the tenant's people see it.
CREATE TABLE escalations (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    task          text NOT NULL,
    lease_no      int NOT NULL,
    worker        text NOT NULL,
    role          text NOT NULL,
    fields        jsonb NOT NULL,
    note          text,
    untrusted     boolean NOT NULL DEFAULT false,
    depth         int NOT NULL DEFAULT 0,
    state         text NOT NULL CHECK (state IN ('staged', 'screened', 'merged', 'dismissed', 'parked', 'in_review', 'forwarded', 'taken', 'answered', 'expired')),
    reason        text,
    merged_into   text,
    dup_count     int NOT NULL DEFAULT 0,
    paid_by       text,
    review_task   text,
    score         int,
    manager_task  text,
    published_at  timestamptz,
    created_at    timestamptz NOT NULL,
    updated_at    timestamptz NOT NULL
);
CREATE UNIQUE INDEX escalations_one_per_attempt ON escalations (task, lease_no);
CREATE INDEX escalations_live ON escalations (tenant, state) WHERE state IN ('screened', 'parked', 'in_review', 'forwarded', 'taken');
