-- SPDX-License-Identifier: AGPL-3.0-only
-- The farm manager rules of v0.3 (GOV-47, MGR-45, MGR-47, MGR-48, MGR-50, MGR-52, MGR-53).

-- The restricted mode of the seat in a tenant after a preventable incident (MGR-47), the field
-- restricted_in of the seat record: one row per tenant while it lasts. repeats counts the
-- repeats of the manager_preventable notice sent (GOV-47).
CREATE TABLE manager_restrictions (
    tenant     text PRIMARY KEY,
    incident   text NOT NULL,
    holder     text NOT NULL,
    since      timestamptz NOT NULL,
    repeats    int NOT NULL DEFAULT 0
);

-- The notices of the seat to people that carry the buttons "Useful" and "Not needed" (P1
-- card.feedback): manager_attention (MGR-52) and postmortem_report (MGR-53). Only the share of
-- the answers goes into the holder report (MGR-50).
CREATE TABLE manager_feedback (
    id          text PRIMARY KEY,
    tenant      text NOT NULL,
    kind        text NOT NULL CHECK (kind IN ('manager_attention', 'postmortem_report')),
    recipient   text NOT NULL,
    nonce       text NOT NULL,
    task        text,
    value       text CHECK (value IN ('useful', 'not_needed')),
    answered_at timestamptz,
    created_at  timestamptz NOT NULL
);
CREATE INDEX manager_feedback_day ON manager_feedback (tenant, recipient, kind, created_at);

-- From v0.3 a takeover is paid from the tenant's manager line (MGR-48).
ALTER TABLE model_grants DROP CONSTRAINT model_grants_line_check;
ALTER TABLE model_grants ADD CONSTRAINT model_grants_line_check CHECK (line IN ('grant', 'takeover', 'manager'));
