// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErasureLogEntry is one erasure request of the separate erasure log backup (RET-5, RET-7):
// metadata only, without the strings of an erasure by match.
type ErasureLogEntry struct {
	ID        string          `json:"id"`
	Tenant    string          `json:"tenant"`
	Subject   string          `json:"subject,omitempty"`
	Reason    string          `json:"reason"`
	Author    json.RawMessage `json:"author"`
	Scope     json.RawMessage `json:"scope,omitempty"`
	CreatedAt time.Time       `json:"created_at"`
	Deadline  *time.Time      `json:"deadline_at,omitempty"`
}

// ErasureLogExport reads the erasure log for its separate backup (RET-5): farmctl runs it on the
// core machine as often as the store backup, at least.
func (c *Core) ErasureLogExport(ctx context.Context) ([]ErasureLogEntry, error) {
	rows, err := c.st.Pool.Query(ctx, `SELECT id, tenant, coalesce(subject, ''), reason, author, scope, created_at, deadline_at FROM erasure_requests ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ErasureLogEntry{}
	for rows.Next() {
		var e ErasureLogEntry
		var scope []byte
		if err := rows.Scan(&e.ID, &e.Tenant, &e.Subject, &e.Reason, &e.Author, &scope, &e.CreatedAt, &e.Deadline); err != nil {
			return nil, err
		}
		if scope != nil {
			e.Scope = scope
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// RestoreBegin marks a store just restored from a backup, before any core serves it (RET-6):
// the epoch grows, so every lease and handle of the previous epoch is fenced (FEN-4); the
// erasure requests of the separate log that the store does not have enter it again as
// requested, so the erasure loop erases their data once more; the core is not ready until
// P2 restore.complete. farmctl runs it on the core machine after loading the backup.
func (c *Core) RestoreBegin(ctx context.Context, log []ErasureLogEntry) (int, error) {
	added := 0
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		added = 0
		var point *time.Time
		if err := tx.QueryRow(ctx, `SELECT greatest((SELECT max(time) FROM events), (SELECT max(time) FROM spend), (SELECT max(updated_at) FROM tasks))`).Scan(&point); err != nil {
			return err
		}
		var prevEpoch, epoch int64
		if err := tx.QueryRow(ctx, `UPDATE farm_meta SET epoch = epoch + 1, restoring = true, restore_point = coalesce($1, restore_point), restored_at = $2 WHERE id = 1
			RETURNING epoch - 1, epoch, restore_point`, point, now).Scan(&prevEpoch, &epoch, &point); err != nil {
			return err
		}
		backupAt := now
		if point != nil {
			backupAt = *point
		}
		// The farm chain of the new epoch opens with the restore record (D-86 (7)).
		if _, err := c.appendAuditAt(ctx, tx, now, epoch, Audit{
			Tenant: "farm", Kind: "restore", Actor: Actor{Principal: "system", Via: "farmctl"}, OK: true,
			Restore: map[string]any{"epoch": epoch, "previous_epoch": prevEpoch, "backup_at": fmtTime(backupAt)},
		}); err != nil {
			return err
		}
		for _, e := range log {
			var scope any
			if len(e.Scope) > 0 {
				scope = []byte(e.Scope)
			}
			tag, err := tx.Exec(ctx, `INSERT INTO erasure_requests (id, tenant, subject, reason, author, state, scope, created_at, deadline_at, updated_at)
				VALUES ($1, $2, $3, $4, $5, 'requested', $6, $7, $8, $9) ON CONFLICT (id) DO NOTHING`,
				e.ID, e.Tenant, nullable(e.Subject), e.Reason, []byte(e.Author), scope, e.CreatedAt, e.Deadline, now)
			if err != nil {
				return err
			}
			added += int(tag.RowsAffected())
		}
		_, err := c.appendAuditAt(ctx, tx, now, epoch, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: "system", Via: "farmctl"},
			Command: "restore.begin", Target: &Target{Kind: "store", ID: "store"}, OK: true,
			After: map[string]any{"erasure_requests_replayed": added},
		})
		return err
	})
	return added, err
}

// restoring says whether a restore began and was not completed yet.
func restoring(ctx context.Context, tx pgx.Tx) (bool, error) {
	var r bool
	err := tx.QueryRow(ctx, `SELECT restoring FROM farm_meta WHERE id = 1`).Scan(&r)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return r, err
}

// settleRestore cancels at the first start after a restore what the restore made unsafe
// (RET-6, APR-7): a pending approval becomes canceled(restored) with a card update; an
// approval in executing whose write call is queued or leased becomes exec_unknown with that
// call, and its action is not repeated. It is idempotent, so a second start does nothing.
func (c *Core) settleRestore(ctx context.Context) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if r, err := restoring(ctx, tx); err != nil || !r {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+approvalColumns+` FROM approvals WHERE state IN ('pending', 'executing') ORDER BY id FOR UPDATE`)
		if err != nil {
			return err
		}
		var as []*approvalRow
		for rows.Next() {
			a, err := scanApproval(rows)
			if err != nil {
				rows.Close()
				return err
			}
			as = append(as, a)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, a := range as {
			if a.State == "pending" {
				if err := c.closeApproval(ctx, tx, now, a, "canceled", "restored"); err != nil {
					return err
				}
				continue
			}
			var call *string
			if err := tx.QueryRow(ctx, `SELECT toolcall FROM approvals WHERE id = $1`, a.ID).Scan(&call); err != nil {
				return err
			}
			if call == nil {
				continue
			}
			var state string
			if err := tx.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = $1 FOR UPDATE`, *call).Scan(&state); err != nil {
				return err
			}
			if state != "queued" && state != "leased" {
				continue
			}
			// The call ends as exec_unknown the usual way: the task and the approval learn it,
			// and the action is not repeated.
			if err := c.endToolCall(ctx, tx, now, *call, "exec_unknown", "restored", nil); err != nil {
				return err
			}
			var still bool
			if err := tx.QueryRow(ctx, `SELECT state = 'executing' FROM approvals WHERE id = $1`, a.ID).Scan(&still); err != nil {
				return err
			}
			if still {
				if err := c.closeApproval(ctx, tx, now, a, "exec_unknown", "restored"); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// RestoreCompleteRequest is p2-restore-complete-request.
type RestoreCompleteRequest struct {
	Op           string `json:"op"`
	SpendPending bool   `json:"spend_pending"`
}

// RestoreComplete makes the core ready after a restore (RET-6). Without spend_pending every
// workspace must have a reconciliation of each UTC day from the restore point to yesterday,
// so the usage made between the backup and the failure is back in the limits. With
// spend_pending the core becomes ready with the gap open: alarm.spend.unreconciled and the
// daily limits of every tenant halved, until a reconciliation covers the gap.
func (c *Core) RestoreComplete(ctx context.Context, op Operator, req RestoreCompleteRequest) (*Done, error) {
	if op.Tenant != "farm" {
		return nil, refuse("forbidden", "only an operator of farm completes a restore")
	}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if r, err := restoring(ctx, tx); err != nil {
			return err
		} else if !r {
			return refuse("conflict", "no restore is in progress")
		}
		missing, err := spendGap(ctx, tx, now)
		if err != nil {
			return err
		}
		if len(missing) > 0 && !req.SpendPending {
			return refuse("not_ready", "the spend gap since the backup is not reconciled: %s", strings.Join(missing, ", "))
		}
		gap := len(missing) > 0
		if _, err := tx.Exec(ctx, `UPDATE farm_meta SET restoring = false, spend_gap_open = $1 WHERE id = 1`, gap); err != nil {
			return err
		}
		if gap {
			if err := c.fireAlarm(ctx, tx, now, unreconciledAlarm()); err != nil {
				return err
			}
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "restore.complete", Target: &Target{Kind: "store", ID: "store"}, OK: true,
			After: map[string]any{"spend_pending": gap},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	c.restoring.Store(false)
	return &Done{OK: true}, nil
}

func unreconciledAlarm() Alarm {
	return Alarm{Tenant: "farm", Name: "spend.unreconciled", Subject: AlarmSubject{Kind: "store", ID: "store"}, Severity: "warn"}
}

// spendGap lists the provider days from the restore point to the day the restore began, both
// included (UTC), that have no provider report, as provider/day. A provider's report of a day
// covers every workspace of that provider: a report leaves out idle workspaces, and a
// workspace first used after the backup is in it too. The restore day itself is reconciled
// from the next day on, so the gap of a same-day restore stays open until then.
func spendGap(ctx context.Context, tx pgx.Tx, now time.Time) ([]string, error) {
	var point, began *time.Time
	if err := tx.QueryRow(ctx, `SELECT restore_point, restored_at FROM farm_meta WHERE id = 1`).Scan(&point, &began); err != nil {
		return nil, err
	}
	if point == nil {
		return nil, nil
	}
	last := now
	if began != nil {
		last = *began
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT provider FROM spend_workspaces ORDER BY provider`)
	if err != nil {
		return nil, err
	}
	providers, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var missing []string
	end := last.UTC().Truncate(24 * time.Hour)
	for _, p := range providers {
		for d := point.UTC().Truncate(24 * time.Hour); !d.After(end); d = d.Add(24 * time.Hour) {
			var ok bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend_report_days WHERE provider = $1 AND day = $2)`, p, d).Scan(&ok); err != nil {
				return nil, err
			}
			if !ok {
				missing = append(missing, p+"/"+d.Format("2006-01-02"))
			}
		}
	}
	return missing, nil
}

// closeSpendGap closes an open spend gap once reconciliations cover it: the alarm resolves and
// the daily limits return (RET-6, BUD-10).
func (c *Core) closeSpendGap(ctx context.Context, tx pgx.Tx, now time.Time) error {
	var open bool
	if err := tx.QueryRow(ctx, `SELECT spend_gap_open FROM farm_meta WHERE id = 1`).Scan(&open); err != nil || !open {
		return err
	}
	missing, err := spendGap(ctx, tx, now)
	if err != nil || len(missing) > 0 {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE farm_meta SET spend_gap_open = false WHERE id = 1`); err != nil {
		return err
	}
	return c.resolveAlarm(ctx, tx, now, unreconciledAlarm())
}

// spendGapOpen says whether daily limits are halved after a restore with spend_pending.
func spendGapOpen(ctx context.Context, tx pgx.Tx) (bool, error) {
	var open bool
	err := tx.QueryRow(ctx, `SELECT spend_gap_open FROM farm_meta WHERE id = 1`).Scan(&open)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return open, err
}

// loadRestoring reads the restore mark of the store into the core, which every core copy does
// at start and at each tick.
func (c *Core) loadRestoring(ctx context.Context) error {
	var r bool
	err := c.st.Pool.QueryRow(ctx, `SELECT restoring FROM farm_meta WHERE id = 1`).Scan(&r)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	c.restoring.Store(r)
	return nil
}
