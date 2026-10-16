-- SPDX-License-Identifier: AGPL-3.0-only
-- A campaign whose content went by period or erasure (RET-10): its template keeps no goal,
-- parameters or arguments and its items no arguments or result fields, so Retry failed has
-- nothing to plan from.
ALTER TABLE campaigns ADD COLUMN purged_at timestamptz;
