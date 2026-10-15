-- SPDX-License-Identifier: AGPL-3.0-only
-- The usage line of a bot message carries its size and type (BUD-3, MSG-11).
ALTER TABLE spend ADD COLUMN message jsonb;
