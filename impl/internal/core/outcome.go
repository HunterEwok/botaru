// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// HeartbeatRequest is p1-heartbeat-request.
type HeartbeatRequest struct {
	Health   []ComponentHealth `json:"health,omitempty"`
	Stage    string            `json:"stage,omitempty"`
	Progress *float64          `json:"progress,omitempty"`
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

// committed is a refusal whose transaction still commits: the outcome it reports is written
// (an attempt ended by output_schema_invalid, OUT-1).
type committed struct{ p *Problem }

func (e *committed) Error() string { return e.p.Error() }

// underLease runs fn under a checked lease; a repeat of an accepted call returns the stored
// answer without side effects (IDM-3). When fn returns a committed refusal, its writes are
// committed and the caller gets the refusal.
func underLease[T any](ctx context.Context, c *Core, w *Worker, header, op string, args any, fn func(tx pgx.Tx, now time.Time, l *leaseRow) (*T, error)) (*T, error) {
	var out *T
	var refusal *Problem
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		refusal = nil
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
		var cm *committed
		if errors.As(err, &cm) {
			out, refusal = nil, cm.p
			return nil
		}
		return err
	})
	if err == nil && refusal != nil {
		return nil, refusal
	}
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
		if w.Kind == "toolhost" && req.Health != nil {
			if err := c.pluginHealth(ctx, tx, now, w, req.Health, false); err != nil {
				return nil, err
			}
		}
		leaseS := c.cfg.LeaseS
		cancel := false
		if l.Kind == "attempt" {
			var typ, state string
			if err := tx.QueryRow(ctx, `SELECT type, state FROM tasks WHERE id = $1`, l.WorkID).Scan(&typ, &state); err != nil {
				return nil, err
			}
			cancel = state == "canceled"
			// A hold with cancel_running on the work of this attempt ends it now: the handles
			// are revoked, the attempt ends with held without being spent, and the task waits in
			// submitted, which claim does not hand out while the hold acts (HLD-3, FEN-4).
			if state == "working" {
				held, err := heldBy(ctx, tx, now, []string{"cancel_running"},
					append(workerScopes(w), scopeRef{kind: "tasktype", tenant: w.Tenant, name: typ})...)
				if err != nil {
					return nil, err
				}
				if held != "" {
					return c.endAttemptHeld(ctx, tx, now, w, l, held)
				}
			}
			if cat, err := c.Catalog(ctx); err == nil {
				if d, ok := parseDuration(cat.TaskTypes[typ].Timeouts.Lease); ok {
					leaseS = int(d / time.Second)
				}
			}
			leaseS = min(leaseS, maxLeaseS)
		}
		if l.Kind == "toolcall" {
			// The call was abandoned or ended: the toolhost cancels the plugin (FEN-6).
			var err error
			if cancel, err = toolcallCanceled(ctx, tx, l.WorkID); err != nil {
				return nil, err
			}
		}
		until := now.Add(time.Duration(leaseS) * time.Second)
		if _, err := tx.Exec(ctx, `UPDATE leases SET lease_until = $3, reason = NULL WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo, until); err != nil {
			return nil, err
		}
		// A change of the reported progress counts as progress against the stall (CNT-4).
		if req.Progress != nil && l.Kind == "attempt" {
			if _, err := tx.Exec(ctx, `UPDATE leases SET progress = $3, progress_at = $4 WHERE work_id = $1 AND lease_no = $2 AND progress IS DISTINCT FROM $3`,
				l.WorkID, l.LeaseNo, *req.Progress, now); err != nil {
				return nil, err
			}
		}
		return &HeartbeatResult{LeaseUntil: fmtTime(until), LeaseRemainingS: leaseS, CancelRequested: cancel}, nil
	})
}

// endAttemptHeld ends a running attempt under a cancel_running hold without spending it.
func (c *Core) endAttemptHeld(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, l *leaseRow, hold string) (*HeartbeatResult, error) {
	res := &HeartbeatResult{CancelRequested: true}
	if err := closeLease(ctx, tx, now, l, "revoked", "held", "heartbeat", nil, res); err != nil {
		return nil, err
	}
	if err := c.settleMessages(ctx, tx, now, l.WorkID); err != nil {
		return nil, err
	}
	if err := c.requeue(ctx, tx, now, l, now); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET reason = 'held' WHERE id = $1`, l.WorkID); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "system", Task: l.WorkID},
		Command: "attempt.cancel", Target: &Target{Kind: "task", ID: l.WorkID}, OK: true, Reason: "held",
		After: map[string]any{"lease_no": l.LeaseNo, "hold": hold, "spent": false},
	}); err != nil {
		return nil, err
	}
	return res, nil
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
				if p, ok := AsProblem(err); ok && p.Reason == "output_schema_invalid" {
					return nil, c.rejectResult(ctx, tx, now, w, l, p, req)
				}
				return nil, err
			}
		case "delivery":
			if req.Receipt == nil {
				return nil, refuse("input_schema_invalid", "a delivery completes with receipt")
			}
			if err := c.completeDelivery(ctx, tx, now, w, l, *req.Receipt); err != nil {
				return nil, err
			}
		case "toolcall":
			if req.Output == nil {
				return nil, refuse("input_schema_invalid", "a tool call completes with output")
			}
			if err := c.completeToolcall(ctx, tx, now, l, req.Output); err != nil {
				return nil, err
			}
		case "erasure":
			if req.Erasure == nil {
				return nil, refuse("input_schema_invalid", "an erasure completes with erasure")
			}
			if err := c.completeErasure(ctx, tx, now, l, req.Erasure); err != nil {
				return nil, err
			}
		default:
			return nil, refuse("input_schema_invalid", "this work kind is not served in v0.1")
		}
		return res, closeLease(ctx, tx, now, l, "closed", "", "complete", req, res)
	})
}

