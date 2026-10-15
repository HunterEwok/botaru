-- SPDX-License-Identifier: AGPL-3.0-only
-- Persona texts (GOV-4, COM-19): a file of git pinned by hash in a tenant charter. farmctl apply
-- sends the texts of the pinned files; the core keeps each by its hash in the tenant and puts it
-- into the trusted instruction of an attempt.
CREATE TABLE personas (
    tenant     text NOT NULL,
    sha256     text NOT NULL,
    body       text NOT NULL,
    created_at timestamptz NOT NULL,
    PRIMARY KEY (tenant, sha256)
);
