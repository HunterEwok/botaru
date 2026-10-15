-- SPDX-License-Identifier: AGPL-3.0-only
-- The farm manager seat (MGR-1, MGR-2, GOV-22): one seat of tenant farm. responsible_since is the
-- start of the holder's unbroken responsibility for MGR-34; unheld_since the time the seat left
-- held, for alarm.manager.absent. An incident keeps whether its class stopped or could have
-- stopped the seat, for seat.resume.
ALTER TABLE seats ADD COLUMN responsible_since timestamptz;
ALTER TABLE seats ADD COLUMN unheld_since timestamptz;
ALTER TABLE incidents ADD COLUMN stops_manager boolean NOT NULL DEFAULT false;
INSERT INTO seats (tenant, id, kind, state, epoch, updated_at, unheld_since)
    VALUES ('farm', 'farm-manager', 'farm_manager', 'vacant', 0, now(), now()) ON CONFLICT DO NOTHING;
