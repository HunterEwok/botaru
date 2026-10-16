// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// ClaimRequest is p1-claim-request.
type ClaimRequest struct {
	Kinds []string `json:"kinds"`
	WaitS *int     `json:"wait_s,omitempty"`
}

// WorkItem is the work-item schema.
type WorkItem struct {
	WorkID          string         `json:"work_id"`
	Kind            string         `json:"kind"`
	LeaseNo         int            `json:"lease_no"`
	Epoch           int64          `json:"epoch"`
	Tenant          string         `json:"tenant"`
	LeaseUntil      string         `json:"lease_until"`
	LeaseRemainingS int            `json:"lease_remaining_s"`
	StartBy         string         `json:"start_by,omitempty"`
	Token           LeaseToken     `json:"token"`
	Payload         map[string]any `json:"payload"`
}

// ClaimResult is p1-claim-response.
type ClaimResult struct {
	Work *WorkItem `json:"work"`
}

// maxLeaseS is the ceiling of a type lease (task-type schema, timeouts.lease).
const maxLeaseS = 120

// Claim hands out the next work item with long polling (DSP-1, FEN-1). The core decides what
// to hand out; the worker names only the kinds it takes.
func (c *Core) Claim(ctx context.Context, w *Worker, req ClaimRequest) (*ClaimResult, error) {
	switch w.State {
	case "draining":
		return nil, refuse("draining", "the worker is draining")
	case "blocked":
		return nil, refuse("blocked", "the worker is blocked")
	case "active":
	default:
		return nil, refuse("not_ready", "send hello first")
	}
	// A draining core hands out no new attempt until the next core starts; tool calls,
	// deliveries and erasure work of running attempts go on (LCY-6).
	if until, err := drainUntil(ctx, c.st.Pool); err != nil {
		return nil, err
	} else if until != nil && !slices.ContainsFunc(req.Kinds, func(k string) bool { return k != "attempt" }) {
		p := refuse("not_ready", "the core is draining before a rollout")
		p.RetryAfterS = 30
		return nil, p
	}
	wait := 30
	if req.WaitS != nil {
		wait = *req.WaitS
	}
	if wait > 60 {
		wait = 60
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		woken := c.wake.wait()
		item, err := c.claimOnce(ctx, w, req.Kinds)
		if err != nil || item != nil {
			return &ClaimResult{Work: item}, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return &ClaimResult{}, nil
		}
		// A lost notification costs at most this pause.
		poll := min(left, 2*time.Second)
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-woken:
		case <-t.C:
		}
		t.Stop()
	}
}

func (c *Core) claimOnce(ctx context.Context, w *Worker, kinds []string) (*WorkItem, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var item *WorkItem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		item = nil
		// A worker revoked during the long poll gets no more work: the row lock orders this with
		// worker.revoke, and a revoked worker never gets a new identity (IDN-8).
		if tag, err := tx.Exec(ctx, `UPDATE workers SET last_seen = $2 WHERE id = $1 AND state <> 'revoked'`, w.ID, now); err != nil {
			return err
		} else if tag.RowsAffected() == 0 {
			return refuse("unauthenticated", "the worker is revoked")
		}
		until, err := drainUntil(ctx, tx)
		if err != nil {
			return err
		}
		for _, k := range kinds {
			if k == "attempt" && until != nil {
				continue
			}
			var err error
			switch {
			case k == "delivery" && (w.Kind == "channel" || w.Kind == "sink" || w.Kind == "monitor"):
				item, err = c.claimDelivery(ctx, tx, now, cat, w)
			case k == "attempt" && (w.Kind == "agent" || w.Kind == "job"):
				item, err = c.claimAttempt(ctx, tx, now, cat, w)
			case k == "toolcall" && w.Kind == "toolhost":
				item, err = c.claimToolcall(ctx, tx, now, cat, w)
			case k == "erasure" && (w.Kind == "sink" || w.Kind == "monitor"):
				item, err = c.claimErasure(ctx, tx, now, w)
			}
			if err != nil || item != nil {
				return err
			}
		}
		return nil
	})
	return item, err
}

