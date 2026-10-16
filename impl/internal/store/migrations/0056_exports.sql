-- SPDX-License-Identifier: AGPL-3.0-only
-- Exports of a tenant or a principal (LCY-2, SEC-10): the core export task writes one JSON Lines
-- file, encrypted with the recipient's public key, and plans one delivery of kind export to the
-- export_only sink. A person's request for a copy is a subject request of kind export with the
-- public key the person supplied.
CREATE TABLE exports (
    id              text PRIMARY KEY,
    tenant          text NOT NULL,
    kind            text NOT NULL CHECK (kind IN ('tenant', 'principal')),
    subject         text,
    sink            text NOT NULL,
    recipient_ref   text NOT NULL,
    recipient_key   text NOT NULL,
    subject_request text,
    author          text NOT NULL,
    state           text NOT NULL CHECK (state IN ('queued', 'done', 'failed')),
    reason          text,
    file_ref        text,
    lines           int,
    created_at      timestamptz NOT NULL,
    updated_at      timestamptz NOT NULL
);
CREATE INDEX exports_queued ON exports (created_at) WHERE state = 'queued';

ALTER TABLE subject_requests ADD COLUMN recipient_key text;
ALTER TABLE subject_requests DROP CONSTRAINT subject_requests_kind_check;
ALTER TABLE subject_requests ADD CONSTRAINT subject_requests_kind_check CHECK (kind IN ('erase', 'erase_match', 'export'));
