// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The P2 views of the operator: dead work and its redrive (DLV-4), the audit query (AUD-7),
// status (MAN-2, MAN-8) and doctor (OBS-4, CAP-5). In this build P2 has only operator
// credentials, each for one tenant; an operator of farm may name another tenant where the
// request has the field tenant (TEN-2).

// deadReasons are the reasons of a task that ran out of attempts, runs or time (DLV-4).
var deadReasons = []string{"attempts_exhausted", "runs_exhausted", "deadline_exceeded"}

// viewTenant is the tenant of a read operation: the operator's own, or the named one for an
// operator of farm.
func viewTenant(op Operator, named string) (string, error) {
	if named == "" || named == op.Tenant {
		return op.Tenant, nil
	}
	if op.Tenant != "farm" {
		return "", refuse("forbidden", "the operator credential is for another tenant")
	}
	return named, nil
}

// DeadItem is one item of p2-deadletter-list-response.
type DeadItem struct {
	Kind   string `json:"kind"`
	ID     string `json:"id"`
	Tenant string `json:"tenant"`
	Type   string `json:"type,omitempty"`
	Reason string `json:"reason"`
	At     string `json:"at"`
}

// DeadList is p2-deadletter-list-response.
type DeadList struct {
	Items []DeadItem `json:"items"`
	Next  string     `json:"next,omitempty"`
}

// DeadletterList lists the dead work of a tenant, oldest first: failed tasks with a reason of
// deadReasons and dead deliveries (DLV-4). The cursor is the time and id of the last item.
func (c *Core) DeadletterList(ctx context.Context, op Operator, req TenantRequest) (*DeadList, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	afterAt, afterID := time.Time{}, ""
	if req.After != "" {
		us, id, ok := strings.Cut(req.After, "/")
		n, perr := strconv.ParseInt(us, 10, 64)
		if !ok || perr != nil {
			return nil, refuse("input_schema_invalid", "after is not a cursor of this operation")
		}
		afterAt, afterID = time.UnixMicro(n).UTC(), id
	}
	out := &DeadList{Items: []DeadItem{}}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT kind, id, type, reason, at FROM (
				SELECT 'task' AS kind, id, type, reason, updated_at AS at FROM tasks WHERE tenant = $1 AND state = 'failed' AND reason = ANY($2)
				UNION ALL
				SELECT 'delivery', id, kind, coalesce(reason, 'attempts_exhausted'), updated_at FROM deliveries WHERE tenant = $1 AND state = 'dead') d
			WHERE (at, id) > ($3, $4) ORDER BY at, id LIMIT $5`, tenant, deadReasons, afterAt, afterID, limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var it DeadItem
			var at time.Time
			if err := rows.Scan(&it.Kind, &it.ID, &it.Type, &it.Reason, &at); err != nil {
				return err
			}
			if len(out.Items) == limit {
				last := out.Items[limit-1]
				lastAt, _ := time.Parse("2006-01-02T15:04:05.000000Z", last.At)
				out.Next = fmt.Sprintf("%d/%s", lastAt.UnixMicro(), last.ID)
				break
			}
			it.Tenant, it.At = tenant, fmtTime(at)
			out.Items = append(out.Items, it)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RedriveRequest is p2-redrive-request.
type RedriveRequest struct {
	Op string `json:"op"`
	ID string `json:"id"`
}

// Redrive creates new work from dead work (DLV-4): a task with the refs link to the original,
// the key system:redrive:<id>:<n> and the rights checked again at enqueue, or a planned
// delivery linked to the dead one, whose ceiling and recipient are checked again at handoff.
// An action.* task is never redriven: it needs a new action request (APR-7). The original must
// still have its body.
func (c *Core) Redrive(ctx context.Context, op Operator, req RedriveRequest) (*Done, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var rejected *Problem
	var newID string
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var id string
		var err error
		switch req.Op {
		case "task.redrive":
			id, rejected, err = c.redriveTask(ctx, tx, now, cat, op, req.ID)
		case "delivery.redrive":
			id, err = c.redriveDelivery(ctx, tx, now, op, req.ID)
		default:
			return refuse("input_schema_invalid", "unknown redrive operation")
		}
		if err != nil || rejected != nil {
			return err
		}
		newID = id
		tenant, kind := op.Tenant, strings.TrimSuffix(req.Op, ".redrive")
		if err := tx.QueryRow(ctx, `SELECT tenant FROM tasks WHERE id = $1 UNION ALL SELECT tenant FROM deliveries WHERE id = $1`, req.ID).Scan(&tenant); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: req.Op, Target: &Target{Kind: kind, ID: req.ID}, OK: true, After: map[string]any{kind: id},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	if rejected != nil {
		return nil, rejected
	}
	return &Done{OK: true, ID: newID}, nil
}

// ownWork checks that the operator may act on work of the tenant.
func ownWork(op Operator, tenant string) error {
	if tenant != op.Tenant && op.Tenant != "farm" {
		return refuse("not_found", "no such work in the tenant of the credential")
	}
	return nil
}

func (c *Core) redriveTask(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, op Operator, id string) (string, *Problem, error) {
	var tenant, typ, state, initiator string
	var reason *string
	var prio int
	var body, rec []byte
	err := tx.QueryRow(ctx, `SELECT tenant, type, state, reason, initiator, priority_class, body, record FROM tasks WHERE id = $1 FOR UPDATE`, id).
		Scan(&tenant, &typ, &state, &reason, &initiator, &prio, &body, &rec)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, refuse("not_found", "no such task")
	}
	if err != nil {
		return "", nil, err
	}
	if err := ownWork(op, tenant); err != nil {
		return "", nil, err
	}
	if state != "failed" || reason == nil || !slices.Contains(deadReasons, *reason) {
		return "", nil, refuse("forbidden", "only a task that ran out of attempts, runs or time is redriven")
	}
	if strings.HasPrefix(typ, "action.") {
		return "", nil, refuse("forbidden", "an action task is not redriven; a new action request is needed")
	}
	if body == nil {
		return "", nil, refuse("gone", "the body of the task is deleted")
	}
	var record struct {
		EnqueuedBy  string  `json:"enqueued_by"`
		FairnessKey string  `json:"fairness_key"`
		Origin      *Origin `json:"origin"`
	}
	if err := json.Unmarshal(rec, &record); err != nil {
		return "", nil, err
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", nil, err
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND record->'refs' ? $2`, tenant, id).Scan(&n); err != nil {
		return "", nil, err
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: tenant, Type: typ, Body: payload, IdempotencyKey: fmt.Sprintf("system:redrive:%s:%d", id, n+1),
		Initiator: initiator, EnqueuedBy: record.EnqueuedBy, Origin: record.Origin, Priority: prio,
		FairnessKey: record.FairnessKey, Refs: []string{id},
	})
	if err != nil {
		return "", nil, err
	}
	if res.Rejected != nil {
		return "", res.Rejected, nil
	}
	return res.TaskID, nil, nil
}