func (c *Core) newLease(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, kind, workID string, leaseNo, leaseS int, runID string, acting ...actingOf) (*WorkItem, error) {
	until := now.Add(time.Duration(leaseS) * time.Second)
	var a actingOf
	if len(acting) > 0 {
		a = acting[0]
	}
	var be, se any
	if a.Bot != "" {
		be = a.BotEpoch
	}
	if a.Seat != "" {
		se = a.SeatEpoch
	}
	if _, err := tx.Exec(ctx, `INSERT INTO leases (work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until, run_id, created_at, acting_bot, bot_epoch, seat, seat_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9, $10, $11, $12, $13)`,
		workID, leaseNo, kind, w.Tenant, w.ID, c.epoch, until, nullable(runID), now, nullable(a.Bot), be, nullable(a.Seat), se); err != nil {
		return nil, err
	}
	tok, err := c.issueToken(c.epoch, workID, leaseNo, w.ID, a)
	if err != nil {
		return nil, err
	}
	return &WorkItem{
		WorkID: workID, Kind: kind, LeaseNo: leaseNo, Epoch: c.epoch, Tenant: w.Tenant,
		LeaseUntil: fmtTime(until), LeaseRemainingS: leaseS, Token: tok,
	}, nil
}

// claimAttempt takes the oldest task of the highest priority class among the types the role
// claims (DSP-2) and writes the lease and the transition to working in one transaction.
// accessLost rechecks a principal: active, with the capability cap (none when empty) and the
// data class mark. It returns the refusal reason, or "" when the access still holds.
func (c *Core) accessLost(ctx context.Context, tx store.Querier, now time.Time, tv *TenantView, principal, cap, mark string) (string, error) {
	if tv == nil {
		return "", nil
	}
	if principal == seatConsent {
		// A farm manager seat task answers to the tenant's consent group as it stands now: a
		// right the tenant took back after enqueue stops it (MGR-3).
		groups := groupClosure(tv, []string{"farm-manager-ops"})
		if cap != "" && !hasCap(tv, groups, cap) {
			return "no_group_cap", nil
		}
		if mark != "" && !hasData(tv, groups, mark) {
			return "no_data_class", nil
		}
		return "", nil
	}
	if principal == "system" {
		return "", nil
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return "person_inactive", nil
	}
	if err != nil {
		return "", err
	}
	if p.State != "active" {
		return "person_inactive", nil
	}
	groups := groupClosure(tv, p.Groups)
	if cap != "" && !hasCap(tv, groups, cap) {
		return "no_group_cap", nil
	}
	if mark != "" && !hasData(tv, groups, mark) {
		return "no_data_class", nil
	}
	return "", nil
}

// seatConsent stands for the tenant's consent group farm-manager-ops in accessLost.
const seatConsent = "seat:" + managerSeatID

// accessPrincipal is whose rights a task's work answers to: the farm manager seat's tasks to the
// tenant's consent group, any other task to its initiator.
func accessPrincipal(initiator string, rec []byte) string {
	if seatOfRecord(rec) == managerSeatID {
		return seatConsent
	}
	return initiator
}

// taskMark is the data mark a task's reader must see: the type's class, raised by the mark in
// the task record (a session, a file or an attachment, ART-2, OUT-1).
func (c *Core) taskMark(rec []byte, class string) string {
	var r struct {
		DataMark string `json:"data_mark"`
	}
	_ = json.Unmarshal(rec, &r)
	return c.higherMark(class, r.DataMark)
}

