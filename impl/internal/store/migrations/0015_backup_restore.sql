-- SPDX-License-Identifier: AGPL-3.0-only
-- Backup reports (RET-8) and the restore from a backup (RET-6).

CREATE TABLE backup_reports (
    id            bigserial PRIMARY KEY,
    kind          text NOT NULL CHECK (kind IN ('backup', 'restore_check')),
    location_id   text NOT NULL,
    started_at    timestamptz NOT NULL,
    finished_at   timestamptz NOT NULL,
    ok            boolean NOT NULL,
    size          bigint,
    reported_by   text NOT NULL,
    created_at    timestamptz NOT NULL
);
CREATE INDEX backup_reports_location ON backup_reports (location_id, kind, finished_at);

-- restoring: a restore began and the core is not ready until restore.complete. restore_point:
-- the newest time the restored store holds, from where the spend gap runs; restored_at: when
-- the restore began, where the gap ends. spend_gap_open: the
-- restore completed without the provider report; daily limits are halved until a
-- reconciliation covers the gap.
ALTER TABLE farm_meta ADD COLUMN restoring boolean NOT NULL DEFAULT false;
ALTER TABLE farm_meta ADD COLUMN restore_point timestamptz;
ALTER TABLE farm_meta ADD COLUMN restored_at timestamptz;
ALTER TABLE farm_meta ADD COLUMN spend_gap_open boolean NOT NULL DEFAULT false;
