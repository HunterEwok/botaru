-- SPDX-License-Identifier: AGPL-3.0-only
-- A person's export request is closed once the sink took the encrypted copy and the person was
-- told where it is (SEC-10).
ALTER TABLE subject_requests DROP CONSTRAINT subject_requests_state_check;
ALTER TABLE subject_requests ADD CONSTRAINT subject_requests_state_check CHECK (state IN ('pending', 'approved', 'rejected', 'withdrawn', 'done'));