// revokeWorkOf ends the tasks a principal has in work once their access no longer covers
// them: the lease and handles go in the same transaction, so the worker is fenced at its next
// call and no more task data reaches the model in their name (PRM-8, FEN-4).
func (c *Core) revokeWorkOf(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, principal string) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	type working struct {
		id, typ string
		rec     []byte
	}
	// An action task is not stopped: its write call may already run; only a pending approval
	// is canceled (APR-6).
	rows, err := tx.Query(ctx, `SELECT id, type, record FROM tasks WHERE tenant = $1 AND initiator = $2 AND state = 'working' AND type NOT LIKE 'action.%' ORDER BY id FOR UPDATE`, tv.Name, principal)
	if err != nil {
		return err
	}
	var ws []working
	for rows.Next() {
		var w working
		if err := rows.Scan(&w.id, &w.typ, &w.rec); err != nil {
			rows.Close()
			return err
		}
		ws = append(ws, w)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, w := range ws {
		reason, err := c.accessLost(ctx, tx, now, tv, principal, "task:"+w.typ, c.taskMark(w.rec, cat.TaskTypes[w.typ].DataClass))
		if err != nil {
			return err
		}
		if reason == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `DELETE FROM handles WHERE work_id = $1`, w.id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE leases SET state = 'revoked', reason = $3, closed_at = $2 WHERE work_id = $1 AND state = 'active'`, w.id, now, reason); err != nil {
			return err
		}
		if err := c.settleMessages(ctx, tx, now, w.id); err != nil {
			return err
		}
		if err := c.failTask(ctx, tx, now, tv.Name, w.id, principal, w.rec, reason); err != nil {
			return err
		}
	}
	return c.recheckApprovalsOf(ctx, tx, now, tv, principal)
}

// hasLabels says whether a worker has every label a task type requires.
func hasLabels(labels, requires map[string]string) bool {
	for k, v := range requires {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func (c *Core) claimAttempt(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker) (*WorkItem, error) {
	role, ok := cat.Roles[w.Role]
	if !ok || role.WorkerKind != w.Kind {
		return nil, nil
	}
	// Only the claimed types that this worker kind runs, by the labels the running process
	// reported at hello (it may narrow the enrolled set).
	labels := w.Labels
	if w.Hello != nil {
		labels = w.Hello.Labels
	}
	var claims []string
	for _, t := range role.Claims {
		if tt, ok := cat.TaskTypes[t]; ok && tt.WorkerKind == w.Kind && hasLabels(labels, tt.Requires) {
			claims = append(claims, t)
		}
	}
	// A hold with no_claim or cancel_running on the farm, the tenant, the role or this worker
	// gives it no work; one on a type takes that type out of the claim (HLD-3).
	stop := []string{"no_claim", "cancel_running"}
	// The worker's own identity; a suspended one gets no tasks (IDN-8).
	own, err := c.workerBot(ctx, tx, now, cat, w)
	if err != nil {
		return nil, err
	}
	if own != nil && own.State != "active" {
		return nil, nil
	}
	scopes := workerScopes(w)
	if own != nil {
		scopes = append(scopes, scopeRef{kind: "bot", tenant: w.Tenant, name: own.ID})
	}
	if held, err := heldBy(ctx, tx, now, stop, scopes...); err != nil || held != "" {
		return nil, err
	}
	open := claims[:0:0]
	for _, t := range claims {
		held, err := heldBy(ctx, tx, now, stop, scopeRef{kind: "tasktype", tenant: w.Tenant, name: t})
		if err != nil {
			return nil, err
		}
		if held == "" {
			open = append(open, t)
		}
	}
	claims = open
	if len(claims) == 0 {
		return nil, nil
	}
	var (
		id, typ   string
		body, rec []byte
		leaseNo   int
		initiator string
	)
	tv := cat.Tenants[w.Tenant]
	// A pair (workspace, model pool) that waits for the provider gets no attempt, and the
	// runtime is not started for it (D18, D19, GW-10, GW-11).
	if wait, err := pairWait(ctx, tx, now, cat, w.Tenant, role); err != nil {
		return nil, err
	} else if wait != "" {
		return nil, markWaiting(ctx, tx, now, w.Tenant, claims, nil, wait)
	}
	quotas := tv != nil && hasRunningCeiling(tv.Spec.Dispatch)
	if quotas {
		// One dispatcher decision at a time per tenant, so two claims never both see the last
		// free place under a ceiling.
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300103, hashtext($1))`, w.Tenant); err != nil {
			return nil, err
		}
	}
	skip := []string{}
	var acting actingOf
	if own != nil {
		acting = actingOf{Bot: own.ID, BotEpoch: own.Epoch}
	}
	// A lens review goes only to its executor role and never to the author's worker (COM-2).
	for picked := false; !picked; {
		err := tx.QueryRow(ctx, `SELECT id, type, body, record, lease_no, initiator FROM tasks
			WHERE tenant = $1 AND state = 'submitted' AND body IS NOT NULL AND type = ANY($2) AND (run_at IS NULL OR run_at <= $3) AND (deadline_at IS NULL OR deadline_at > $3)
			AND NOT id = ANY($4)
			AND (record->>'executor_role' IS NULL OR record->>'executor_role' = $5) AND (record->>'not_worker' IS NULL OR record->>'not_worker' <> $6)
			ORDER BY priority_class, created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, claims, now, skip, w.Role, w.ID).
			Scan(&id, &typ, &body, &rec, &leaseNo, &initiator)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// A seat task runs on behalf of the seat's holder with the current seat epoch; a
		// vacant seat, or one whose holder is stopped, gets no tasks (IDN-10, IDN-12).
		if seat := seatOfRecord(rec); seat != "" {
			// A reading runs beside the decision attempt that asked for it (MGR-25).
			a, ok, err := c.seatActing(ctx, tx, now, cat, w.Tenant, seat, seat == managerSeatID && cat.TaskTypes[typ].Purpose == "inspect")
			if err != nil {
				return nil, err
			}
			if !ok || (a.Bot != "" && a.Bot == notBotOfRecord(rec)) {
				skip = append(skip, id)
				continue
			}
			// The seat's tasks of a production tenant wait until the seat model profile has
			// passed the mgr-inj set (MGR-23).
			if seat == managerSeatID && productionTenant(tv) {
				if wait, err := c.seatEvalWait(ctx, tx, now, cat, w.Tenant, role); err != nil {
					return nil, err
				} else if wait {
					if err := markWaiting(ctx, tx, now, w.Tenant, nil, []string{id}, "seat_eval_missing"); err != nil {
						return nil, err
					}
					skip = append(skip, id)
					continue
				}
			}
			acting = a
		} else if own != nil {
			acting = actingOf{Bot: own.ID, BotEpoch: own.Epoch}
		} else {
			acting = actingOf{}
		}
		// The initiator's rights are checked again under the task lock: a person disabled or
		// ungranted after enqueue gets no work done in their name (PRM-8).
		if reason, err := c.accessLost(ctx, tx, now, tv, accessPrincipal(initiator, rec), "task:"+typ, cat.TaskTypes[typ].DataClass); err != nil {
			return nil, err
		} else if reason != "" {
			if err := c.failTask(ctx, tx, now, w.Tenant, id, initiator, rec, reason); err != nil {
				return nil, err
			}
			continue
		}
		// A task above the running ceiling of its type waits without spending an attempt
		// (DSP-7).
		if quotas {
			full, err := runningCeilingReached(ctx, tx, w.Tenant, tv.Spec.Dispatch, typ, rec)
			if err != nil {
				return nil, err
			}
			if full {
				if err := markWaiting(ctx, tx, now, w.Tenant, nil, []string{id}, "type_quota"); err != nil {
					return nil, err
				}
				skip = append(skip, id)
				continue
			}
		}
		picked = true
	}
	tt := cat.TaskTypes[typ]
	leaseS := c.cfg.LeaseS
	if d, ok := parseDuration(tt.Timeouts.Lease); ok {
		leaseS = int(d / time.Second)
	}
	leaseS = min(leaseS, maxLeaseS)
	leaseNo++
	runID := id + ":" + strconv.Itoa(leaseNo)
	item, err := c.newLease(ctx, tx, now, w, "attempt", id, leaseNo, leaseS, runID, acting)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'working', lease_no = $2, run_at = NULL, reason = NULL, updated_at = $3 WHERE id = $1`, id, leaseNo, now); err != nil {
		return nil, err
	}
	// The task record names the identity of its current attempt, or none; only the core writes
	// it (IDN-8).
	if acting.Bot != "" {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('acting_bot', $2::text, 'bot_epoch', $3::int) WHERE id = $1`,
			id, acting.Bot, acting.BotEpoch); err != nil {
			return nil, err
		}
	} else if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record - 'acting_bot' - 'bot_epoch' WHERE id = $1 AND record ? 'acting_bot'`, id); err != nil {
		return nil, err
	}
	var record map[string]any
	if err := json.Unmarshal(rec, &record); err != nil {
		return nil, err
	}
	// The run fingerprint and the catalog values of the result check are fixed at claim and
	// written into the attempt (PRV-1, OUT-1); the worker does not set them.
	slot, profile := mainSlot(role)
	// An attempt of the farm manager seat runs on the seat model of the farm charter (MGR-1).
	if seatOfRecord(rec) == managerSeatID {
		if mp := mstr(managerCharter(cat), "model_profile"); mp != "" {
			slot, profile = "seat", mp
		}
	}
	mark, _ := record["data_mark"].(string)
	g := &toolsGrant{Tenant: w.Tenant, Task: id, Worker: w.ID}
	if err := c.loadGrantTask(ctx, tx, g); err != nil {
		return nil, err
	}
	tools, err := c.toolCatalog(cat, g)
	if err != nil {
		return nil, err
	}
	// The persona of the acting identity's position, and the lens addendum of a review, go
	// before the type instruction and into the run fingerprint (GOV-4, COM-19, PRV-1).
	lens := ""
	if tt := cat.TaskTypes[typ]; tt.Purpose == "review" {
		lens, _ = record["lens"].(string)
	}
	persona, personaHashes, err := attemptPersona(ctx, tx, cat, w.Tenant, w.Role, acting, lens)
	if err != nil {
		return nil, err
	}
	att, err := c.prepareAttempt(cat, w, w.Tenant, typ, mark, slot, profile, tools, personaHashes)
	if err != nil {
		return nil, err
	}
	attRaw, err := json.Marshal(att)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE leases SET fingerprint = $3, attempt = $4 WHERE work_id = $1 AND lease_no = $2`, id, leaseNo, att.Fingerprint, attRaw); err != nil {
		return nil, err
	}
	// The model handle of the role's main slot and the tools handle of a role with tools (SEC-3):
	// bound to this lease and epoch. A role with no plugin tools gets one when the core offers it
	// a built-in tool.
	handles := map[string]any{"tools": nil, "model": nil, "session": nil, "memory": nil}
	if len(roleTools(cat, role)) > 0 || len(tools) > 0 {
		h := "bh_" + ids.Secret(32)
		if _, err := tx.Exec(ctx, `INSERT INTO handles (hash, kind, tenant, work_id, lease_no, epoch, worker_id, created_at)
			VALUES ($1, 'tools', $2, $3, $4, $5, $6, $7)`,
			c.hashSecret(h), w.Tenant, id, leaseNo, c.epoch, w.ID, now); err != nil {
			return nil, err
		}
		handles["tools"] = h
	}
	if slot != "" {
		h := "bh_" + ids.Secret(32)
		if _, err := tx.Exec(ctx, `INSERT INTO handles (hash, kind, tenant, work_id, lease_no, epoch, worker_id, slot, profile, created_at)
			VALUES ($1, 'model', $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.hashSecret(h), w.Tenant, id, leaseNo, c.epoch, w.ID, slot, profile, now); err != nil {
			return nil, err
		}
		handles["model"] = h
	}
	if err := c.leaseHandles(ctx, tx, now, w, role, id, leaseNo, g.Initiator, record, handles); err != nil {
		return nil, err
	}
	var taskBody map[string]any
	if err := json.Unmarshal(body, &taskBody); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"task": map[string]any{
			"id": id, "type": typ, "body": taskBody,
			"record": map[string]any{"max_tier": record["max_tier"], "data_mark": record["data_mark"], "untrusted_seen": record["untrusted_seen"]},
		},
		"instruction": joinInstruction(persona, tt.Instruction),
		"handles":     handles,
	}
	// Implementation extension (MAN-3): the result schema, so that the runtime can ask the
	// model for a matching answer. The instruction itself stays the trusted text from git.
	if tt.ResultSchema != nil {
		payload["result_schema"] = c.payloadResultSchema(cat, w.Tenant, tt)
	}
	// A type that reads bot memory gets the facts of the acting identity (MEM-6).
	if facts, err := c.taskFacts(ctx, tx, now, cat, w, typ, id, acting, record); err != nil {
		return nil, err
	} else if facts != nil {
		payload["facts"] = facts
	}
	// An analysis task carries the author's "My outcomes", the last rows by code (DIG-7, INS-5).
	if tt.Purpose == "insight" {
		mo, err := myOutcomes(ctx, tx, w.Tenant, w.ID, myOutcomesRows)
		if err != nil {
			return nil, err
		}
		payload["my_outcomes"] = mo
	}
	item.Payload = payload
	if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "task.working", Subject: id, Data: map[string]any{"task": id, "lease_no": leaseNo, "fingerprint": att.Fingerprint}}); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "bot", Role: w.Role, Task: id},
		Command: "task.claim", Target: &Target{Kind: "task", ID: id}, OK: true,
		After: map[string]any{"lease_no": leaseNo, "worker": w.ID},
	}); err != nil {
		return nil, err
	}
	return item, nil
}

