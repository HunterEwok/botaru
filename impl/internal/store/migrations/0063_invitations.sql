-- SPDX-License-Identifier: AGPL-3.0-only
-- Invitations and the verification code of privileged groups (PRM-7, PRM-13, IDN-14).

-- An invitation is single-use and has an expiry (PRM-7). The link code carries 128 random bits
-- and is stored only as a keyed hash (IDN-4). privileged lists the groups that stay pending
-- until the operator checks the verification code (invite.confirm, PRM-13).
CREATE TABLE invitations (
    id           text PRIMARY KEY,
    tenant       text NOT NULL,
    created_by   text NOT NULL,
    groups       jsonb NOT NULL,
    privileged   jsonb NOT NULL DEFAULT '[]',
    requested_by text,
    code_hash    text NOT NULL UNIQUE,
    state        text NOT NULL CHECK (state IN ('open', 'used', 'confirmed', 'revoked')),
    used_by      text,
    used_at      timestamptz,
    expires_at   timestamptz NOT NULL,
    created_at   timestamptz NOT NULL
);
CREATE INDEX invitations_creator ON invitations (tenant, created_by) WHERE state = 'open';

-- Who granted a membership, on whose written request and through which invitation; a pending
-- membership of a privileged group waits for the verification code (PRM-13).
ALTER TABLE memberships ADD COLUMN granted_by text;
ALTER TABLE memberships ADD COLUMN requested_by text;
ALTER TABLE memberships ADD COLUMN invite text;
-- A membership pending from an invitation lapses at pending_until unless its code is checked.
ALTER TABLE memberships ADD COLUMN pending_until timestamptz;
