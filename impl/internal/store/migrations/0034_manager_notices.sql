-- SPDX-License-Identifier: AGPL-3.0-only
-- The manager_notice messages to people (MGR-39, MGR-13): one message per basis and recipient
-- chat, of the route tenant (the people of the incident's tenant) or farm (the farm operators,
-- codes and numbers only). The row names the delivery of the first message; later deliveries
-- edit it through the channel driver's edit, or, on a driver without edit, send the new lines
-- of the basis log at most once in 15 minutes; each delivery records in its body the lines it
-- carries (lines_to), so the next batch starts after the last delivered one.
CREATE TABLE manager_notices (
    delivery    text PRIMARY KEY,
    tenant      text NOT NULL,
    basis_kind  text NOT NULL CHECK (basis_kind IN ('incident')),
    basis       text NOT NULL,
    route       text NOT NULL CHECK (route IN ('tenant', 'farm')),
    recipient   text NOT NULL,
    channel     text NOT NULL,
    target_id   text NOT NULL,
    edit        boolean NOT NULL,
    batch_at    timestamptz,
    created_at  timestamptz NOT NULL
);
CREATE INDEX manager_notices_basis ON manager_notices (basis_kind, basis);
-- The lines of the manager_notice of an incident: its states and operations, codes and ids only.
ALTER TABLE incidents ADD COLUMN notice_log jsonb NOT NULL DEFAULT '[]';
