// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"math"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// HeartbeatRequest is p1-heartbeat-request.
type HeartbeatRequest struct {
	Stage    string   `json:"stage,omitempty"`
	Progress *float64 `json:"progress,omitempty"`
	Usage    *struct {
		InputTokens  int64 `json:"input_tokens,omitempty"`
		OutputTokens int64 `json:"output_tokens,omitempty"`
	} `json:"usage,omitempty"`
}

// HeartbeatResult is p1-heartbeat-response.
type HeartbeatResult struct {
	LeaseUntil      string `json:"lease_until,omitempty"`
	LeaseRemainingS int    `json:"lease_remaining_s"`
	CancelRequested bool   `json:"cancel_requested"`
}

// CompleteRequest is p1-complete-request.
type CompleteRequest struct {
	Result  map[string]any `json:"result,omitempty"`
	Output  map[string]any `json:"output,omitempty"`
	Receipt *Receipt       `json:"receipt,omitempty"`
	Erasure map[string]any `json:"erasure,omitempty"`
}

// Receipt is a delivery receipt.
type Receipt struct {
	ExternalID string `json:"external_id,omitempty"`
	ExitCode   *int   `json:"exit_code,omitempty"`
	Output     string `json:"output,omitempty"`
	Location   string `json:"location,omitempty"`
}

// CompleteResult is p1-complete-response.
type CompleteResult struct {
	LeaseState string `json:"lease_state"`
}

// FailRequest is p1-fail-request.
type FailRequest struct {
	Reason      string `json:"reason"`
	Retryable   bool   `json:"retryable"`
	RetryAfterS int    `json:"retry_after_s,omitempty"`
}

// FailResult is p1-fail-response.
type FailResult struct {
	RetryAt string `json:"retry_at,omitempty"`
	Final   bool   `json:"final"`
	Reason  string `json:"reason,omitempty"`
}

// ReleaseResult is p1-release-response.
type ReleaseResult struct {
	LeaseState string `json:"lease_state"`
}

// underLease runs fn under a checked lease; a repeat of an accepted call returns the stored
// answer without side effects (IDM-3).
func underLease[T any](ctx context.Context, c *Core, w *Worker, header, op string, args any, fn func(tx pgx.Tx, now time.Time, l *leaseRow) (*T, error)) (*T, error) {
	var out *T
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		l, replay, err := c.heldLease(ctx, tx, w, header, op, args)
		if err != nil {
			return err
		}
		if replay != nil {
			var v T
			if err := json.Unmarshal(replay, &v); err != nil {
				return err
			}
			out = &v
			return nil
		}
		if !l.Until.After(now) {
			return refuse("fenced", "the lease has expired")
		}
		out, err = fn(tx, now, l)
		return err
	})
	if err != nil {
		if p, ok := AsProblem(err); ok && (p.Reason == "fenced" || p.Reason == "lease_mismatch") {
			c.auditFence(ctx, w, op, p.Reason)
		}
		return nil, err
	}
	return out, nil
}

// auditFence writes a refused call under a lease to the audit with its code (FEN-2).
func (c *Core) auditFence(ctx context.Context, w *Worker, op, reason string) {
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "bot"},
			Command: op, Target: &Target{Kind: "worker", ID: w.ID}, OK: false, Reason: reason,
		})
		return err
	})
	if err != nil {
		c.log.Warn("audit of a fenced call failed", "err", err)
	}
}

