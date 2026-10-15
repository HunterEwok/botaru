-- SPDX-License-Identifier: AGPL-3.0-only
-- The lease of the attempt whose model call wrote a usage line: a late call of an expired
-- attempt is not progress of the retry (CNT-4).
ALTER TABLE spend ADD COLUMN attempt_lease_no int;
