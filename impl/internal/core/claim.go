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
	"github.com/HunterEwok/botaru/impl/internal/money"
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
			case k == "erasure" && (w.Kind == "sink" || w.Kind == "monitor" || w.Kind == "channel" || w.Kind == "agent"):
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
	// One read of the epoch: the lease row and its token carry the same one, even if a test
	// bench restore moves it meanwhile.
	epoch := c.epoch.Load()
	if _, err := tx.Exec(ctx, `INSERT INTO leases (work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until, run_id, created_at, acting_bot, bot_epoch, seat, seat_epoch)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9, $10, $11, $12, $13)`,
		workID, leaseNo, kind, w.Tenant, w.ID, epoch, until, nullable(runID), now, nullable(a.Bot), be, nullable(a.Seat), se); err != nil {
		return nil, err
	}
	tok, err := c.issueToken(epoch, workID, leaseNo, w.ID, a)
	if err != nil {
		return nil, err
	}
	return &WorkItem{
		WorkID: workID, Kind: kind, LeaseNo: leaseNo, Epoch: epoch, Tenant: w.Tenant,
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

// revokeWorkOf cancels the tasks a principal has in work once their access no longer covers
// them: the lease and handles go in the same transaction, so the worker is fenced at its next
// call and no more task data reaches the model in their name (PRM-3, PRM-8, FEN-4).
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
	// Queued tasks go in the same transaction as running ones (spec/lifecycles/task.md, PRM-3).
	rows, err := tx.Query(ctx, `SELECT id, type, record FROM tasks WHERE tenant = $1 AND initiator = $2 AND state IN ('submitted', 'working') AND type NOT LIKE 'action.%' ORDER BY id FOR UPDATE`, tv.Name, principal)
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
		if err := c.revokeTask(ctx, tx, now, tv.Name, w.id, principal, w.rec, reason); err != nil {
			return err
		}
	}
	return c.recheckApprovalsOf(ctx, tx, now, tv, principal)
}

// revokeTask cancels a task whose initiator lost access (PRM-3) with the ends a failure had:
// the task.canceled event keeps the detailed access reason (PRM-8), a child answers its
// parent's continuation (CNT-1..CNT-3), a scheduled slot ends, and for a lost right the origin
// chat gets the code text task.canceled.rights_revoked.
func (c *Core) revokeTask(ctx context.Context, tx pgx.Tx, now time.Time, tenant, id, initiator string, rec []byte, access string) error {
	reason := revokedReason(access)
	if err := c.cancelTaskIDsWith(ctx, tx, now, tenant, []string{id}, reason, map[string]any{"access_reason": access}); err != nil {
		return err
	}
	if err := c.replyContinuation(ctx, tx, now, tenant, id, "canceled"); err != nil {
		return err
	}
	if err := c.scheduleSlotEnded(ctx, tx, now, tenant, id, rec, reason); err != nil {
		return err
	}
	if reason != "rights_revoked" {
		return nil
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
		Body: map[string]any{"form": "text_code", "text_key": "task.canceled.rights_revoked", "params": map[string]any{"reason": access}},
	})
	return err
}