// Heartbeat extends a lease (FEN-4).
func (c *Core) Heartbeat(ctx context.Context, w *Worker, header string, req HeartbeatRequest) (*HeartbeatResult, error) {
	return underLease(ctx, c, w, header, "heartbeat", nil, func(tx pgx.Tx, now time.Time, l *leaseRow) (*HeartbeatResult, error) {
		leaseS := c.cfg.LeaseS
		cancel := false
		if l.Kind == "attempt" {
			var typ, state string
			if err := tx.QueryRow(ctx, `SELECT type, state FROM tasks WHERE id = $1`, l.WorkID).Scan(&typ, &state); err != nil {
				return nil, err
			}
			cancel = state == "canceled"
			if cat, err := c.Catalog(ctx); err == nil {
				if d, ok := parseDuration(cat.TaskTypes[typ].Timeouts.Lease); ok {
					leaseS = int(d / time.Second)
				}
			}
			leaseS = min(leaseS, maxLeaseS)
		}
		until := now.Add(time.Duration(leaseS) * time.Second)
		if _, err := tx.Exec(ctx, `UPDATE leases SET lease_until = $3, reason = NULL WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo, until); err != nil {
			return nil, err
		}
		return &HeartbeatResult{LeaseUntil: fmtTime(until), LeaseRemainingS: leaseS, CancelRequested: cancel}, nil
	})
}

// Complete accepts the outcome of a work item. The transition, the event, the deliveries and
// the audit record are written in one transaction (DLV-2).
func (c *Core) Complete(ctx context.Context, w *Worker, header string, req CompleteRequest) (*CompleteResult, error) {
	return underLease(ctx, c, w, header, "complete", req, func(tx pgx.Tx, now time.Time, l *leaseRow) (*CompleteResult, error) {
		res := &CompleteResult{LeaseState: "closed"}
		switch l.Kind {
		case "attempt":
			if req.Result == nil {
				return nil, refuse("input_schema_invalid", "an attempt completes with result")
			}
			if err := c.completeAttempt(ctx, tx, now, w, l, req.Result); err != nil {
				return nil, err
			}
		case "delivery":
			if req.Receipt == nil {
				return nil, refuse("input_schema_invalid", "a delivery completes with receipt")
			}
			if err := c.completeDelivery(ctx, tx, now, w, l, *req.Receipt); err != nil {
				return nil, err
			}
		default:
			return nil, refuse("input_schema_invalid", "this work kind is not served in v0.1")
		}
		return res, closeLease(ctx, tx, now, l, "closed", "", "complete", req, res)
	})
}

func (c *Core) completeAttempt(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, l *leaseRow, result map[string]any) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var typ, initiator string
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT type, initiator, record FROM tasks WHERE id = $1 FOR UPDATE`, l.WorkID).Scan(&typ, &initiator, &rec); err != nil {
		return err
	}
	tt := cat.TaskTypes[typ]
	if tt.ResultSchema != nil {
		if err := c.rs.validate(tt.ResultSchema, result); err != nil {
			return refuse("output_schema_invalid", "the result does not match result_schema")
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'completed', result = $2, updated_at = $3 WHERE id = $1`, l.WorkID, raw, now); err != nil {
		return err
	}
	if err := releaseTaskReserve(ctx, tx, l.WorkID); err != nil {
		return err
	}
	if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "task.completed", Subject: l.WorkID, Data: map[string]any{"task": l.WorkID}}); err != nil {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "bot", Role: w.Role, Task: l.WorkID},
		Command: "task.complete", Target: &Target{Kind: "task", ID: l.WorkID}, OK: true,
		After: map[string]any{"state": "completed", "worker": w.ID},
	}); err != nil {
		return err
	}
	// The answer goes to the task origin; the address comes from the record, never from the
	// result (DLV-6).
	var record struct {
		Origin   *Origin `json:"origin"`
		DataMark string  `json:"data_mark"`
	}
	if err := json.Unmarshal(rec, &record); err != nil {
		return err
	}
	if record.Origin == nil {
		return nil
	}
	text, fields := resultText(result, tt.SummaryFields)
	body := map[string]any{"form": "result", "text": text}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: w.Tenant, Task: l.WorkID, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: record.Origin.Chat.ID(),
		ExecKind: "channel", ExecName: record.Origin.Channel, Recipient: initiator, Body: body,
		DataMark: record.DataMark, DataOrigin: "model",
	})
	return err
}

func (c *Core) completeDelivery(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, l *leaseRow, r Receipt) error {
	var kind string
	if err := tx.QueryRow(ctx, `UPDATE deliveries SET state = 'delivered', external_id = $2, updated_at = $3 WHERE id = $1 RETURNING kind`,
		l.WorkID, nullable(r.ExternalID), now).Scan(&kind); err != nil {
		return err
	}
	if kind == "ai_notice" {
		if _, err := tx.Exec(ctx, `UPDATE contacts SET ai_notice_at = $2 WHERE ai_notice_delivery = $1`, l.WorkID, now); err != nil {
			return err
		}
		// Model text that waited for the notice can go now (DLV-13).
		if err := notifyWork(ctx, tx); err != nil {
			return err
		}
	}
	if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "delivery.delivered", Subject: l.WorkID, Data: map[string]any{"delivery": l.WorkID, "kind": kind}}); err != nil {
		return err
	}
	_, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "channel"},
		Command: "delivery.complete", Target: &Target{Kind: "delivery", ID: l.WorkID}, OK: true,
		After: map[string]any{"kind": kind, "state": "delivered", "worker": w.ID},
	})
	return err
}

// Fail ends a lease without an outcome. Whether the attempt is spent is decided by the reason
// registry (FEN-3).
func (c *Core) Fail(ctx context.Context, w *Worker, header string, req FailRequest) (*FailResult, error) {
	reasons, err := c.spec.Registry("spec/model/reasons.yaml")
	if err != nil {
		return nil, err
	}
	entry, ok := reasons.Entry(req.Reason)
	if !ok {
		return nil, refuse("input_schema_invalid", "unknown reason code")
	}
	return underLease(ctx, c, w, header, "fail", req, func(tx pgx.Tx, now time.Time, l *leaseRow) (*FailResult, error) {
		var res *FailResult
		var err error
		switch l.Kind {
		case "attempt":
			spends, _ := entry["spends_attempt"].(bool)
			if entry["spends_attempt"] == nil {
				spends = true
			}
			retry, _ := entry["retry"].(string)
			res, err = c.endAttempt(ctx, tx, now, w.Tenant, l, req.Reason, spends, req.Retryable && retry != "none", req.RetryAfterS, retry)
		case "delivery":
			res, err = c.endDelivery(ctx, tx, now, w.Tenant, l, req.Reason, req.Retryable, req.RetryAfterS)
		default:
			return nil, refuse("input_schema_invalid", "this work kind is not served in v0.1")
		}
		if err != nil {
			return nil, err
		}
		return res, closeLease(ctx, tx, now, l, "closed", req.Reason, "fail", req, res)
	})
}

// Release returns the work to the queue without spending an attempt (worker_drain).
func (c *Core) Release(ctx context.Context, w *Worker, header string) (*ReleaseResult, error) {
	return underLease(ctx, c, w, header, "release", nil, func(tx pgx.Tx, now time.Time, l *leaseRow) (*ReleaseResult, error) {
		if err := c.requeue(ctx, tx, now, l, now); err != nil {
			return nil, err
		}
		res := &ReleaseResult{LeaseState: "released"}
		return res, closeLease(ctx, tx, now, l, "released", "worker_drain", "release", nil, res)
	})
}

// requeue puts an attempt or a delivery back to the queue from run_at on.
func (c *Core) requeue(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, runAt time.Time) error {
	var err error
	switch l.Kind {
	case "attempt":
		_, err = tx.Exec(ctx, `UPDATE tasks SET state = 'submitted', run_at = $2, updated_at = $3 WHERE id = $1 AND state = 'working'`, l.WorkID, runAt, now)
	case "delivery":
		_, err = tx.Exec(ctx, `UPDATE deliveries SET state = 'planned', run_at = $2, updated_at = $3 WHERE id = $1 AND state = 'sending'`, l.WorkID, runAt, now)
	}
	if err != nil {
		return err
	}
	return notifyWork(ctx, tx)
}

// endAttempt decides between a retry and the end of the task after an attempt ended without a
// result: by the spent attempts, the type retry policy and the task deadline (FEN-3).
// A reason with retry mode on_fix is not retried on a timer: the task waits without a run time
// until the next apply wakes it, or until its deadline (spec/model/reasons.yaml).
func (c *Core) endAttempt(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, l *leaseRow, reason string, spends, retryable bool, retryAfterS int, mode string) (*FailResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var typ, initiator string
	var spent int
	var deadline *time.Time
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT type, initiator, attempts_spent, deadline_at, record FROM tasks WHERE id = $1 FOR UPDATE`, l.WorkID).
		Scan(&typ, &initiator, &spent, &deadline, &rec); err != nil {
		return nil, err
	}
	tt := cat.TaskTypes[typ]
	if spends {
		spent++
		if _, err := tx.Exec(ctx, `UPDATE tasks SET attempts_spent = $2 WHERE id = $1`, l.WorkID, spent); err != nil {
			return nil, err
		}
	}
	maxAttempts := tt.Retry.MaxAttempts
	if maxAttempts == 0 {
		maxAttempts = 2
	}
	for _, r := range tt.Retry.NonRetryable {
		if r == reason {
			retryable = false
		}
	}
	finalReason := reason
	if retryable && spent >= maxAttempts {
		retryable, finalReason = false, "attempts_exhausted"
	}
	// A run counts when the model answered during this attempt; retry.max_runs caps them.
	var answered bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend s JOIN leases l ON l.work_id = s.task
		WHERE s.task = $1 AND l.lease_no = $2 AND s.unit = 'tokens' AND s.state = 'committed' AND s.time >= l.created_at)`,
		l.WorkID, l.LeaseNo).Scan(&answered); err != nil {
		return nil, err
	}
	if answered {
		var runs int
		if err := tx.QueryRow(ctx, `UPDATE tasks SET runs_counted = runs_counted + 1 WHERE id = $1 RETURNING runs_counted`, l.WorkID).Scan(&runs); err != nil {
			return nil, err
		}
		if retryable && tt.Retry.MaxRuns > 0 && runs >= tt.Retry.MaxRuns {
			retryable, finalReason = false, "runs_exhausted"
		}
	}
	if retryable && mode == "on_fix" {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'submitted', run_at = 'infinity', reason = 'waiting_on_fix', updated_at = $2 WHERE id = $1 AND state = 'working'`, l.WorkID, now); err != nil {
			return nil, err
		}
		return &FailResult{Final: false}, nil
	}
	if retryable {
		runAt := now.Add(backoff(tt.Retry.Backoff, spent))
		if ra := now.Add(time.Duration(retryAfterS) * time.Second); ra.After(runAt) {
			runAt = ra
		}
		if deadline == nil || runAt.Before(*deadline) {
			if err := c.requeue(ctx, tx, now, l, runAt); err != nil {
				return nil, err
			}
			return &FailResult{RetryAt: fmtTime(runAt), Final: false}, nil
		}
		finalReason = "deadline_exceeded"
	}
	if err := c.failTask(ctx, tx, now, tenant, l.WorkID, initiator, rec, finalReason); err != nil {
		return nil, err
	}
	return &FailResult{Final: true, Reason: finalReason}, nil
}

