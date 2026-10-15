-- SPDX-License-Identifier: AGPL-3.0-only
-- The read log of the farm manager seat and the findings of its reader role (MGR-25, MGR-28,
-- MGR-30). A read row keeps only metadata of the object an attempt opened: its kind, id, size,
-- hash and mark, and the basis of the attempt; the content stays where it was. A finding is
-- stored in its tenant and is always untrusted; its note sits in data class content, and the
-- audit keeps only the note's hash and length.
CREATE TABLE manager_reads (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    task        text NOT NULL,
    lease_no    int NOT NULL,
    acting_bot  text,
    epoch       int,
    basis       text NOT NULL,
    kind        text NOT NULL,
    object      text NOT NULL,
    size        bigint,
    hash        text,
    data_mark   text,
    tokens      int,
    personal    boolean NOT NULL DEFAULT false,
    at          timestamptz NOT NULL
);
CREATE INDEX manager_reads_tenant ON manager_reads (tenant, at);
CREATE INDEX manager_reads_attempt ON manager_reads (task, lease_no);

CREATE TABLE findings (
    id            text PRIMARY KEY,
    tenant        text NOT NULL,
    task          text NOT NULL,
    incident      text,
    kind          text NOT NULL,
    subject_kind  text NOT NULL,
    subject_id    text NOT NULL,
    body          jsonb NOT NULL,
    note          text,
    untrusted     boolean NOT NULL DEFAULT true CHECK (untrusted),
    personal_seen boolean NOT NULL DEFAULT false,
    in_code_targets boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL
);
CREATE INDEX findings_tenant ON findings (tenant, created_at);
CREATE INDEX findings_task ON findings (task);