// mainSlot picks the slot of the attempt's model handle: base, then analysis, then seat, then
// the first by name.
func mainSlot(role manifest.Role) (string, string) {
	for _, s := range []string{"base", "analysis", "seat"} {
		if p, ok := role.Models[s]; ok {
			return s, p
		}
	}
	names := make([]string, 0, len(role.Models))
	for s := range role.Models {
		names = append(names, s)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", ""
	}
	return names[0], role.Models[names[0]]
}

// claimDelivery takes the oldest planned delivery for this channel, sink or adapter by its
// enroll name (TEN-3). Model text for a recipient waits until their ai_notice in this channel
// is delivered (DLV-13), and until a privacy_notice was delivered and none is pending (DLV-12).
func (c *Core) claimDelivery(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker) (*WorkItem, error) {
	var (
		id, kind, addr, targetKind, targetID string
		recipient, dataMark, task            *string
		subjectKind, subjectID               *string
		origin                               string
		body                                 []byte
		leaseNo                              int
	)
	tv := cat.Tenants[w.Tenant]
	version := ""
	if tv != nil {
		version = tv.Spec.Privacy.NoticeVersionString()
	}
	var cardBody map[string]any
	replacesID := ""
	for picked := false; !picked; {
		err := tx.QueryRow(ctx, `SELECT d.id, d.kind, d.addr, d.target_kind, d.target_id, d.recipient, d.data_mark, d.body, d.lease_no, d.task, d.dataorigin, d.subject_kind, d.subject_id FROM deliveries d
		WHERE d.tenant = $1 AND d.exec_kind = $2 AND d.exec_name = $3 AND d.state = 'planned' AND (d.run_at IS NULL OR d.run_at <= $4)
		AND NOT (d.dataorigin = 'model' AND EXISTS (SELECT 1 FROM contacts ct WHERE ct.tenant = d.tenant AND ct.channel = d.exec_name
			AND ct.principal = d.recipient AND ct.ai_notice_delivery IS NOT NULL AND ct.ai_notice_at IS NULL))
		AND NOT (d.dataorigin = 'model' AND d.recipient IS NOT NULL AND (
			EXISTS (SELECT 1 FROM deliveries p WHERE p.tenant = d.tenant AND p.exec_name = d.exec_name AND p.recipient = d.recipient
				AND p.kind = 'privacy_notice' AND p.state IN ('planned', 'sending'))
			OR NOT EXISTS (SELECT 1 FROM deliveries p WHERE p.tenant = d.tenant AND p.exec_name = d.exec_name AND p.recipient = d.recipient
				AND p.kind = 'privacy_notice' AND p.state = 'delivered' AND p.notice_version IS NOT DISTINCT FROM nullif($5, ''))))
		AND NOT (d.alarmkey IS NOT NULL AND EXISTS (SELECT 1 FROM deliveries o WHERE o.tenant = d.tenant AND o.alarmkey = d.alarmkey
			AND o.recipient IS NOT DISTINCT FROM d.recipient AND o.seq < d.seq AND o.state IN ('planned', 'sending')))
		ORDER BY d.seq LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, w.Kind, w.Target, now, version).
			Scan(&id, &kind, &addr, &targetKind, &targetID, &recipient, &dataMark, &body, &leaseNo, &task, &origin, &subjectKind, &subjectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		// The recipient is checked again at handoff: a person disabled, offboarded or ungranted
		// after the delivery was planned gets nothing, a code notice included (DLV-6).
		if recipient != nil && tv != nil {
			mark := ""
			if dataMark != nil {
				mark = *dataMark
			}
			reason, err := c.accessLost(ctx, tx, now, tv, *recipient, "", mark)
			if err != nil {
				return nil, err
			}
			if reason == "person_inactive" {
				// Nothing more goes to an inactive person: their other planned deliveries end too,
				// so model text waiting behind a withheld notice does not stay planned.
				if err := c.withholdDeliveriesTo(ctx, tx, now, w.Tenant, *recipient, reason); err != nil {
					return nil, err
				}
				continue
			}
			if reason != "" {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
					return nil, err
				}
				if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "delivery.withheld", Subject: id, Data: map[string]any{"delivery": id, "kind": kind, "reason": reason}}); err != nil {
					return nil, err
				}
				continue
			}
		}
		// A hold with no_delivery on the farm, the tenant or this channel withholds the
		// delivery; an alarm notice and the export of alarms and of the audit head still go,
		// since a hold does not silence alarms (HLD-3, MON-1).
		role, err := deliveryRole(ctx, tx, deref(task))
		if err != nil {
			return nil, err
		}
		if kind != "alarm_notice" && kind != "monitor_alarm" && kind != "monitor_audit_head" {
			scopes := append(tenantScopes(w.Tenant), scopeRef{kind: "channel", tenant: w.Tenant, name: w.Target})
			if role != "" && origin == "model" {
				// The role whose attempt wrote the text, as the auto-stop of HLD-6 scopes it. A
				// code text, such as the notice in place of an answer above the ceiling, is not
				// the role's writing.
				scopes = append(scopes, scopeRef{kind: "role", tenant: w.Tenant, name: role})
			}
			held, err := heldBy(ctx, tx, now, []string{"no_delivery"}, scopes...)
			if err != nil {
				return nil, err
			}
			if held != "" {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'held', updated_at = $2 WHERE id = $1`, id, now); err != nil {
					return nil, err
				}
				if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "delivery.withheld", Subject: id, Data: map[string]any{"delivery": id, "kind": kind, "reason": "held"}}); err != nil {
					return nil, err
				}
				continue
			}
		}
		// A script run is checked again at hand-out: a hold set after the call was queued, or a
		// start window that ran out, withholds it, and the sweep ends its call (HLD-3, MON-10).
		if kind == "monitor_script" {
			reason, err := c.scriptHandout(ctx, tx, now, id)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
					return nil, err
				}
				if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "delivery.withheld", Subject: id, Data: map[string]any{"delivery": id, "kind": kind, "reason": reason}}); err != nil {
					return nil, err
				}
				if err := c.scriptFailed(ctx, tx, now, id, reason); err != nil {
					return nil, err
				}
				continue
			}
		}
		// A group chat gets a delivery only while it is bound, has no unknown member and the
		// mark is within the binding ceiling (DLV-7, DLV-15).
		if w.Kind == "channel" && targetKind == "chat" && strings.HasPrefix(targetID, "group:") {
			b, err := loadBinding(ctx, tx, w.Tenant, w.Target, targetID)
			if err != nil {
				return nil, err
			}
			reason := ""
			switch {
			case b == nil:
				reason = "ref_invalid"
			case b.MembersUnknown:
				reason = "unknown_member"
			case deref(dataMark) != "" && !c.markWithin(*dataMark, b.Ceiling):
				reason = "channel_ceiling"
			}
			if reason != "" {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
					return nil, err
				}
				if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "delivery.withheld", Subject: id, Data: map[string]any{"delivery": id, "kind": kind, "reason": reason}}); err != nil {
					return nil, err
				}
				if kind == "privacy_notice" {
					if err := resetPrivacyNotice(ctx, tx, id); err != nil {
						return nil, err
					}
				}
				continue
			}
		}
		if tv != nil {
			reason, err := c.outboundRefusal(ctx, tx, now, tv, w.Kind, w.Target, outgoing{
				ID: id, Kind: kind, Addr: addr, TargetKind: targetKind, TargetID: targetID,
				Task: deref(task), Recipient: deref(recipient), DataMark: deref(dataMark), DataOrigin: origin, Body: body,
			}, role)
			if err != nil {
				return nil, err
			}
			if reason != "" {
				continue
			}
		}
		if sk := deref(subjectKind); sk == "approval" || sk == "proposal" || sk == "control" || sk == "campaign" || sk == "script" || sk == "script_gate" {
			// A card is drawn from the record now, so it shows the current state (APR-3). An
			// update waits while the card it edits is being sent, and is dropped when that card
			// never arrived: the card itself then shows the current state.
			var b map[string]any
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			var drawn map[string]any
			var replaces string
			var err error
			switch sk {
			case "approval":
				drawn, replaces, err = c.approvalCardBody(ctx, tx, cat, w.Tenant, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			case "campaign":
				drawn, replaces, err = c.campaignCardBody(ctx, tx, cat, w.Tenant, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			case "script", "script_gate":
				drawn, replaces, err = c.scriptCardBody(ctx, tx, cat, w.Tenant, sk, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			case "proposal":
				drawn, replaces, err = c.proposalCardBody(ctx, tx, cat, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			default:
				drawn, replaces, err = controlCardBody(ctx, tx, w.Tenant, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			}
			if err != nil {
				return nil, err
			}
			if drawn == nil && kind != cardUpdate {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'ref_invalid', updated_at = $2 WHERE id = $1`, id, now); err != nil {
					return nil, err
				}
				continue
			}
			if kind == cardUpdate && replaces == "" {
				var sending bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE id = $1 AND state IN ('planned', 'sending'))`, b["of"]).Scan(&sending); err != nil {
					return nil, err
				}
				if sending {
					if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2 WHERE id = $1`, id, now.Add(5*time.Second)); err != nil {
						return nil, err
					}
				} else if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'ref_invalid', updated_at = $2 WHERE id = $1`, id, now); err != nil {
					return nil, err
				}
				continue
			}
			cardBody, replacesID = drawn, replaces
		} else if sk := deref(subjectKind); sk == "memory_record" || sk == "subject_request" {
			// The card of a memory record or a person's request is drawn now too; one whose
			// record was erased is not sent.
			var b map[string]any
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			drawn, err := c.subjectCardBody(ctx, tx, w.Tenant, sk, deref(subjectID), b)
			if err != nil {
				return nil, err
			}
			if drawn == nil {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'ref_invalid', updated_at = $2 WHERE id = $1`, id, now); err != nil {
					return nil, err
				}
				continue
			}
			cardBody = map[string]any{"form": "card", "card": drawn}
		} else if deref(subjectKind) == "incident" && kind == managerNoticeKind {
			// The message to people about an incident is drawn now from its record and its
			// notice log; an update edits the first message, or waits while it is being sent
			// (MGR-39).
			var b map[string]any
			if err := json.Unmarshal(body, &b); err != nil {
				return nil, err
			}
			drawn, replaces, reason, wait, err := c.managerNoticeBody(ctx, tx, now, id, deref(subjectID), deref(recipient), b)
			if err != nil {
				return nil, err
			}
			if wait {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2 WHERE id = $1`, id, now.Add(5*time.Second)); err != nil {
					return nil, err
				}
				continue
			}
			if reason != "" {
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = $2, updated_at = $3 WHERE id = $1`, id, reason, now); err != nil {
					return nil, err
				}
				continue
			}
			cardBody, replacesID = drawn, replaces
		}
		picked = true
	}
	leaseNo++
	leaseS := c.cfg.LeaseS
	if kind == "monitor_script" {
		// A script runs up to its timeout plus the API round trip within one lease.
		leaseS = max(leaseS, min(scriptTimeout+60, maxLeaseS))
	}
	item, err := c.newLease(ctx, tx, now, w, "delivery", id, leaseNo, leaseS, "")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'sending', lease_no = $2, attempts = attempts + 1, run_at = NULL, updated_at = $3 WHERE id = $1`,
		id, leaseNo, now); err != nil {
		return nil, err
	}
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	if cardBody != nil {
		b = cardBody
	}
	d := map[string]any{"id": id, "kind": kind, "to": addr, "body": b}
	if replacesID != "" {
		d["replaces_external_id"] = replacesID
	}
	if targetKind == "chat" {
		k, ext, _ := strings.Cut(targetID, ":")
		d["recipient"] = map[string]any{"chat": map[string]any{"kind": k, "external_id": ext}}
	}
	payload := map[string]any{
		"target":   map[string]any{"kind": w.Kind, "name": w.Target},
		"delivery": d,
	}
	// Implementation extension (MAN-3): the recipient's locale for code texts; without it the
	// channel takes en (DLV-9).
	if recipient != nil {
		var locale *string
		_ = tx.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, w.Tenant, *recipient).Scan(&locale)
		if locale != nil && *locale != "" {
			payload["locale"] = *locale
		}
	}
	item.Payload = payload
	return item, nil
}

// joinInstruction puts the persona before the type instruction (GOV-4).
func joinInstruction(persona, instruction string) string {
	instruction = strings.TrimSpace(instruction)
	if persona == "" {
		return instruction
	}
	if instruction == "" {
		return persona
	}
	return persona + "\n\n" + instruction
}
