-- SPDX-License-Identifier: AGPL-3.0-only
-- A fact ages from its last verification, the same as a memory record from its last change
-- (MEM-10): the retention loop reads greatest(created_at, verified_at).
DROP INDEX IF EXISTS facts_age;
CREATE INDEX facts_age ON facts (greatest(created_at, verified_at));
