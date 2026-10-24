-- SPDX-License-Identifier: AGPL-3.0-only
-- The attempt deadline fixed at claim from timeouts.attempt of the task type or of the tenant's
-- dispatch rule (FEN-3, DSP-3); the stall loop ends an attempt lease when it passes.
ALTER TABLE leases ADD COLUMN attempt_until timestamptz;
CREATE INDEX leases_attempt_until ON leases (attempt_until) WHERE state = 'active' AND attempt_until IS NOT NULL;
