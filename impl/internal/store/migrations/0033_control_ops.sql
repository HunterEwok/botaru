-- SPDX-License-Identifier: AGPL-3.0-only
-- Control operations (MGR-9, MGR-13, MGR-14): one row per applied operation of the farm manager
-- seat or of a person, in the tenant of its target, for the seat's hourly limit, control.list
-- and control.revert. denials counts the seat's control.apply refusals in a row (MGR-14).
CREATE TABLE control_ops (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    op          text NOT NULL,
    target_kind text NOT NULL,
    target_id   text NOT NULL,
    incident    text,
    actor_kind  text NOT NULL CHECK (actor_kind IN ('seat', 'person')),
    actor       text NOT NULL,
    epoch       int,
    origin      text NOT NULL DEFAULT 'code',
    hold        text,
    created_at  timestamptz NOT NULL
);
CREATE INDEX control_ops_tenant ON control_ops (tenant, created_at);
CREATE INDEX control_ops_seat ON control_ops (created_at) WHERE actor_kind = 'seat';
ALTER TABLE seats ADD COLUMN denials int NOT NULL DEFAULT 0;
