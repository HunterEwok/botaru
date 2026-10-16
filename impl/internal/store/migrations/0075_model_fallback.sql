-- SPDX-License-Identifier: AGPL-3.0-only
-- A call on a fallback profile (GW-5): the usage line names the actual profile in profile and
-- carries the fallback marker; slot_profile keeps the profile of the handle's slot.
ALTER TABLE spend ADD COLUMN fallback boolean NOT NULL DEFAULT false;
ALTER TABLE spend ADD COLUMN slot_profile text;