// revokedReason is the cancellation reason of a task whose initiator lost access (PRM-3): a
// disabled or unknown initiator gives person_inactive, a lost task:<type> or data:<class>
// right gives rights_revoked.
func revokedReason(reason string) string {
	if reason == "person_inactive" {
		return reason
	}
	return "rights_revoked"
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
	// A runtime with model_attribution per_profile has capacity 1: while it holds an active
	// attempt lease, an expired one too until the reaper ends it, it gets no second attempt, so
	// the profile's handle and its usage belong to that one lease (RT-3).
	if perProfile(w) {
		var busy bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM leases WHERE worker_id = $1 AND kind = 'attempt' AND state = 'active')`,
			w.ID).Scan(&busy); err != nil || busy {
			return nil, err
		}
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
	// A role with skills runs only on a runtime that declared runtime.skills: runtime_files
	// at its last hello; another gets no attempt, and the tasks wait (SKL-2).
	withSkills := knowledgeOn() && len(role.Skills) > 0
	if withSkills && w.Kind == "agent" && (w.Hello == nil || w.Hello.Runtime == nil || w.Hello.Runtime.Skills != "runtime_files") {
		return nil, markWaiting(ctx, tx, now, w.Tenant, claims, nil, "no_member_ready")
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
	// fallback is the farm charter's fallback_profile when a seat task runs on it (MGR-49).
	fallback := ""
	// A lens review goes only to its executor role and never to the author's worker (COM-2).
	// Fairness within a class (DSP-6): the next task comes from the fairness key with the
	// fewest running attempts per weight of its kind; a key of weight 0 only when no other key
	// of the class waits; a tie goes to the key whose oldest waiting task came first, then to
	// the smaller task id.
	var weights map[string]int
	if tv != nil && tv.Spec.Dispatch.Fairness != nil {
		weights = tv.Spec.Dispatch.Fairness.Weights
		if weights == nil {
			weights = map[string]int{}
		}
	}
	for picked := false; !picked; {
		fallback = ""
		const waiting = `tenant = $1 AND state = 'submitted' AND body IS NOT NULL AND type = ANY($2) AND (run_at IS NULL OR run_at <= $3) AND (deadline_at IS NULL OR deadline_at > $3)
			AND NOT id = ANY($4)
			AND (record->>'executor_role' IS NULL OR record->>'executor_role' = $5) AND (record->>'not_worker' IS NULL OR record->>'not_worker' <> $6)`
		var err error
		if weights == nil {
			err = tx.QueryRow(ctx, `SELECT id, type, body, record, lease_no, initiator FROM tasks WHERE `+waiting+`
			ORDER BY priority_class, created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, claims, now, skip, w.Role, w.ID).
				Scan(&id, &typ, &body, &rec, &leaseNo, &initiator)
		} else {
			err = tx.QueryRow(ctx, `WITH keys AS (
				SELECT priority_class, coalesce(record->>'fairness_key', '') AS fk, min(created_at) AS oldest FROM tasks WHERE `+waiting+` GROUP BY 1, 2),
			running AS (SELECT coalesce(record->>'fairness_key', '') AS fk, count(*) AS n FROM tasks WHERE tenant = $1 AND state = 'working' GROUP BY 1),
			weighted AS (SELECT k.priority_class, k.fk, k.oldest, coalesce(r.n, 0) AS n,
				coalesce(($7::jsonb ->> split_part(k.fk, ':', 1))::int, 1) AS weight FROM keys k LEFT JOIN running r ON r.fk = k.fk)
			SELECT t.id, t.type, t.body, t.record, t.lease_no, t.initiator FROM tasks t
			JOIN weighted k ON k.priority_class = t.priority_class AND k.fk = coalesce(t.record->>'fairness_key', '')
			WHERE `+strings.ReplaceAll(waiting, "tenant = $1", "t.tenant = $1")+`
			ORDER BY t.priority_class, k.weight = 0, CASE WHEN k.weight = 0 THEN 0 ELSE k.n::float8 / k.weight END, k.oldest, t.created_at, t.id
			LIMIT 1 FOR UPDATE OF t SKIP LOCKED`, w.Tenant, claims, now, skip, w.Role, w.ID, weights).
				Scan(&id, &typ, &body, &rec, &leaseNo, &initiator)
		}
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
			// From v0.3 work outside sev1 waits once 70% of the month of the manager line is
			// spent, or runs on a checked fallback profile (MGR-49).
			if seat == managerSeatID {
				fb, wait, err := c.managerMonthGate(ctx, tx, now, cat, tv, rec, cat.TaskTypes[typ])
				if err != nil {
					return nil, err
				}
				if wait {
					if err := markWaiting(ctx, tx, now, w.Tenant, nil, []string{id}, "budget_exhausted"); err != nil {
						return nil, err
					}
					skip = append(skip, id)
					continue
				}
				fallback = fb
				// The analysis takes its tree budget from the rest of the day's ceiling, or
				// waits for the next day (MGR-53).
				if typ == postmortemType {
					budget, ok, err := postmortemClaimBudget(ctx, tx, now, cat, tv, id)
					if err != nil {
						return nil, err
					}
					if !ok {
						// The deadline and start_by move by the same shift, so the wait does not
						// end the task.
						if _, err := tx.Exec(ctx, `UPDATE tasks SET run_at = $2, deadline_at = deadline_at + ($2 - $3), start_by_at = start_by_at + ($2 - $3) WHERE id = $1`,
							id, tenantDay(tv, now).AddDate(0, 0, 1), now); err != nil {
							return nil, err
						}
						if err := markWaiting(ctx, tx, now, w.Tenant, nil, []string{id}, "budget_exhausted"); err != nil {
							return nil, err
						}
						skip = append(skip, id)
						continue
					}
					if typeMax := money.FromEUR(cat.TaskTypes[typ].TreeBudgetEUR); typeMax > 0 && typeMax < budget {
						budget = typeMax
					}
					if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('tree_budget_micros', $2::bigint) WHERE id = $1`, id, budget); err != nil {
						return nil, err
					}
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
			// The task is canceled, not failed, without spending an attempt (PRM-3).
			if err := c.revokeTask(ctx, tx, now, w.Tenant, id, initiator, rec, reason); err != nil {
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
	// A run gets a role's skills only from the core copy whose tree hash equals its skill-ref;
	// while one is missing the task gets no attempt and waits with skill_missing (SKL-2).
	var skillRefs []any
	var skillFiles map[string]map[string]string
	if withSkills {
		var err error
		skillRefs, skillFiles, err = roleSkills(ctx, tx, role)
		if errors.Is(err, errSkillMissing) {
			if skillWarnDue(w.Tenant, w.Role, role.Skills, time.Now()) {
				c.log.Warn("a pinned skill has no core copy of its hash; the role's tasks wait", "tenant", w.Tenant, "role", w.Role)
			}
			return nil, markWaiting(ctx, tx, now, w.Tenant, claims, nil, "skill_missing")
		}
		if err != nil {
			return nil, err
		}
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
		if fallback != "" {
			slot, profile = "seat", fallback
			if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || '{"seat_fallback": true}'::jsonb WHERE id = $1`, id); err != nil {
				return nil, err
			}
		} else if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record - 'seat_fallback' WHERE id = $1 AND record ? 'seat_fallback'`, id); err != nil {
			return nil, err
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
			c.hashSecret(h), w.Tenant, id, leaseNo, item.Epoch, w.ID, now); err != nil {
			return nil, err
		}
		handles["tools"] = h
	}
	// An approved grant of the task gives this attempt the granted slot; the run fingerprint
	// keeps the profile configured for the role's slot (MGR-18, GW-7, PRV-1).
	if slot != "" {
		if gp, ok, err := claimGrant(ctx, tx, now, cat, w.Tenant, id, w.Role); err != nil {
			return nil, err
		} else if ok {
			slot, profile = grantedSlot, gp
		}
	}
	if slot != "" {
		h := "bh_" + ids.Secret(32)
		if _, err := tx.Exec(ctx, `INSERT INTO handles (hash, kind, tenant, work_id, lease_no, epoch, worker_id, slot, profile, created_at)
			VALUES ($1, 'model', $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.hashSecret(h), w.Tenant, id, leaseNo, item.Epoch, w.ID, slot, profile, now); err != nil {
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
	// The language of the answer by the locale order for the recipient of the result; the
	// model does not choose it (DLV-18).
	var tr treeRecord
	if err := json.Unmarshal(rec, &tr); err != nil {
		return nil, err
	}
	rl, err := c.replyLocale(ctx, tx, cat, w.Tenant, id, g.Initiator, tr)
	if err != nil {
		return nil, err
	}
	payload["reply_locale"] = rl
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
		// And the evolution block of its role: variants of the archive with different
		// approaches and the live review rules, outcome classes only (EVO-3, EVO-5).
		// Only for a tenant with the evolution on: the input of 0.3.0 stays as it was otherwise.
		if on, err := evolutionOn(ctx, tx, w.Tenant); err != nil {
			return nil, err
		} else if on {
			evo, err := evolutionInput(ctx, tx, w.Tenant, w.Role)
			if err != nil {
				return nil, err
			}
			payload["evolution"] = evo
		}
	}
	// The skills as references, and their files as the extension field skill_files, which a
	// runtime of runtime.skills: runtime_files writes into its own skill directory (SKL-2, MAN-3).
	if len(skillRefs) > 0 {
		payload["skills"] = skillRefs
		payload["skill_files"] = skillFiles
	}
	// An ingest job gets the document it cuts (extension field kb_document, KNW-5).
	if kd, err := kbDocumentPayload(ctx, tx, id, typ); err != nil {
		return nil, err
	} else if kd != nil {
		payload["kb_document"] = kd
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
	// skip are the deliveries this claim passed over: a deferred card behind an earlier one.
	skip := []string{}
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
		AND NOT d.id = ANY($6)
		ORDER BY d.seq LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, w.Kind, w.Target, now, version, skip).
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
			// The executor itself: its channel or sink and its worker (HLD-1, SEC-6).
			scopes := append(tenantScopes(w.Tenant), scopeRef{kind: "worker", tenant: w.Tenant, name: strings.ToLower(w.ID)})
			switch w.Kind {
			case "sink":
				scopes = append(scopes, scopeRef{kind: "sink", tenant: w.Tenant, name: w.Target})
			default:
				scopes = append(scopes, scopeRef{kind: "channel", tenant: w.Tenant, name: w.Target})
			}
			if role != "" && origin == "model" {
				// The role whose attempt wrote the text, as the auto-stop of HLD-6 scopes it. A
				// code text, such as the notice in place of an answer above the ceiling, is not
				// the role's writing.
				scopes = append(scopes, scopeRef{kind: "role", tenant: w.Tenant, name: role})
			}
			// An export passes a suspension or an offboarding of its tenant: only export is
			// allowed while a tenant is offboarding (LCY-1); a compromised, sink, worker or farm
			// hold still stops it.
			// The admins' notice about a suspension or an offboarding passes it too (HLD-3).
			var except []string
			if kind == "export" {
				except = []string{"suspended", "offboarding"}
			} else if hold := aboutTenantHold(body); kind == "notice" && hold != "" {
				// Only the kind of hold the notice is about, and only when that hold is a
				// suspension or an offboarding of this very tenant.
				var reason string
				err := tx.QueryRow(ctx, `SELECT reason FROM holds WHERE id = $1 AND scope_kind = 'tenant' AND scope_tenant = $2
					AND reason IN ('suspended', 'offboarding')`, hold, w.Tenant).Scan(&reason)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return nil, err
				}
				if reason != "" {
					except = []string{reason}
				}
			}
			held, err := heldByExcept(ctx, tx, now, []string{"no_delivery"}, except, scopes...)
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
		// The card limit per person per day: a non-urgent card over it waits until the next day
		// (APR-15).
		if recipient != nil && tv != nil && w.Kind == "channel" {
			wait, err := c.cardLimit(ctx, tx, now, tv, id, kind, *recipient, deref(subjectKind), deref(subjectID), w.Target)
			if err != nil {
				return nil, err
			}
			if wait {
				skip = append(skip, id)
				continue
			}
		}
		// The person's quiet hours and daily digest defer a deferrable delivery (DLV-10).
		if recipient != nil && w.Kind == "channel" {
			wait, err := c.prefsDefer(ctx, tx, now, cat, w.Tenant, id, kind, *recipient)
			if err != nil {
				return nil, err
			}
			if wait {
				skip = append(skip, id)
				continue
			}
		}
		// The digest pace of the chat holds at hand-out too (REP-10).
		if kind == "digest" && targetKind == "chat" && w.Kind == "channel" {
			wait, err := c.digestHandout(ctx, tx, now, w.Tenant, id, w.Target, targetID)
			if err != nil {
				return nil, err
			}
			if wait {
				skip = append(skip, id)
				continue
			}
		}
		// The first delivery of a limited card sets the expiry of its subject; it is undone
		// when the card is withheld below, so it holds only for a card that goes to sending
		// (APR-15). The card is drawn after it, so it shows the new deadline.
		firstSet := false
		if recipient != nil && tv != nil && w.Kind == "channel" && c.limitedCard(kind, deref(subjectID)) {
			if _, err := tx.Exec(ctx, `SAVEPOINT card_first`); err != nil {
				return nil, err
			}
			if err := c.firstCardDelivery(ctx, tx, now, kind, deref(subjectKind), deref(subjectID)); err != nil {
				return nil, err
			}
			firstSet = true
		}
		undoFirst := func() error {
			if !firstSet {
				return nil
			}
			firstSet = false
			_, err := tx.Exec(ctx, `ROLLBACK TO SAVEPOINT card_first`)
			return err
		}
		if sk := deref(subjectKind); sk == "approval" || sk == "proposal" || sk == "control" || sk == "campaign" || sk == "script" || sk == "script_gate" || sk == "governance_case" || sk == "attribution" || sk == "takeover" {
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
			case "governance_case", "attribution":
				drawn, replaces, err = c.govCardBody(ctx, tx, cat, sk, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			case "proposal":
				drawn, replaces, err = c.proposalCardBody(ctx, tx, cat, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			case "takeover":
				drawn, replaces, err = takeoverCardBody(ctx, tx, w.Tenant, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			default:
				drawn, replaces, err = controlCardBody(ctx, tx, w.Tenant, kind, deref(subjectID), targetID, deref(recipient), w.Target, b)
			}
			if err != nil {
				return nil, err
			}
			if drawn == nil && kind != cardUpdate {
				if err := undoFirst(); err != nil {
					return nil, err
				}
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'withheld', reason = 'ref_invalid', updated_at = $2 WHERE id = $1`, id, now); err != nil {
					return nil, err
				}
				continue
			}
			if kind == cardUpdate && replaces == "" {
				var sending bool
				var cardAt *time.Time
				err := tx.QueryRow(ctx, `SELECT true, run_at FROM deliveries WHERE id = $1 AND state IN ('planned', 'sending')`, b["of"]).Scan(&sending, &cardAt)
				if err != nil && !errors.Is(err, pgx.ErrNoRows) {
					return nil, err
				}
				if sending {
					// A card deferred by the card limit goes at its run_at; the update waits
					// for it instead of asking again every few seconds (APR-15).
					next := now.Add(5 * time.Second)
					if cardAt != nil && cardAt.After(next) {
						next = *cardAt
					}
					if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2 WHERE id = $1`, id, next); err != nil {
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
				if err := undoFirst(); err != nil {
					return nil, err
				}
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
		if firstSet {
			if _, err := tx.Exec(ctx, `RELEASE SAVEPOINT card_first`); err != nil {
				return nil, err
			}
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
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'sending', lease_no = $2, attempts = attempts + 1, run_at = NULL, updated_at = $3,
		handed_at = coalesce(handed_at, $3) WHERE id = $1`,
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
	// Implementation extension (MAN-3): the origin mark of the delivery, which the channel
	// passes the way its driver declares and a sink writes as ai_generated (DLV-13).
	d["dataorigin"] = origin
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
	// Implementation extension (MAN-3): the locale of the code texts by the order of DLV-18.
	locale, err := c.deliveryLocale(ctx, tx, cat, w.Tenant, w.Target, targetKind, targetID, deref(recipient), deref(task), addr)
	if err != nil {
		return nil, err
	}
	payload["locale"] = locale
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

// aboutTenantHold is the hold the admins' notice about a tenant hold names, or "".
func aboutTenantHold(body []byte) string {
	var b struct {
		AboutHold string `json:"about_hold"`
		TextKey   string `json:"text_key"`
	}
	if json.Unmarshal(body, &b) != nil || b.TextKey != "notice.tenant_held" {
		return ""
	}
	return b.AboutHold
}
