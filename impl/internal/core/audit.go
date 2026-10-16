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
	"github.com/HunterEwok/botaru/impl/internal/store"
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
	// PersonalSeen are the sources of the personal_seen flag of the object, a marker without
	// content (AUD-5).
	PersonalSeen []string
	// ServiceGrant is the block of a service_grant record (APR-17, APR-18).
	ServiceGrant map[string]any
	// Restore is the block of a restore record that opens a new epoch (RET-6, D-86 (7)).
	Restore map[string]any
	// Period is the block of a period_deleted record: the bounds, count and last hash of the
	// months the retention loop deleted (AUD-3).
	Period map[string]any
}

// Actor is who made the change.
type Actor struct {
	Principal string `json:"principal"`
	Via       string `json:"via"`
	Role      string `json:"role,omitempty"`
	Task      string `json:"task,omitempty"`
	// ActingBot and BotEpoch name the identity of a bot's work; the core fills them from the
	// task's lease (IDN-8).
	ActingBot string `json:"acting_bot,omitempty"`
	BotEpoch  int    `json:"bot_epoch,omitempty"`
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
	return c.appendAuditAt(ctx, tx, now, c.epoch, a)
}

// appendAuditAt appends to the chain of the given epoch; only a restore writes to an epoch
// other than the core's own, the first record of the new one.
func (c *Core) appendAuditAt(ctx context.Context, tx pgx.Tx, now time.Time, epoch int64, a Audit) (int64, error) {
	var seq int64
	var prev string
	err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_heads WHERE tenant = $1 AND epoch = $2 FOR UPDATE`, a.Tenant, epoch).Scan(&seq, &prev)
	if errors.Is(err, pgx.ErrNoRows) {
		seq, prev = 0, jcs.ZeroHash
		if _, err := tx.Exec(ctx, `INSERT INTO audit_heads (tenant, epoch, seq, hash) VALUES ($1, $2, 0, $3) ON CONFLICT (tenant, epoch) DO NOTHING`, a.Tenant, epoch, prev); err != nil {
			return 0, err
		}
		if err := tx.QueryRow(ctx, `SELECT seq, hash FROM audit_heads WHERE tenant = $1 AND epoch = $2 FOR UPDATE`, a.Tenant, epoch).Scan(&seq, &prev); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	seq++
	if a.Actor.Via == "bot" && a.Actor.Task != "" && a.Actor.ActingBot == "" {
		acting, err := leaseActing(ctx, tx, a.Actor.Task, "")
		if err != nil {
			return 0, err
		}
		a.Actor.ActingBot, a.Actor.BotEpoch = acting.Bot, acting.BotEpoch
	}
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
	if len(a.PersonalSeen) > 0 {
		rec["personal_seen"] = a.PersonalSeen
	}
	if a.ServiceGrant != nil {
		rec["service_grant"] = a.ServiceGrant
	}
	if a.Restore != nil {
		rec["restore"] = a.Restore
	}
	if a.Period != nil {
		rec["period"] = a.Period
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
		a.Tenant, epoch, seq, now, raw, h); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `UPDATE audit_heads SET seq = $2, hash = $3 WHERE tenant = $1 AND epoch = $4`, a.Tenant, seq, h, epoch); err != nil {
		return 0, err
	}
	return seq, nil
}

// VerifyAudit recomputes the chain of a tenant in the current epoch (P2 audit.verify, AUD-6).
// It returns the seq checked up to (the number of records ever written to the chain, the
// records below a cut included) and the seq of the first broken record, or 0. The seq
// numbers must run from 1, or from the cut, without gaps, and the last record must be the one
// the audit head names, so a deleted tail is found too.
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

// verifyRows checks the chain from its start, or from its cut when the retention loop deleted
// whole months of it: the cut holds the seq and hash of the last record deleted (AUD-3). A cut
// holds only when the kept chain has the period_deleted record of that cut (validCut); else
// the chain is broken just after the cut.
func (c *Core) verifyRows(ctx context.Context, tx pgx.Tx, tenant string) (count int64, brokenAt int64, prev string, err error) {
	prev = jcs.ZeroHash
	err = tx.QueryRow(ctx, `SELECT seq, hash FROM audit_cuts WHERE tenant = $1 AND epoch = $2`, tenant, c.epoch).Scan(&count, &prev)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", err
	}
	if count > 0 {
		if ok, err := validCut(ctx, tx, tenant, c.epoch, prev); err != nil || !ok {
			return count, count + 1, prev, err
		}
	}
	rows, err := tx.Query(ctx, `SELECT seq, record, hash FROM audit WHERE tenant = $1 AND epoch = $2 AND seq > $3 ORDER BY seq`, tenant, c.epoch, count)
	if err != nil {
		return 0, 0, "", err
	}
	defer rows.Close()
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

// auditMinPeriod is the shortest metadata period a tenant may set (min_days of the class
// metadata, RET-9): no audit younger than it is ever cut.
const auditMinPeriod = 93 * 24 * time.Hour

// validCut says whether the cut of a chain with the hash cutHash is one the retention loop made:
// the kept chain has a period_deleted record whose last_hash is the cut hash and whose newest
// deleted record (period.to) was older than the shortest metadata period when the record was
// written (AUD-3, RET-10).
func validCut(ctx context.Context, q store.Querier, tenant string, epoch int64, cutHash string) (bool, error) {
	var at time.Time
	var to string
	err := q.QueryRow(ctx, `SELECT at, record->'period'->>'to' FROM audit WHERE tenant = $1 AND epoch = $2 AND record->>'kind' = 'period_deleted'
		AND record->'period'->>'last_hash' = $3 ORDER BY seq LIMIT 1`, tenant, epoch, cutHash).Scan(&at, &to)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	end, err := time.Parse(time.RFC3339Nano, to)
	return err == nil && !end.Add(auditMinPeriod).After(at), nil
}
