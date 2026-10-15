// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// BackupConfig holds the backup settings of the deployment (RET-5, RET-8).
type BackupConfig struct {
	// Locations are the location_id values of the store backups from the deployment
	// description; the health loop watches each.
	Locations []string `yaml:"locations"`
	// ErasureLogLocation is the location_id the backup process reports the separate erasure
	// log backup with; doctor fails when it is older than the newest store backup.
	ErasureLogLocation string `yaml:"erasure_log_location"`
	// Monitor is true in an installation that claims profile monitor: the monitoring system
	// watches the backups, and the core raises no backup alarm.
	Monitor bool `yaml:"monitor"`
}

const (
	backupOverdue       = 26 * time.Hour
	restoreCheckOverdue = 8 * 24 * time.Hour
	backupClockSkew     = 5 * time.Minute
)

// BackupReportRequest is p2-backup-report-request.
type BackupReportRequest struct {
	Op         string `json:"op"`
	Kind       string `json:"kind"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
	OK         bool   `json:"ok"`
	Size       *int64 `json:"size"`
	LocationID string `json:"location_id"`
}

// BackupReport records a report of the external backup process after a store backup, an
// erasure log backup or a restore check (RET-8). The process reports with its report key, which
// opens only the report operations, or an operator credential of farm.
func (c *Core) BackupReport(ctx context.Context, op Operator, req BackupReportRequest) (*Done, error) {
	if op.Tenant != "farm" {
		return nil, refuse("forbidden", "only the backup process of farm reports backups")
	}
	started, err1 := time.Parse(time.RFC3339, req.StartedAt)
	finished, err2 := time.Parse(time.RFC3339, req.FinishedAt)
	if err1 != nil || err2 != nil || finished.Before(started) {
		return nil, refuse("input_schema_invalid", "started_at and finished_at are times, the end not before the start")
	}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// A report from the future would count as a fresh backup and hold the alarms off
		// (RET-8); a few minutes cover the clock skew of the backup machine.
		if finished.After(now.Add(backupClockSkew)) {
			return refuse("input_schema_invalid", "finished_at is in the future")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO backup_reports (kind, location_id, started_at, finished_at, ok, size, reported_by, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, req.Kind, req.LocationID, started, finished, req.OK, req.Size, op.Principal, now); err != nil {
			return err
		}
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "backup.report", Target: &Target{Kind: "backup", ID: req.LocationID}, OK: true,
			After: map[string]any{"kind": req.Kind, "ok": req.OK},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &Done{OK: true}, nil
}

// backupState is what the latest reports say about one location.
type backupState struct {
	Location string
	// LastOK is the end of the newest good report of the kind, or nil.
	LastOK, LastCheckOK *time.Time
	// LastFailed says whether the newest report of the kind failed.
	LastFailed, LastCheckFailed bool
}

func (c *Core) backupLocations(ctx context.Context, tx pgx.Tx) ([]string, error) {
	locs := slices.Clone(c.cfg.Backup.Locations)
	rows, err := tx.Query(ctx, `SELECT DISTINCT location_id FROM backup_reports`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			rows.Close()
			return nil, err
		}
		if !slices.Contains(locs, l) && l != c.cfg.Backup.ErasureLogLocation {
			locs = append(locs, l)
		}
	}
	rows.Close()
	slices.Sort(locs)
	return locs, rows.Err()
}

func backupStateOf(ctx context.Context, tx pgx.Tx, loc string) (backupState, error) {
	s := backupState{Location: loc}
	last := func(kind string) (*time.Time, bool, error) {
		var ok *time.Time
		var failed bool
		err := tx.QueryRow(ctx, `SELECT (SELECT max(finished_at) FROM backup_reports WHERE location_id = $1 AND kind = $2 AND ok),
			coalesce((SELECT NOT ok FROM backup_reports WHERE location_id = $1 AND kind = $2 ORDER BY finished_at DESC, id DESC LIMIT 1), false)`,
			loc, kind).Scan(&ok, &failed)
		return ok, failed, err
	}
	var err error
	if s.LastOK, s.LastFailed, err = last("backup"); err != nil {
		return s, err
	}
	s.LastCheckOK, s.LastCheckFailed, err = last("restore_check")
	return s, err
}

// backupAlarms raises and resolves the backup alarms of an installation without profile
// monitor (RET-8): no good backup of a location for 26 hours or a failed one gives
// alarm.backup.overdue; no good restore check for 8 days or a failed one gives
// alarm.backup.restore_check_overdue. A location without any report counts from the creation
// of the store.
func (c *Core) backupAlarms(ctx context.Context, tx pgx.Tx, now time.Time, set func(Alarm, bool) error) error {
	if c.cfg.Backup.Monitor {
		return nil
	}
	locs, err := c.backupLocations(ctx, tx)
	if err != nil {
		return err
	}
	var created time.Time
	if err := tx.QueryRow(ctx, `SELECT created_at FROM farm_meta WHERE id = 1`).Scan(&created); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	since := func(t *time.Time) time.Time {
		if t == nil {
			return created
		}
		return *t
	}
	for _, loc := range locs {
		s, err := backupStateOf(ctx, tx, loc)
		if err != nil {
			return err
		}
		subject := AlarmSubject{Kind: "backup", ID: checkID(loc)}
		if err := set(Alarm{Tenant: "farm", Name: "backup.overdue", Subject: subject, Severity: "warn"},
			s.LastFailed || now.Sub(since(s.LastOK)) > backupOverdue); err != nil {
			return err
		}
		if err := set(Alarm{Tenant: "farm", Name: "backup.restore_check_overdue", Subject: subject, Severity: "warn"},
			s.LastCheckFailed || now.Sub(since(s.LastCheckOK)) > restoreCheckOverdue); err != nil {
			return err
		}
	}
	return nil
}

// doctorBackup adds the age of the last good backup and restore check of each location, and
// fails when the erasure log backup is older than the newest store backup (RET-8, RET-5).
func (c *Core) doctorBackup(ctx context.Context, tx pgx.Tx, now time.Time, add func(DoctorCheck)) error {
	locs, err := c.backupLocations(ctx, tx)
	if err != nil {
		return err
	}
	var newest *time.Time
	age := func(t *time.Time) any {
		if t == nil {
			return nil
		}
		return int64(now.Sub(*t).Seconds())
	}
	for _, loc := range locs {
		s, err := backupStateOf(ctx, tx, loc)
		if err != nil {
			return err
		}
		if s.LastOK != nil && (newest == nil || s.LastOK.After(*newest)) {
			newest = s.LastOK
		}
		b := DoctorCheck{ID: checkID("backup.age_s." + loc), OK: true, Severity: "info", Value: age(s.LastOK)}
		if s.LastFailed || s.LastOK == nil || now.Sub(*s.LastOK) > backupOverdue {
			b.OK, b.Severity = false, "warn"
		}
		add(b)
		r := DoctorCheck{ID: checkID("backup.restore_check_age_s." + loc), OK: true, Severity: "info", Value: age(s.LastCheckOK)}
		if s.LastCheckFailed || s.LastCheckOK == nil || now.Sub(*s.LastCheckOK) > restoreCheckOverdue {
			r.OK, r.Severity = false, "warn"
		}
		add(r)
	}
	if loc := c.cfg.Backup.ErasureLogLocation; loc != "" {
		s, err := backupStateOf(ctx, tx, loc)
		if err != nil {
			return err
		}
		ch := DoctorCheck{ID: "backup.erasure_log_age_s", OK: true, Severity: "info", Value: age(s.LastOK)}
		if newest != nil && (s.LastOK == nil || s.LastOK.Before(*newest)) {
			ch.OK, ch.Severity = false, "fail"
		}
		add(ch)
	}
	var restoringNow, gap bool
	if err := tx.QueryRow(ctx, `SELECT restoring, spend_gap_open FROM farm_meta WHERE id = 1`).Scan(&restoringNow, &gap); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	add(DoctorCheck{ID: "store.restoring", OK: !restoringNow, Severity: map[bool]string{true: "fail", false: "info"}[restoringNow], Value: restoringNow})
	add(DoctorCheck{ID: "spend.gap_open", OK: !gap, Severity: map[bool]string{true: "warn", false: "info"}[gap], Value: gap})
	return nil
}