// personalSeen are the sources of the personal_seen flag in a task record (AUD-5).
func personalSeen(rec []byte) []string {
	var r struct {
		PersonalSeen []string `json:"personal_seen"`
	}
	_ = json.Unmarshal(rec, &r)
	return r.PersonalSeen
}

func (c *Core) completeAttempt(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, l *leaseRow, result map[string]any) error {
	// Only the core writes provenance; a result that brings its own is refused (PRV-2).
	if _, has := result["provenance"]; has {
		return refuse("output_schema_invalid", "a result carries no provenance: the core writes it")
	}
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
	att, err := loadAttempt(ctx, tx, l)
	if err != nil {
		return err
	}
	// The facts a type with memory.facts: propose may hand in sit beside its result; the core
	// takes them out before the result check and writes them below (MEM-6).
	var proposedFacts any
	if tt.Memory != nil && tt.Memory.Facts == "propose" {
		if f, ok := result["facts"]; ok {
			proposedFacts = f
			rest := make(map[string]any, len(result))
			for k, v := range result {
				if k != "facts" {
					rest[k] = v
				}
			}
			result = rest
		}
	}
	// The schema of the type at claim; a lease taken before the upgrade has none and uses the
	// current type.
	schema := att.ResultSchema
	if schema == nil {
		schema = tt.ResultSchema
	}
	// A lease taken before the upgrade has no attempt record: its enum_from values are
	// resolved now, against the current catalog and the task's mark.
	if names := (map[string]bool{}); att.EnumFrom == nil {
		enumFromNames(schema, names)
		if len(names) > 0 {
			var mark struct {
				DataMark string `json:"data_mark"`
			}
			if err := json.Unmarshal(rec, &mark); err != nil {
				return err
			}
			att.EnumFrom = c.resolveEnumFrom(cat, w.Tenant, mark.DataMark, names)
		}
	}
	// The full result check (OUT-1, D-98): enum_from against the catalog values fixed for this
	// attempt and filtered by the task's mark, and x-botaru-format as an assertion.
	if schema != nil {
		full, err := c.fullSchema(schema, att.EnumFrom)
		if err != nil {
			return err
		}
		if err := c.rs.validate(full, result); err != nil {
			return refuse("output_schema_invalid", "the result does not match the full result_schema")
		}
	}
	// An analysis or aggregator result is proposals or nothing_found (INS-1).
	var bodies []map[string]any
	if p := tt.Purpose; p == "insight" || p == "aggregate" {
		if bodies, err = insightBodies(result); err != nil {
			return err
		}
	}
	// A reading of the farm manager hands in only findings, which the core checks and stores
	// (MGR-25).
	if tt.Purpose == "inspect" {
		if err := c.takeFindings(ctx, tx, now, cat, w.Tenant, l, rec, result); err != nil {
			return err
		}
	}
	// A lens review hands in review-result for its own lens and never as a shadow, which only
	// the shadow executor writes (COM-3, COM-9).
	if tt.Purpose == "review" {
		var lens struct {
			Lens string `json:"lens"`
		}
		if err := json.Unmarshal(rec, &lens); err != nil {
			return err
		}
		if c.spec.Validate("urn:botaru:schema:review-result:1", result) != nil || result["lens"] != lens.Lens || result["shadow"] == true {
			return refuse("output_schema_invalid", "the result is not a review-result of the task's lens")
		}
	}
	// A translator hands in only the closed translation-result (TRN-3).
	if tt.Purpose == "translate" {
		if err := c.checkTranslation(ctx, tx, w.Tenant, l.WorkID, result); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'completed', result = $2, updated_at = $3 WHERE id = $1`, l.WorkID, raw, now); err != nil {
		return err
	}
	// Proposals are born only here, in the transaction of a successful complete (INS-3).
	if err := c.createProposals(ctx, tx, now, cat, w, l.WorkID, typ, rec, bodies); err != nil {
		return err
	}
	// So are facts: a failed, canceled or fenced attempt leaves none (MEM-6).
	if proposedFacts != nil {
		var recMap map[string]any
		if err := json.Unmarshal(rec, &recMap); err != nil {
			return err
		}
		if err := c.proposeFacts(ctx, tx, now, cat, w, l, recMap, proposedFacts); err != nil {
			return err
		}
	}
	// What the attempt staged is published and what it read acknowledged, in this transaction
	// (MSG-9).
	if err := c.publishMessages(ctx, tx, now, cat, w.Tenant, l.WorkID, l.LeaseNo, w.ID); err != nil {
		return err
	}
	if err := releaseTaskReserve(ctx, tx, l.WorkID); err != nil {
		return err
	}
	if err := c.autoReports(ctx, tx, now, w.Tenant, l.WorkID); err != nil {
		return err
	}
	data := map[string]any{"task": l.WorkID}
	if att.Fingerprint != "" {
		data["fingerprint"], data["fingerprint_parts"] = att.Fingerprint, att.Parts
	}
	if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "task.completed", Subject: l.WorkID, Data: data}); err != nil {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "bot", Role: w.Role, Task: l.WorkID},
		Command: "task.complete", Target: &Target{Kind: "task", ID: l.WorkID}, OK: true,
		After: map[string]any{"state": "completed", "worker": w.ID}, PersonalSeen: personalSeen(rec),
	}); err != nil {
		return err
	}
	// A child's end enqueues the reply continuation of its parent (CNT-1, CNT-2).
	if err := c.replyContinuation(ctx, tx, now, w.Tenant, l.WorkID, "completed"); err != nil {
		return err
	}
	if err := c.campaignItemEnded(ctx, tx, now, l.WorkID, "completed", ""); err != nil {
		return err
	}
	if err := c.scriptTaskEnded(ctx, tx, now, l.WorkID, "completed", ""); err != nil {
		return err
	}
	// The answer goes to the task origin; the address comes from the record, never from the
	// result (DLV-6). A child answers its parent, not the person.
	var record struct {
		treeRecord
		DataMark string `json:"data_mark"`
	}
	if err := json.Unmarshal(rec, &record); err != nil {
		return err
	}
	// A reporter's answer is a digest the code builds (REP-2, REP-9).
	if purposeOf(tt) == "report" && !record.child() {
		return c.deliverDigest(ctx, tx, now, cat, w.Tenant, l.WorkID, rec, result)
	}
	// A task of a schedule's tree reports to the schedule's addresses where a task with an
	// origin would report to it: the root and its continuations, never a child (DLV-6, CNT-1).
	if name, err := scheduleOf(ctx, tx, record.treeRecord, l.WorkID); err != nil {
		return err
	} else if name != "" {
		if record.child() {
			return nil
		}
		return c.deliverScheduled(ctx, tx, now, cat, w.Tenant, l.WorkID, name, tt, result, record.DataMark)
	}
	if !record.reportsToOrigin() {
		return nil
	}
	// The follow-up window is for a direct chat: in a group the answer goes privately, where a
	// plain reply does not continue the group's task (CNT-5).
	var window time.Duration
	if record.Origin.Chat.Kind != "group" {
		if window, err = c.openFollowup(ctx, tx, now, l.WorkID, tt, result); err != nil {
			return err
		}
	}
	text, fields := resultText(result, tt.SummaryFields)
	dataOrigin := "model"
	// The answer to a translator task is a code text, not model output (TRN-5).
	if tt.Purpose == "translate" {
		if text, err = c.translatorAnswer(ctx, tx, now, cat, w.Tenant, w.Role, initiator, result); err != nil {
			return err
		}
		fields, dataOrigin = nil, "code"
	}
	if window > 0 {
		// The code text under the bot's question says the answer can simply be written (CNT-5).
		var locale *string
		if err := tx.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, w.Tenant, initiator).Scan(&locale); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		text += "\n\n" + c.texts.Render(deref(locale), "cmd.followup.hint", map[string]any{"minutes": int(window / time.Minute)})
	}
	body := map[string]any{"form": "result", "text": text}
	if len(fields) > 0 {
		body["fields"] = fields
	}
	if record.Origin.Chat.Kind == "group" {
		if tv := cat.Tenants[w.Tenant]; tv != nil {
			return c.planGroupAnswer(ctx, tx, now, tv, l.WorkID, initiator, record.Origin, tt, result, body, record.DataMark)
		}
		return nil
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: w.Tenant, Task: l.WorkID, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: record.Origin.Chat.ID(),
		ExecKind: "channel", ExecName: record.Origin.Channel, Recipient: initiator, Body: body,
		DataMark: record.DataMark, DataOrigin: dataOrigin,
	})
	return err
}

// rejectResult ends an attempt whose result failed the full check (OUT-1, D-98): the attempt
// is spent and the retry follows the type's backoff, as the reason registry says for
// output_schema_invalid. The lease closes with the refusal stored as its outcome: a repeat of
// the same complete gets the same refusal (IDM-3), and any other call under the lease is
// fenced, so the holder cannot hand in another result. The writes are committed and the holder
// gets the refusal.
func (c *Core) rejectResult(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, l *leaseRow, p *Problem, args any) error {
	reasons, err := c.spec.Registry("spec/model/reasons.yaml")
	if err != nil {
		return err
	}
	entry, _ := reasons.Entry(p.Reason)
	spends, ok := entry["spends_attempt"].(bool)
	if !ok {
		spends = true
	}
	mode, _ := entry["retry"].(string)
	if _, err := c.endAttempt(ctx, tx, now, w.Tenant, l, p.Reason, spends, mode != "none", 0, mode); err != nil {
		return err
	}
	h, err := jcs.Hash(argsValue(args))
	if err != nil {
		return err
	}
	outcome, err := json.Marshal(map[string]any{"op": "complete", "args": h, "problem": map[string]any{"reason": p.Reason, "detail": p.Detail}})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'closed', reason = $3, outcome = $4, closed_at = $5 WHERE work_id = $1 AND lease_no = $2`,
		l.WorkID, l.LeaseNo, p.Reason, outcome, now); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo); err != nil {
		return err
	}
	if err := c.settleMessages(ctx, tx, now, l.WorkID); err != nil {
		return err
	}
	// The rejected result carried the same personal data as an accepted one would (AUD-5).
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, l.WorkID).Scan(&rec); err != nil {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "bot", Role: w.Role, Task: l.WorkID},
		Command: "task.complete", Target: &Target{Kind: "task", ID: l.WorkID}, OK: false, Reason: p.Reason, PersonalSeen: personalSeen(rec),
	}); err != nil {
		return err
	}
	return &committed{p: p}
}

