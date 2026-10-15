-- SPDX-License-Identifier: AGPL-3.0-only
-- Files (ART-1..ART-3, ART-6, PRV-2).

-- content: the file record only the core writes and the bytes (ART-1). sha256 never leaves
-- the core.
CREATE TABLE files (
    file_ref        text PRIMARY KEY,
    tenant          text NOT NULL,
    origin          text NOT NULL CHECK (origin IN ('upload', 'produced', 'ingested')),
    producer        text NOT NULL,
    media_type      text NOT NULL,
    size            bigint NOT NULL,
    sha256          text NOT NULL,
    data_mark       text NOT NULL,
    personal_seen   text CHECK (personal_seen IN ('declared', 'masking')),
    state           text NOT NULL CHECK (state IN ('stored', 'expired', 'purged')),
    provenance      jsonb,
    content         bytea,
    created_at      timestamptz NOT NULL,
    expires_at      timestamptz
);
CREATE INDEX files_tenant ON files (tenant, created_at);
CREATE INDEX files_producer ON files (tenant, producer);

-- A service principal that declares the personal data of its uploads (ART-6).
ALTER TABLE principals ADD COLUMN uploads_personal boolean;

-- A session carries the highest data mark of the tasks that wrote to it, so a later task that
-- loads it is not lower.
ALTER TABLE sessions ADD COLUMN data_mark text;