func (c *Core) redriveDelivery(ctx context.Context, tx pgx.Tx, now time.Time, op Operator, id string) (string, error) {
	var d plannedDelivery
	var state string
	var task, recipient, mark, notice, alarmkey *string
	var episode *int64
	var body []byte
	err := tx.QueryRow(ctx, `SELECT tenant, task, kind, addr, target_kind, target_id, exec_kind, exec_name, recipient, state, body, data_mark, dataorigin, notice_version, alarmkey, episode
		FROM deliveries WHERE id = $1 FOR UPDATE`, id).Scan(&d.Tenant, &task, &d.Kind, &d.Addr, &d.TargetKind, &d.TargetID, &d.ExecKind, &d.ExecName,
		&recipient, &state, &body, &mark, &d.DataOrigin, &notice, &alarmkey, &episode)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", refuse("not_found", "no such delivery")
	}
	if err != nil {
		return "", err
	}
	if err := ownWork(op, d.Tenant); err != nil {
		return "", err
	}
	if state != "dead" {
		return "", refuse("forbidden", "only a dead delivery is redriven")
	}
	if body == nil {
		return "", refuse("gone", "the body of the delivery is deleted")
	}
	if err := json.Unmarshal(body, &d.Body); err != nil {
		return "", err
	}
	d.Task, d.Recipient, d.DataMark, d.NoticeVersion, d.Alarmkey = deref(task), deref(recipient), deref(mark), deref(notice), deref(alarmkey)
	if episode != nil {
		d.Episode = *episode
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE redrive_of = $1`, id).Scan(&n); err != nil {
		return "", err
	}
	nid, err := c.planDelivery(ctx, tx, now, d)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET redrive_of = $2, redrive_key = $3 WHERE id = $1`, nid, id, fmt.Sprintf("system:redrive:%s:%d", id, n+1)); err != nil {
		return "", err
	}
	// The first contact waits for the new ai_notice, not for the dead one (DLV-13).
	_, err = tx.Exec(ctx, `UPDATE contacts SET ai_notice_delivery = $2 WHERE ai_notice_delivery = $1`, id, nid)
	return nid, err
}