// failTask moves a task to failed and tells the initiator by a code text at the origin.
func (c *Core) failTask(ctx context.Context, tx pgx.Tx, now time.Time, tenant, id, initiator string, rec []byte, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'failed', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
		return err
	}
	if err := releaseTaskReserve(ctx, tx, id); err != nil {
		return err
	}
	if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "task.failed", Subject: id, Data: map[string]any{"task": id, "reason": reason}}); err != nil {
		return err
	}
	var record struct {
		Origin *Origin `json:"origin"`
	}
	if err := json.Unmarshal(rec, &record); err != nil {
		return err
	}
	if record.Origin == nil {
		return nil
	}
	_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Task: id, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: record.Origin.Chat.ID(),
		ExecKind: "channel", ExecName: record.Origin.Channel, Recipient: initiator, DataOrigin: "code",
		Body: map[string]any{"form": "text_code", "text_key": "task.failed", "params": map[string]any{"reason": reason}},
	})
	return err
}

// endDelivery retries a delivery with a growing pause up to the farm ceiling, then marks it
// dead (DLV-3).
func (c *Core) endDelivery(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, l *leaseRow, reason string, retryable bool, retryAfterS int) (*FailResult, error) {
	var attempts int
	var kind string
	if err := tx.QueryRow(ctx, `SELECT attempts, kind FROM deliveries WHERE id = $1 FOR UPDATE`, l.WorkID).Scan(&attempts, &kind); err != nil {
		return nil, err
	}
	if retryable && attempts < c.cfg.DeliveryMaxAttempts {
		runAt := now.Add(backoff(manifest.Backoff{}, attempts))
		if ra := now.Add(time.Duration(retryAfterS) * time.Second); ra.After(runAt) {
			runAt = ra
		}
		if err := c.requeue(ctx, tx, now, l, runAt); err != nil {
			return nil, err
		}
		return &FailResult{RetryAt: fmtTime(runAt), Final: false}, nil
	}
	final := reason
	if retryable {
		final = "attempts_exhausted"
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', reason = $2, updated_at = $3 WHERE id = $1`, l.WorkID, final, now); err != nil {
		return nil, err
	}
	// A privacy notice that never arrived is planned again at the next contact; model text for
	// the recipient waits until then (DLV-12).
	if kind == "privacy_notice" {
		if _, err := tx.Exec(ctx, `UPDATE contacts ct SET privacy_version = NULL FROM deliveries d
			WHERE d.id = $1 AND ct.tenant = d.tenant AND ct.channel = d.exec_name AND ct.principal = d.recipient`, l.WorkID); err != nil {
			return nil, err
		}
	}
	if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.dead", Subject: l.WorkID, Data: map[string]any{"delivery": l.WorkID, "kind": kind, "reason": final}}); err != nil {
		return nil, err
	}
	return &FailResult{Final: true, Reason: final}, nil
}

