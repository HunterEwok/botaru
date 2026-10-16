-- SPDX-License-Identifier: AGPL-3.0-only
-- A person's request for the model paragraph of an incident's analysis before the incident
-- resolves (MGR-53): kept beside the incident record, which has no such field, so no incident
-- view shows it.
ALTER TABLE incidents ADD COLUMN paragraph_requested_at timestamptz, ADD COLUMN paragraph_requested_by text;