// AuditQueryRequest is p2-audit-query-request.
type AuditQueryRequest struct {
	From       string `json:"from"`
	To         string `json:"to"`
	Kind       string `json:"kind"`
	Actor      string `json:"actor"`
	Command    string `json:"command"`
	TargetKind string `json:"target_kind"`
	Limit      int    `json:"limit"`
	AfterSeq   int64  `json:"after_seq"`
}

// AuditQueryResult is p2-audit-query-response.
type AuditQueryResult struct {
	Items        []json.RawMessage `json:"items"`
	NextAfterSeq int64             `json:"next_after_seq,omitempty"`
}

// AuditQuery reads the audit records of the credential's tenant by a filter, metadata only
// (AUD-2). In this build only an operator calls P2, which is the emergency access of AUD-7:
// the read itself writes an audit record of kind read with via farmctl. The sequence numbers
// are those of the current epoch of the chain.
func (c *Core) AuditQuery(ctx context.Context, op Operator, req AuditQueryRequest) (*AuditQueryResult, error) {
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	from, to := time.Time{}, time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, p := range []struct {
		s string
		t *time.Time
	}{{req.From, &from}, {req.To, &to}} {
		if p.s == "" {
			continue
		}
		t, err := time.Parse(time.RFC3339Nano, p.s)
		if err != nil {
			return nil, refuse("input_schema_invalid", "from and to are datetimes")
		}
		*p.t = t
	}
	out := &AuditQueryResult{Items: []json.RawMessage{}}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT seq, record FROM audit WHERE tenant = $1 AND epoch = $2 AND seq > $3 AND at >= $4 AND at < $5
			AND ($6 = '' OR record->>'kind' = $6) AND ($7 = '' OR record->'actor'->>'principal' = $7)
			AND ($8 = '' OR record->>'command' = $8) AND ($9 = '' OR record->'target'->>'kind' = $9)
			ORDER BY seq LIMIT $10`, op.Tenant, c.epoch, req.AfterSeq, from, to, req.Kind, req.Actor, req.Command, req.TargetKind, limit+1)
		if err != nil {
			return err
		}
		var last int64
		for rows.Next() {
			var seq int64
			var rec []byte
			if err := rows.Scan(&seq, &rec); err != nil {
				rows.Close()
				return err
			}
			if len(out.Items) == limit {
				out.NextAfterSeq = last
				break
			}
			out.Items, last = append(out.Items, rec), seq
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "read", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "audit.query", Target: &Target{Kind: "audit"}, OK: true, After: map[string]any{"records": len(out.Items)},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// StatusResult is p2-status-response.
type StatusResult struct {
	Commit              string       `json:"commit"`
	SetHash             string       `json:"set_hash,omitempty"`
	Errors              []ApplyError `json:"errors"`
	Holds               []Hold       `json:"holds"`
	ConformsTo          ConformsTo   `json:"conforms_to"`
	FarmManagerDisabled []string     `json:"farm_manager_disabled"`
}

// ConformsTo is the claim of MAN-8: the skeleton version and the profiles.
type ConformsTo struct {
	Skeleton string   `json:"skeleton"`
	Profiles []string `json:"profiles"`
}

// Status shows the applied commit and set hash, the holds of the tenant in force and the claim
// of this build. An apply that failed changes nothing in the store, so errors stay empty; this
// build has no farm manager, so every tenant has it disabled.
func (c *Core) Status(ctx context.Context, op Operator, req TenantRequest) (*StatusResult, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	out := &StatusResult{Errors: []ApplyError{}, FarmManagerDisabled: []string{},
		ConformsTo: ConformsTo{Skeleton: strings.TrimPrefix(spec.Version, "v") + ".0", Profiles: []string{"core"}}}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		err := tx.QueryRow(ctx, `SELECT commit, set_hash FROM apply_status WHERE id = 1`).Scan(&out.Commit, &out.SetHash)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// The stored hash carries the time of the apply, so that core copies reload.
		if n := len("sha256:") + 64; len(out.SetHash) > n {
			out.SetHash = out.SetHash[:n]
		}
		rows, err := tx.Query(ctx, `SELECT name FROM tenants WHERE name <> 'farm' ORDER BY name`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			if op.Tenant == "farm" || t == tenant {
				out.FarmManagerDisabled = append(out.FarmManagerDisabled, t)
			}
		}
		rows.Close()
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	holds, err := c.HoldList(ctx, OperatorActor(op), TenantRequest{Tenant: tenant})
	if err != nil {
		return nil, err
	}
	out.Holds = holds.Items
	return out, nil
}

// DoctorCheck is one check of p2-doctor-response.
type DoctorCheck struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Severity string `json:"severity"`
	Value    any    `json:"value"`
	Tenant   string `json:"tenant,omitempty"`
}

// DoctorResult is p2-doctor-response.
type DoctorResult struct {
	OK     bool          `json:"ok"`
	Checks []DoctorCheck `json:"checks"`
}

// Doctor checks the farm: the tick of each loop, the store volume and the health indicators
// (farm-wide numbers), and for the tenant the holds in force, the dead work, the stale workers,
// the open alarms and the open erasure requests. ok is false when a check of severity fail
// fails.
func (c *Core) Doctor(ctx context.Context, op Operator, req TenantRequest) (*DoctorResult, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	h := c.cfg.Health
	out := &DoctorResult{OK: true, Checks: []DoctorCheck{}}
	add := func(ch DoctorCheck) {
		out.Checks = append(out.Checks, ch)
		if !ch.OK && ch.Severity == "fail" {
			out.OK = false
		}
	}
	if free, total, ok := c.measureCapacity(); ok {
		ratio := float64(free) / float64(total)
		sev := "info"
		switch {
		case ratio < h.StoreFreeHard:
			sev = "fail"
		case ratio < h.StoreFreeSoft:
			sev = "warn"
		}
		add(DoctorCheck{ID: "store.free_ratio", OK: sev == "info", Severity: sev, Value: ratio})
	} else {
		add(DoctorCheck{ID: "store.free_ratio", OK: true, Severity: "info", Value: nil})
	}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		in, err := c.indicators(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, loop := range loopOrder {
			age, ticked := in.ticks[loop]
			ch := DoctorCheck{ID: "loop.tick." + strings.ReplaceAll(loop, "-", "_"), OK: true, Severity: "info", Value: nil}
			if ticked {
				ch.Value = age
				if age > h.LoopTickS {
					ch.OK, ch.Severity = false, "fail"
				}
			}
			add(ch)
		}
		urgent := DoctorCheck{ID: "health.oldest_urgent_age_s", OK: true, Severity: "info", Value: nil}
		if in.urgentAge != nil {
			urgent.Value = *in.urgentAge
			if *in.urgentAge > int64(h.UrgentAgeS) {
				urgent.OK, urgent.Severity = false, "warn"
			}
		}
		add(urgent)
		share := DoctorCheck{ID: "health.delivered_10m_ratio", OK: true, Severity: "info", Value: nil}
		if in.share != nil {
			share.Value = *in.share
			if *in.share < h.DeliveryShareMin {
				share.OK, share.Severity = false, "warn"
			}
		}
		add(share)
		counts := []struct {
			id, sql string
			args    []any
			warn    bool
		}{
			{"holds.active", `SELECT count(*) FROM holds WHERE tenant = $1 AND state = 'active' AND (until IS NULL OR until > $2)`, []any{tenant, now}, false},
			{"deadletter.count", `SELECT (SELECT count(*) FROM tasks WHERE tenant = $1 AND state = 'failed' AND reason = ANY($2))
				+ (SELECT count(*) FROM deliveries WHERE tenant = $1 AND state = 'dead')`, []any{tenant, deadReasons}, true},
			{"workers.stale", `SELECT count(*) FROM workers WHERE tenant = $1 AND stale_since IS NOT NULL`, []any{tenant}, true},
			{"alarms.open", `SELECT count(*) FROM alarm_episodes WHERE tenant = $1 AND state = 'firing'`, []any{tenant}, true},
			{"erasure.open", `SELECT count(*) FROM erasure_requests WHERE tenant = $1 AND state <> 'done'`, []any{tenant}, true},
		}
		if err := c.doctorMoney(ctx, tx, now, tenant, add); err != nil {
			return err
		}
		for _, q := range counts {
			var n int64
			if err := tx.QueryRow(ctx, q.sql, q.args...).Scan(&n); err != nil {
				return fmt.Errorf("doctor %s: %w", q.id, err)
			}
			ch := DoctorCheck{ID: q.id, OK: true, Severity: "info", Value: n, Tenant: tenant}
			if q.warn && n > 0 {
				ch.OK, ch.Severity = false, "warn"
			}
			add(ch)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