// backoff is the pause before retry n (1-based): initial times factor^(n-1), capped by max.
// The defaults are 10 seconds, factor 2 and 5 minutes.
func backoff(b manifest.Backoff, n int) time.Duration {
	initial := durationOr(b.Initial, 10*time.Second)
	maxD := durationOr(b.Max, 5*time.Minute)
	factor := b.Factor
	if factor == 0 {
		factor = 2
	}
	if n < 1 {
		n = 1
	}
	d := time.Duration(float64(initial) * math.Pow(factor, float64(n-1)))
	if d > maxD || d <= 0 {
		d = maxD
	}
	return d
}

// releaseTaskReserve drops the enqueue reserve of a task that became terminal (BUD-1). A call
// reserve that was never charged stays counted at its full amount, marked estimated, like a
// call without an answer (BUD-11), so it no longer waits for a charge that will not come.
func releaseTaskReserve(ctx context.Context, tx pgx.Tx, task string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM spend WHERE root = (SELECT coalesce(record->>'root', id) FROM tasks WHERE id = $1) AND task = $1 AND unit = 'task' AND state = 'reserved'`, task); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE spend SET state = 'estimated' WHERE root = (SELECT coalesce(record->>'root', id) FROM tasks WHERE id = $1) AND task = $1 AND unit <> 'task' AND state = 'reserved'`, task)
	return err
}