func (c *Core) completeDelivery(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, l *leaseRow, r Receipt) error {
	var kind string
	if err := tx.QueryRow(ctx, `UPDATE deliveries SET state = 'delivered', external_id = $2, updated_at = $3 WHERE id = $1 RETURNING kind`,
		l.WorkID, nullable(r.ExternalID), now).Scan(&kind); err != nil {
		return err
	}
	if kind == "monitor_alarm" || kind == "monitor_audit_head" {
		if err := c.monitorExported(ctx, tx, now, w.Tenant, kind); err != nil {
			return err
		}
	}
	if kind == "monitor_script" {
		if err := c.scriptDone(ctx, tx, now, l.WorkID, r); err != nil {
			return err
		}
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
		case "toolcall":
			res, err = &FailResult{Final: true, Reason: req.Reason}, c.failToolcall(ctx, tx, now, l, req.Reason)
		case "erasure":
			res = &FailResult{Final: !req.Retryable, Reason: req.Reason}
			err = c.endErasureStep(ctx, tx, now, l, "failed", "", req.Reason, req.Retryable)
		default:
			return nil, refuse("input_schema_invalid", "this work kind is not served in v0.1")
		}
		if err != nil {
			return nil, err
		}
		if err := closeLease(ctx, tx, now, l, "closed", req.Reason, "fail", req, res); err != nil {
			return nil, err
		}
		// What the attempt staged is discarded, what it read returns (MSG-9).
		return res, c.settleMessages(ctx, tx, now, l.WorkID)
	})
}

