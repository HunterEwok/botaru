// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// Audit is the content of an audit record before the core numbers and chains it
// (spec/model/audit-record.schema.json). Metadata only: codes and ids (AUD-2).
type Audit struct {
	Tenant  string
	Kind    string // command, decision, authorize, service_grant, read, period_deleted, restore
	Actor   Actor
	Command string
	Target  *Target
	Before  map[string]any
	After   map[string]any
	Reason  string
	GitRev  string
	OK      bool
	Flags   []string
}

// Actor is who made the change.
type Actor struct {
	Principal string `json:"principal"`
	Via       string `json:"via"`
	Role      string `json:"role,omitempty"`
	Task      string `json:"task,omitempty"`
}

// Target is what the change is about.
type Target struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// workerPrincipal writes a worker id as a principal reference: the reference grammar allows
// only lowercase after "worker:", and the ULID alphabet is case-insensitive.
func workerPrincipal(workerID string) string { return "worker:" + strings.ToLower(workerID) }

// appendAudit numbers the record in the tenant chain and links it to the previous hash
// (AUD-1, AUD-6). The chain head row is locked, so records of one tenant are serialized.
func (c *Core) appendAudit(ctx context.Context, tx pgx.Tx, now time.Time, a Audit) (int64, error) {
	var seq int64
	var prev string
	err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_heads WHERE tenant = $1 AND epoch = $2 FOR UPDATE`, a.Tenant, c.epoch).Scan(&seq, &prev)
	if errors.Is(err, pgx.ErrNoRows) {
		seq, prev = 0, jcs.ZeroHash
		if _, err := tx.Exec(ctx, `INSERT INTO audit_heads (tenant, epoch, seq, hash) VALUES ($1, $2, 0, $3) ON CONFLICT (tenant, epoch) DO NOTHING`, a.Tenant, c.epoch, prev); err != nil {
			return 0, err
		}
		if err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_heads WHERE tenant = $1 AND epoch = $2 FOR UPDATE`, a.Tenant, c.epoch).Scan(&seq, &prev); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	seq++
	rec := map[string]any{
		"seq":       seq,
		"at":        fmtTime(now),
		"tenant":    a.Tenant,
		"kind":      a.Kind,
		"actor":     a.Actor,
		"ok":        a.OK,
		"prev_hash": prev,
	}
	if a.Command != "" {
		rec["command"] = a.Command
	}
	if a.Target != nil {
		rec["target"] = a.Target
	}
	if len(a.Before) > 0 {
		rec["before"] = a.Before
	}
	if len(a.After) > 0 {
		rec["after"] = a.After
	}
	if a.Reason != "" {
		rec["reason"] = a.Reason
	}
	if a.GitRev != "" {
		rec["git_rev"] = a.GitRev
	}
	if len(a.Flags) > 0 {
		rec["flags"] = a.Flags
	}
	h, err := jcs.Hash(rec)
	if err != nil {
		return 0, err
	}
	rec["hash"] = h
	raw, err := json.Marshal(rec)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit (tenant, epoch, seq, at, record, hash) VALUES ($1, $2, $3, $4, $5, $6)`,
		a.Tenant, c.epoch, seq, now, raw, h); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_heads SET seq = $2, hash = $3 WHERE tenant = $1 AND epoch = $4`, a.Tenant, seq, h, c.epoch); err != nil {
		return 0, err
	}
	return seq, nil
}

// VerifyAudit recomputes the chain of a tenant in the current epoch (P2 audit.verify, AUD-6).
// It returns the number of records and the seq of the first broken record, or 0. The seq
// numbers must run from 1 without gaps, and the last record must be the one the audit head
// names, so a deleted tail is found too.
// The head and the rows are read in one snapshot, so a record appended meanwhile is in both
// or in neither.
func (c *Core) VerifyAudit(ctx context.Context, tenant string) (count int64, brokenAt int64, err error) {
	tx, err := c.st.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return 0, 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var headSeq int64
	headHash := jcs.ZeroHash
	err = tx.QueryRow(ctx, `SELECT seq, hash FROM audit_heads WHERE tenant = $1 AND epoch = $2`, tenant, c.epoch).Scan(&headSeq, &headHash)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, err
	}
	count, brokenAt, prev, err := c.verifyRows(ctx, tx, tenant)
	if err != nil || brokenAt != 0 {
		return count, brokenAt, err
	}
	if count != headSeq || prev != headHash {
		return count, count + 1, nil
	}
	return count, 0, nil
}

func (c *Core) verifyRows(ctx context.Context, tx pgx.Tx, tenant string) (count int64, brokenAt int64, prev string, err error) {
	rows, err := tx.Query(ctx, `SELECT seq, record, hash FROM audit WHERE tenant = $1 AND epoch = $2 ORDER BY seq`, tenant, c.epoch)
	if err != nil {
		return 0, 0, "", err
	}
	defer rows.Close()
	prev = jcs.ZeroHash
	for rows.Next() {
		var seq int64
		var raw []byte
		var stored string
		if err := rows.Scan(&seq, &raw, &stored); err != nil {
			return 0, 0, "", err
		}
		count++
		if seq != count {
			return count, count, prev, nil
		}
		var rec map[string]any
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&rec); err != nil {
			return count, seq, prev, nil
		}
		delete(rec, "hash")
		h, err := jcs.Hash(rec)
		if err != nil || h != stored || rec["prev_hash"] != prev || fmt.Sprint(rec["seq"]) != strconv.FormatInt(seq, 10) {
			return count, seq, prev, nil
		}
		prev = stored
	}
	return count, 0, prev, rows.Err()
}

// Event is a CloudEvents record of the event log; data is metadata only (AUD-2).
type Event struct {
	Type    string
	Subject string
	Data    map[string]any
	// Causation is the direct cause when the core knows a better one than the default: the
	// task of a task event, the task (or else the delivery) of a delivery event (OBS-2).
	Causation string
}

// appendEvent writes an event; the core sets source and id (IDM-4).
func (c *Core) appendEvent(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, e Event) error {
	data := e.Data
	if data == nil {
		data = map[string]any{}
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	corr, cause, err := eventCause(ctx, tx, e)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO events (id, tenant, type, source, subject, time, data, correlationid, causationid) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		ids.New(ids.Event), tenant, e.Type, "botaru:core/"+tenant, nullable(e.Subject), now, raw, nullable(corr), nullable(cause))
	return err
}

// eventCause gives an event its correlationid, the id of the root of the task tree, and its
// causationid, the id of the direct cause (OBS-2). An event outside a task tree correlates to
// its own subject.
func eventCause(ctx context.Context, tx pgx.Tx, e Event) (string, string, error) {
	corr, cause := e.Subject, e.Causation
	switch {
	case strings.HasPrefix(e.Type, "task."):
		var root string
		err := tx.QueryRow(ctx, `SELECT coalesce(record->>'root', id) FROM tasks WHERE id = $1`, e.Subject).Scan(&root)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", "", err
		}
		if root != "" {
			corr = root
		}
		if cause == "" {
			cause = e.Subject
		}
	case strings.HasPrefix(e.Type, "delivery."):
		var task, root *string
		err := tx.QueryRow(ctx, `SELECT d.task, coalesce(t.record->>'root', t.id) FROM deliveries d LEFT JOIN tasks t ON t.id = d.task WHERE d.id = $1`, e.Subject).Scan(&task, &root)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", "", err
		}
		if root != nil {
			corr = *root
		}
		if cause == "" {
			cause = e.Subject
			if task != nil {
				cause = *task
			}
		}
	}
	return corr, cause, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