// Release returns the work to the queue without spending an attempt (worker_drain).
func (c *Core) Release(ctx context.Context, w *Worker, header string) (*ReleaseResult, error) {
	return underLease(ctx, c, w, header, "release", nil, func(tx pgx.Tx, now time.Time, l *leaseRow) (*ReleaseResult, error) {
		if err := c.requeue(ctx, tx, now, l, now); err != nil {
			return nil, err
		}
		res := &ReleaseResult{LeaseState: "released"}
		if err := closeLease(ctx, tx, now, l, "released", "worker_drain", "release", nil, res); err != nil {
			return nil, err
		}
		return res, c.settleMessages(ctx, tx, now, l.WorkID)
	})
}

// requeue puts an attempt or a delivery back to the queue from run_at on.
func (c *Core) requeue(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, runAt time.Time) error {
	var err error
	switch l.Kind {
	case "attempt":
		_, err = tx.Exec(ctx, `UPDATE tasks SET state = 'submitted', run_at = $2, updated_at = $3 WHERE id = $1 AND state = 'working'`, l.WorkID, runAt, now)
	case "delivery":
		var kind string
		if err := tx.QueryRow(ctx, `SELECT kind FROM deliveries WHERE id = $1`, l.WorkID).Scan(&kind); err != nil {
			return err
		}
		if kind == "monitor_script" {
			// A script is handed out once (MON-10): a released run ends, as a released call does.
			// The adapter may have run it before releasing, so nobody knows whether it ran (APR-7).
			if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', reason = 'worker_drain', updated_at = $2 WHERE id = $1 AND state = 'sending'`, l.WorkID, now); err != nil {
				return err
			}
			if err := c.scriptFailed(ctx, tx, now, l.WorkID, "worker_drain"); err != nil {
				return err
			}
			if err := c.appendEvent(ctx, tx, now, l.Tenant, Event{Type: "delivery.dead", Subject: l.WorkID,
				Data: map[string]any{"delivery": l.WorkID, "kind": kind, "reason": "worker_drain"}}); err != nil {
				return err
			}
			return notifyWork(ctx, tx)
		}
		_, err = tx.Exec(ctx, `UPDATE deliveries SET state = 'planned', run_at = $2, updated_at = $3 WHERE id = $1 AND state = 'sending'`, l.WorkID, runAt, now)
	case "toolcall":
		// A call is not handed out again (DLV-1): a released call ends, and the model decides.
		_, err = tx.Exec(ctx, `UPDATE toolcalls SET state = 'error', reason = 'worker_drain', updated_at = $2, ended_at = $2 WHERE id = $1 AND state = 'leased'`, l.WorkID, now)
	case "erasure":
		_, err = tx.Exec(ctx, `UPDATE erasure_steps SET state = 'pending', run_at = $2, updated_at = $3 WHERE id = $1 AND state = 'leased'`, l.WorkID, runAt, now)
		if err == nil {
			_, err = tx.Exec(ctx, `UPDATE subject_match_steps SET state = 'pending', run_at = $2, updated_at = $3 WHERE id = $1 AND state = 'leased'`, l.WorkID, runAt, now)
		}
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
	// A run counts when the model answered during this attempt; retry.max_runs caps them. An
	// error answer of the provider leaves no token counts, so it is not a run.
	var answered bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM spend s JOIN leases l ON l.work_id = s.task
		WHERE s.task = $1 AND l.lease_no = $2 AND s.unit = 'tokens' AND s.state = 'committed' AND s.tokens IS NOT NULL AND s.time >= l.created_at)`,
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
	// The task keeps the last provider reason, also into failed(deadline_exceeded) (CAP-2,
	// GW-13).
	if strings.HasPrefix(reason, "provider_") {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('provider_reason', $2::text) WHERE id = $1`, l.WorkID, reason); err != nil {
			return nil, err
		}
		rec, _ = jsonSet(rec, "provider_reason", reason)
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
			// A task that waits for the provider says so; its reserve does not count toward
			// the 80% threshold (BUD-4, D18, D19).
			if wait := providerWaiting(reason); wait != "" {
				if _, err := tx.Exec(ctx, `UPDATE tasks SET reason = $2 WHERE id = $1 AND state = 'submitted'`, l.WorkID, wait); err != nil {
					return nil, err
				}
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

// providerWaiting is the waiting reason of a task after a provider reason, or "".
func providerWaiting(reason string) string {
	switch reason {
	case "provider_unavailable":
		return "provider_unavailable"
	case "provider_rate_limited", "provider_quota":
		return "provider_quota"
	}
	return ""
}

// jsonSet sets one field of a JSON object.
func jsonSet(raw []byte, key string, v any) ([]byte, error) {
	m := map[string]any{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw, err
	}
	m[key] = v
	return json.Marshal(m)
}

// failTask moves a task to failed and tells the initiator by a code text at the origin.
func (c *Core) failTask(ctx context.Context, tx pgx.Tx, now time.Time, tenant, id, initiator string, rec []byte, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'failed', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
		return err
	}
	if err := releaseTaskReserve(ctx, tx, id); err != nil {
		return err
	}
	if err := c.autoReports(ctx, tx, now, tenant, id); err != nil {
		return err
	}
	data := map[string]any{"task": id, "reason": reason}
	var last struct {
		Provider string `json:"provider_reason"`
	}
	if json.Unmarshal(rec, &last) == nil && last.Provider != "" && reason == "deadline_exceeded" {
		data["provider_reason"] = last.Provider
	}
	if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "task.failed", Subject: id, Data: data}); err != nil {
		return err
	}
	if err := c.replyContinuation(ctx, tx, now, tenant, id, "failed"); err != nil {
		return err
	}
	if err := c.campaignItemEnded(ctx, tx, now, id, "failed", reason); err != nil {
		return err
	}
	if err := c.scriptTaskEnded(ctx, tx, now, id, "failed", reason); err != nil {
		return err
	}
	// A draft of a translator that failed afterwards is not started any more.
	if err := c.cancelRunsOf(ctx, tx, now, tenant, id, "expired"); err != nil {
		return err
	}
	var record treeRecord
	if err := json.Unmarshal(rec, &record); err != nil {
		return err
	}
	if !record.reportsToOrigin() {
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
	var kind, channel, targetID string
	var task, fallback *string
	var episodeOpen bool
	if err := tx.QueryRow(ctx, `SELECT d.attempts, d.kind, d.exec_name, d.target_id, d.task, d.fallback_chat, coalesce(e.state = 'firing', false) FROM deliveries d
		LEFT JOIN alarm_episodes e ON e.id = d.episode WHERE d.id = $1 FOR UPDATE OF d`, l.WorkID).Scan(&attempts, &kind, &channel, &targetID, &task, &fallback, &episodeOpen); err != nil {
		return nil, err
	}
	// A script runs at most once: any end of its delivery ends the call, by the reason (MON-10,
	// APR-7).
	if kind == "monitor_script" {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'dead', reason = $2, updated_at = $3 WHERE id = $1`, l.WorkID, reason, now); err != nil {
			return nil, err
		}
		if err := c.scriptFailed(ctx, tx, now, l.WorkID, reason); err != nil {
			return nil, err
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.dead", Subject: l.WorkID, Data: map[string]any{"delivery": l.WorkID, "kind": kind, "reason": reason}}); err != nil {
			return nil, err
		}
		return &FailResult{Final: true, Reason: reason}, nil
	}
	// The channel saw an unknown member in a group chat: the delivery is withheld and the chat
	// gets nothing more until the operator binds it again (DLV-7).
	if reason == "unknown_member" && strings.HasPrefix(targetID, "group:") {
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $2, updated_at = $3 WHERE id = $1`, l.WorkID, reason, now); err != nil {
			return nil, err
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.withheld", Subject: l.WorkID, Data: map[string]any{"delivery": l.WorkID, "kind": kind, "reason": reason}}); err != nil {
			return nil, err
		}
		if kind == "privacy_notice" {
			if err := resetPrivacyNotice(ctx, tx, l.WorkID); err != nil {
				return nil, err
			}
		}
		b, err := loadBinding(ctx, tx, tenant, channel, targetID)
		if err != nil {
			return nil, err
		}
		if b != nil {
			if err := c.markMembersUnknown(ctx, tx, now, b); err != nil {
				return nil, err
			}
		}
		return &FailResult{Final: true, Reason: reason}, nil
	}
	// The export of an alarm change or of the audit head does not die by attempt count: it is
	// retried with a pause of at most 10 minutes until it is exported (MON-11).
	// A newer change of the same alarm replaces this one, so it is not retried.
	// A refusal the adapter calls final is not a failure of the monitoring system: it goes the
	// common way, by attempts, and does not set off a re-export.
	export := retryable && (kind == "monitor_alarm" || kind == "monitor_audit_head")
	if export {
		if err := monitorFailed(ctx, tx, now, tenant); err != nil {
			return nil, err
		}
		if superseded, err := exportSuperseded(ctx, tx, now, l.WorkID, kind); err != nil || superseded {
			if err != nil {
				return nil, err
			}
			return &FailResult{Final: true, Reason: "superseded"}, nil
		}
	}
	// An alarm notice does not die by attempt count while its episode is open; it is retried
	// with a pause of at most 10 minutes (MON-13, like the export of MON-11).
	if export || retryable && kind == "alarm_notice" && episodeOpen {
		runAt := now.Add(min(backoff(manifest.Backoff{}, attempts), 10*time.Minute))
		if ra := now.Add(time.Duration(retryAfterS) * time.Second); ra.After(runAt) {
			runAt = ra
		}
		if err := c.requeue(ctx, tx, now, l, runAt); err != nil {
			return nil, err
		}
		return &FailResult{RetryAt: fmtTime(runAt), Final: false}, nil
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
	// The direct message of a split group answer never arrived: the group asks the person to
	// open a private chat with the bot, and the content does not go to the group (DLV-15).
	if fallback != nil {
		if err := c.planOpenPrivateChat(ctx, tx, now, tenant, deref(task), *fallback, channel); err != nil {
			return nil, err
		}
	}
	// A privacy notice that never arrived is planned again at the next contact; model text for
	// the recipient waits until then (DLV-12).
	if kind == "privacy_notice" {
		if err := resetPrivacyNotice(ctx, tx, l.WorkID); err != nil {
			return nil, err
		}
	}
	// Model text that waited for a dead ai_notice is withheld; the next contact gets a new
	// notice, but not these stale results (DLV-12, ai_notice_dead).
	if kind == "ai_notice" {
		rows, err := tx.Query(ctx, `UPDATE deliveries m SET state = 'withheld', reason = 'ai_notice_dead', updated_at = $2 FROM deliveries d
			WHERE d.id = $1 AND m.tenant = d.tenant AND m.exec_name = d.exec_name AND m.recipient = d.recipient
			AND m.dataorigin = 'model' AND m.state = 'planned' RETURNING m.id, m.kind, m.task, m.fallback_chat`, l.WorkID, now)
		if err != nil {
			return nil, err
		}
		var held [][2]string
		var asks [][2]string
		for rows.Next() {
			var id, k string
			var t, fb *string
			if err := rows.Scan(&id, &k, &t, &fb); err != nil {
				rows.Close()
				return nil, err
			}
			held = append(held, [2]string{id, k})
			if fb != nil {
				asks = append(asks, [2]string{deref(t), *fb})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
		for _, h := range held {
			if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "delivery.withheld", Subject: h[0], Data: map[string]any{"delivery": h[0], "kind": h[1], "reason": "ai_notice_dead"}}); err != nil {
				return nil, err
			}
		}
		// The direct half of a split group answer cannot reach the person: the group asks them
		// to open a private chat with the bot (DLV-15).
		for _, a := range asks {
			if err := c.planOpenPrivateChat(ctx, tx, now, tenant, a[0], a[1], channel); err != nil {
				return nil, err
			}
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
// call without an answer (BUD-11), so it no longer waits for a charge that will not come. The
// reserve of a tool call settles when the call ends.
func releaseTaskReserve(ctx context.Context, tx pgx.Tx, task string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM spend WHERE root = (SELECT coalesce(record->>'root', id) FROM tasks WHERE id = $1) AND task = $1 AND unit = 'task' AND state = 'reserved'`, task); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE spend SET state = 'estimated' WHERE root = (SELECT coalesce(record->>'root', id) FROM tasks WHERE id = $1) AND task = $1 AND unit <> 'task' AND toolcall IS NULL AND state = 'reserved'`, task)
	return err
}

// resetPrivacyNotice clears the notice version of the recipient of a privacy notice that never
// arrived, so the next contact plans it again (DLV-12).
func resetPrivacyNotice(ctx context.Context, tx pgx.Tx, delivery string) error {
	_, err := tx.Exec(ctx, `UPDATE contacts ct SET privacy_version = NULL FROM deliveries d
		WHERE d.id = $1 AND ct.tenant = d.tenant AND ct.channel = d.exec_name AND ct.principal = d.recipient`, delivery)
	return err
}

// planOpenPrivateChat asks a group chat to open a private chat with the bot for the answer to
// a task; the content does not go to the group (DLV-15).
func (c *Core) planOpenPrivateChat(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task, group, channel string) error {
	_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Task: task, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: group,
		ExecKind: "channel", ExecName: channel, DataOrigin: "code",
		Body: map[string]any{"form": "text_code", "text_key": "delivery.open_private_chat"},
	})
	return err
}
