// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Takeover in its v0.2 form (MGR-21, decision point takeover_confirm). The farm manager seat asks
// with takeover_request to run a task of the tenant again on the seat profile. Code refuses it
// under distrust (takeover_untrusted) and without every condition (takeover_not_allowed): the
// type on the tenant's takeover.types, task.takeover:<type> in the tenant's consent group for
// the manager and money on the takeover line. An accepted request is staged and published at the
// end of the seat attempt as the takeover_confirm card to the tenant's people with
// model_grant.manage; the card is never lifted. "Approve" creates the new task with the key
// system:takeover:<task> and a grant of kind takeover on the seat profile for 1 attempt, whose
// cap_eur is the estimate the card showed, on the takeover line; the original task is canceled
// as superseded_by_takeover. The seat gets no answer beyond {staged: true}.

const (
	takeoverToolName = "takeover_request"
	takeoverCard     = "takeover_confirm"
	takeoverCardTTL  = time.Hour
	// takeoverGrantTTL is the expiry of the takeover grant: the 6 h of every grant.
	takeoverGrantTTL = grantTTLMax
)

// takeoverTool is takeover_request in a decision attempt of the farm manager seat (MGR-21).
func takeoverTool(cat *Catalog, g *toolsGrant) *catalogTool {
	if g.Purpose != "manage" || profileOf(cat, g.Role) != "farm-manager" || g.Type == grantsType {
		return nil
	}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []any{"task"},
		"properties": map[string]any{"task": map[string]any{"type": "string", "maxLength": 64, "pattern": "^t_[0-9A-HJKMNP-TV-Z]{26}$"}}}
	desc := "Ask a person of the tenant to let a task run again on the seat model, for one attempt. The answer is only {staged: true}."
	return &catalogTool{Name: takeoverToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: schema}
}

// takeoverToolCall checks a takeover request and stages it for the end of the attempt.
func (c *Core) takeoverToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	if !offered {
		return toolAnswer(map[string]any{"code": "forbidden"}, true), nil
	}
	if err := c.rs.validate(takeoverTool(cat, g).Schema, args); err != nil {
		return toolAnswer(map[string]any{"code": "input_schema_invalid"}, true), nil
	}
	acting, err := leaseActing(ctx, tx, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil {
		return nil, err
	}
	if m == nil || m.State != "held" || m.Holder != acting.Bot || m.Epoch != acting.SeatEpoch {
		return toolAnswer(map[string]any{"code": "fenced"}, true), nil
	}
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::boolean, false) FROM tasks WHERE id = $1`, g.Task).Scan(&untrusted); err != nil {
		return nil, err
	}
	refused := func(code string) (*mcp.CallResult, error) {
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "command", Actor: Actor{Principal: acting.Bot, Via: "seat", Role: g.Role, Task: g.Task},
			Command: takeoverToolName, Target: &Target{Kind: "task", ID: mstr(args, "task")}, OK: false, Reason: code,
		})
		return toolAnswer(map[string]any{"code": code}, true), err
	}
	// Distrust forbids a takeover (MGR-21, MGR-22).
	if untrusted {
		return refused("takeover_untrusted")
	}
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return nil, err
	}
	if restricted, err := seatTaskRestricted(ctx, tx, g.Tenant, rec); err != nil {
		return nil, err
	} else if restricted {
		return refused(restrictedReason)
	}
	task := mstr(args, "task")
	var typ, state string
	var body []byte
	err = tx.QueryRow(ctx, `SELECT type, state, body FROM tasks WHERE id = $1 AND tenant = $2`, task, g.Tenant).Scan(&typ, &state, &body)
	if errors.Is(err, pgx.ErrNoRows) || state == "completed" || state == "failed" || state == "canceled" {
		return refused("takeover_not_allowed")
	}
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[g.Tenant]
	types := mstrs(mmap(mmap(tv.Raw, "spec"), "takeover"), "types")
	profile := mstr(managerCharter(cat), "model_profile")
	line := takeoverLine()
	if !slices.Contains(types, typ) || profile == "" || !lineOpen(tv, line) ||
		!hasCap(tv, groupClosure(tv, []string{"farm-manager-ops"}), "task.takeover:"+typ) {
		return refused("takeover_not_allowed")
	}
	pr, ok := cat.Profile(g.Tenant, profile)
	if !ok {
		return refused("takeover_not_allowed")
	}
	// From v0.3 a functionary worker runs the takeover on the seat profile only with an eval
	// report of its role on that profile without a security failure (MGR-48).
	if claimsV03() {
		var orig []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, task).Scan(&orig); err != nil {
			return nil, err
		}
		role := takeoverRole(cat, typ, orig)
		if ok, err := takeoverEvalPassed(cat, g.Tenant, role, profile); err != nil {
			return nil, err
		} else if !ok {
			return refused("takeover_not_allowed")
		}
	}
	estimate := grantEstimate(pr, len(body), 1)
	if err := lockTenantBudget(ctx, tx, g.Tenant); err != nil {
		return nil, err
	}
	if fits, err := takeoverFits(ctx, tx, now, tv, estimate); err != nil {
		return nil, err
	} else if !fits {
		return refused("takeover_not_allowed")
	}
	open, err := openGrantMicros(ctx, tx, g.Tenant, line)
	if err != nil {
		return nil, err
	}
	if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", extra: estimate + open, line: line}); p != nil {
		if p.Reason != "budget_exhausted" {
			return nil, p
		}
		return refused("takeover_not_allowed")
	}
	var leaseNo int
	if err := tx.QueryRow(ctx, `SELECT lease_no FROM leases WHERE work_id = $1 AND state = 'active' AND kind = 'attempt'`, g.Task).Scan(&leaseNo); err != nil {
		return nil, err
	}
	id := ids.New(ids.Takeover)
	tag, err := tx.Exec(ctx, `INSERT INTO takeovers (id, tenant, task, type, seat_task, lease_no, acting_bot, epoch, profile, estimate_micros, state, nonce, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'staged', $11, $12, $12) ON CONFLICT DO NOTHING`,
		id, g.Tenant, task, typ, g.Task, leaseNo, acting.Bot, m.Epoch, profile, estimate, ids.Secret(16), now)
	if err != nil {
		return nil, err
	}
	if tag.RowsAffected() == 0 {
		return refused("takeover_not_allowed")
	}
	return toolAnswer(map[string]any{"staged": true}, false), nil
}

// publishTakeovers publishes the staged takeover requests of a completed seat attempt: each
// opens its takeover_confirm card (MGR-21).
func (c *Core) publishTakeovers(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, task string, leaseNo int) error {
	// The attempt may have seen untrusted text after it staged the request: then the request
	// is dropped as takeover_untrusted (MGR-21).
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::boolean, false) FROM tasks WHERE id = $1`, task).Scan(&untrusted); err != nil {
		return err
	}
	if untrusted {
		rows, err := tx.Query(ctx, `UPDATE takeovers SET state = 'dropped', reason = 'takeover_untrusted', updated_at = $3 WHERE seat_task = $1 AND lease_no = $2 AND state = 'staged'
			RETURNING id, tenant`, task, leaseNo, now)
		if err != nil {
			return err
		}
		type drop struct{ id, tenant string }
		drops, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (drop, error) {
			var d drop
			return d, row.Scan(&d.id, &d.tenant)
		})
		if err != nil {
			return err
		}
		for _, d := range drops {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: d.tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "seat", Role: managerSeatID, Task: task},
				Command: takeoverToolName, Target: &Target{Kind: "takeover", ID: d.id}, OK: false, Reason: "takeover_untrusted", After: map[string]any{"state": "dropped"},
			}); err != nil {
				return err
			}
		}
		return nil
	}
	rows, err := tx.Query(ctx, `UPDATE takeovers SET state = 'pending', expires_at = $3, updated_at = $4 WHERE seat_task = $1 AND lease_no = $2 AND state = 'staged'
		RETURNING id, tenant`, task, leaseNo, now.Add(takeoverCardTTL), now)
	if err != nil {
		return err
	}
	type pub struct{ id, tenant string }
	var ps []pub
	for rows.Next() {
		var p pub
		if err := rows.Scan(&p.id, &p.tenant); err != nil {
			rows.Close()
			return err
		}
		ps = append(ps, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, p := range ps {
		tv := cat.Tenants[p.tenant]
		if tv == nil {
			continue
		}
		chats, err := c.adminChats(ctx, tx, now, tv, "", "model_grant.manage")
		if err != nil {
			return err
		}
		for _, ch := range chats {
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: p.tenant, Kind: takeoverCard, Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: ch.ExternalID}.ID(), ExecKind: "channel", ExecName: ch.Channel,
				Recipient: ch.Principal, Body: map[string]any{"form": "card", "card": takeoverCard}, DataOrigin: "code",
				SubjectKind: "takeover", SubjectID: p.id,
			}); err != nil {
				return err
			}
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: p.tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "seat", Role: managerSeatID, Task: task},
			Command: takeoverToolName, Target: &Target{Kind: "takeover", ID: p.id}, OK: true, After: map[string]any{"state": "pending"},
		}); err != nil {
			return err
		}
	}
	return nil
}

// dropStaged removes what an attempt staged and did not publish because it did not complete:
// escalations and takeover requests (MGR-5, MGR-21).
func dropStaged(ctx context.Context, tx pgx.Tx, now time.Time, task string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM escalations e WHERE e.task = $1 AND e.state = 'staged'
		AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.work_id = e.task AND l.lease_no = e.lease_no AND l.state = 'active')`, task); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE takeovers t SET state = 'dropped', updated_at = $2 WHERE t.seat_task = $1 AND t.state = 'staged'
		AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.work_id = t.seat_task AND l.lease_no = t.lease_no AND l.state = 'active')`, task, now)
	return err
}

// takeoverRow is a takeover request as the store keeps it.
type takeoverRow struct {
	ID, Tenant, Task, Type, Profile, State, Nonce, DecidedBy, NewTask string
	Estimate                                                          int64
	Expires                                                           *time.Time
}

func loadTakeover(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*takeoverRow, error) {
	q := `SELECT id, tenant, task, type, profile, state, nonce, coalesce(decided_by, ''), coalesce(new_task, ''), estimate_micros, expires_at
		FROM takeovers WHERE id = $1 AND tenant = $2 AND state <> 'staged'`
	if lock {
		q += ` FOR UPDATE`
	}
	var r takeoverRow
	err := tx.QueryRow(ctx, q, id, tenant).Scan(&r.ID, &r.Tenant, &r.Task, &r.Type, &r.Profile, &r.State, &r.Nonce, &r.DecidedBy, &r.NewTask, &r.Estimate, &r.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// card draws the takeover_confirm card from the record: the task, its type, the seat profile,
// one attempt and the estimate that becomes the grant's cap; no model text.
func (r *takeoverRow) card() map[string]any {
	buttons := []any{}
	if r.State == "pending" {
		for _, code := range []string{"approve", "reject"} {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "takeover", "id": r.ID, "nonce": r.Nonce})
		}
	}
	card := map[string]any{
		"card": takeoverCard, "subject_kind": "takeover", "id": r.ID, "nonce": r.Nonce,
		"title": cut("Farm manager takeover: "+r.Type+" "+r.Task, 200), "state": r.State, "buttons": buttons,
		"context": map[string]any{"task": r.Task, "type": r.Type, "profile": r.Profile, "attempts": 1, "estimate_eur": money.CentsUp(r.Estimate),
			"line": takeoverLine(), "label": "farm manager request for a person's decision"},
		"untrusted_seen": false,
		"footer":         map[string]any{"why_you": "model_grant.manage", "on_silence": "no takeover, the original task continues"},
	}
	if r.Expires != nil {
		card["expires_at"] = fmtTime(*r.Expires)
		card["footer"].(map[string]any)["until"] = fmtTime(*r.Expires)
	}
	return card
}

// takeoverCardBody draws a takeover card or its update at hand-out.
func takeoverCardBody(ctx context.Context, tx pgx.Tx, tenant, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	r, err := loadTakeover(ctx, tx, tenant, id, false)
	if err != nil || r == nil {
		return nil, "", err
	}
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $5 AND subject_kind = 'takeover' AND subject_id = $1 AND kind = $6
			AND exec_name = $2 AND target_id = $3 AND recipient IS NOT DISTINCT FROM $4 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, channel, target, nullable(recipient), of, takeoverCard).Scan(&ext)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
		if ext == nil {
			return nil, "", nil
		}
		replaces = *ext
	}
	return map[string]any{"form": "card", "card": r.card()}, replaces, nil
}

// decideTakeover is P1 decide on a takeover_confirm card: once, by a person of the tenant with
// model_grant.manage before the card expires (MGR-21).
func (c *Core) decideTakeover(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a takeover card is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadTakeover(ctx, tx, w.Tenant, req.ID, true)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse("not_found", "no such takeover card")
		}
		if r.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "takeover", r.ID, []string{takeoverCard})
		if err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "takeover", ID: r.ID, Decision: req.Decision, State: r.State}
		if r.State != "pending" {
			if r.DecidedBy == press.principal && (r.State == "rejected") == (req.Decision == "reject") {
				return nil
			}
			if r.State == "expired" {
				return refuse("expired", "the card has expired")
			}
			return refuse("already_decided", "the card is decided")
		}
		if !hasCap(press.tv, press.groups, "model_grant.manage") {
			return refuse("no_group_cap", "the person has no model_grant.manage")
		}
		if r.Expires != nil && !r.Expires.After(now) {
			if err := c.endTakeover(ctx, tx, now, r, "expired", "", "", "expired"); err != nil {
				return err
			}
			return &committed{p: refuse("expired", "the card has expired")}
		}
		state, newTask, reason := "rejected", "", ""
		if req.Decision == "approve" {
			newTask, reason, err = c.runTakeover(ctx, tx, now, cat, r, press.principal)
			if err != nil {
				return err
			}
			state = "approved"
			if reason != "" {
				state = "rejected"
			}
		}
		if err := c.endTakeover(ctx, tx, now, r, state, press.principal, newTask, reason); err != nil {
			return err
		}
		out.State = state
		if err := c.decisionEvent(ctx, tx, now, w, ob, press, "takeover", r.ID, takeoverCard, req.Decision); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "takeover", ID: r.ID}, OK: true,
			After: map[string]any{"decision": req.Decision, "state": state, "task": nullable(newTask), "reason": nullable(reason)},
		})
		return err
	})
	return out, err
}

// runTakeover makes the takeover on approval: the new task of the same type with the original
// body and initiator, key system:takeover:<task>, the takeover grant on the seat profile for one
// attempt with the card's estimate as its cap on the takeover line, and the original task
// canceled as superseded_by_takeover. A condition that no longer holds gives the reason and no
// takeover.
func (c *Core) runTakeover(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *takeoverRow, by string) (string, string, error) {
	var typ, state, initiator string
	var body []byte
	if err := tx.QueryRow(ctx, `SELECT type, state, initiator, body FROM tasks WHERE id = $1 AND tenant = $2 FOR UPDATE`, r.Task, r.Tenant).Scan(&typ, &state, &initiator, &body); err != nil {
		return "", "", err
	}
	tv := cat.Tenants[r.Tenant]
	line := takeoverLine()
	if state == "completed" || state == "failed" || state == "canceled" || !lineOpen(tv, line) {
		return "", "takeover_not_allowed", nil
	}
	if err := lockTenantBudget(ctx, tx, r.Tenant); err != nil {
		return "", "", err
	}
	// The month's takeovers other than this one, which is still pending, and the ceilings
	// (MGR-48).
	if fits, err := takeoverFits(ctx, tx, now, tv, r.Estimate, r.ID); err != nil {
		return "", "", err
	} else if !fits {
		return "", "takeover_not_allowed", nil
	}
	open, err := openGrantMicros(ctx, tx, r.Tenant, line)
	if err != nil {
		return "", "", err
	}
	if p := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", extra: r.Estimate + open, line: line}); p != nil {
		return "", "takeover_not_allowed", nil
	}
	// From v0.3 the eval gate of the functionary role is checked again at the decision, and
	// that role is pinned on the new task (MGR-48).
	v03Role := ""
	if claimsV03() {
		var orig []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, r.Task).Scan(&orig); err != nil {
			return "", "", err
		}
		v03Role = takeoverRole(cat, typ, orig)
		if ok, err := takeoverEvalPassed(cat, r.Tenant, v03Role, r.Profile); err != nil {
			return "", "", err
		} else if !ok {
			return "", "takeover_not_allowed", nil
		}
	}
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return "", "", err
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: r.Tenant, Type: typ, Body: b, IdempotencyKey: "system:takeover:" + r.Task,
		Initiator: initiator, EnqueuedBy: "system:takeover", Refs: []string{r.Task}})
	if err != nil {
		return "", "", err
	}
	if res.Rejected != nil {
		return "", res.Rejected.Reason, nil
	}
	role := ""
	if err := tx.QueryRow(ctx, `SELECT coalesce(record->>'executor_role', '') FROM tasks WHERE id = $1`, res.TaskID).Scan(&role); err != nil {
		return "", "", err
	}
	if claimsV03() {
		// The new task runs on a worker of the functionary role and inherits untrusted_seen of
		// the original task, its attempts and the manager's basis (MGR-48).
		var orig []byte
		var untrusted bool
		if err := tx.QueryRow(ctx, `SELECT o.record, coalesce((o.record->>'untrusted_seen')::boolean, false) OR coalesce((s.record->>'untrusted_seen')::boolean, false)
			FROM tasks o, takeovers k JOIN tasks s ON s.id = k.seat_task WHERE o.id = $1 AND k.id = $2`, r.Task, r.ID).Scan(&orig, &untrusted); err != nil {
			return "", "", err
		}
		// The role whose eval report passed runs the new task, and only it gets the grant.
		role = v03Role
		inherit := map[string]any{"takeover_of": r.Task, "executor_role": role}
		if untrusted {
			inherit["untrusted_seen"], inherit["untrusted_source"] = true, "continues"
		}
		extra, err := json.Marshal(inherit)
		if err != nil {
			return "", "", err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, extra); err != nil {
			return "", "", err
		}
		if err := takeoverPoint(ctx, tx, now, r.Tenant, typ, role, r.ID); err != nil {
			return "", "", err
		}
	}
	if role == "" {
		role = managerSeatID
	}
	gid := ids.New(ids.ModelGrant)
	decided, _ := json.Marshal(map[string]any{"kind": "human", "principal": by})
	if _, err := tx.Exec(ctx, `INSERT INTO model_grants (id, tenant, task, role, kind, line, profile, attempts, attempts_left, estimate_micros, cap_micros, expires_at,
		decided_by, decided_at, state, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'takeover', $10, $5, 1, 1, $6, $6, $7, $8, $9, 'approved', $9, $9)`,
		gid, r.Tenant, res.TaskID, role, r.Profile, r.Estimate, now.Add(takeoverGrantTTL), decided, now, line); err != nil {
		return "", "", err
	}
	if err := c.cancelTaskIDs(ctx, tx, now, r.Tenant, []string{r.Task}, "superseded_by_takeover"); err != nil {
		return "", "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE takeovers SET grant_id = $2 WHERE id = $1`, r.ID, gid); err != nil {
		return "", "", err
	}
	return res.TaskID, "", nil
}

// endTakeover puts a takeover card in its final state and plans an update of every delivered
// copy, so none keeps its buttons.
func (c *Core) endTakeover(ctx context.Context, tx pgx.Tx, now time.Time, r *takeoverRow, state, by, newTask, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE takeovers SET state = $2, decided_by = $3, new_task = $4, reason = $5, updated_at = $6 WHERE id = $1`,
		r.ID, state, nullable(by), nullable(newTask), nullable(reason), now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'takeover' AND subject_id = $1 AND kind = $2 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, r.ID, takeoverCard)
	if err != nil {
		return err
	}
	type copyOf struct{ id, channel, chat, recipient string }
	var cs []copyOf
	for rows.Next() {
		var d copyOf
		if err := rows.Scan(&d.id, &d.channel, &d.chat, &d.recipient); err != nil {
			rows.Close()
			return err
		}
		cs = append(cs, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, d := range cs {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: r.Tenant, Kind: cardUpdate, Addr: "admins", TargetKind: "chat", TargetID: d.chat,
			ExecKind: "channel", ExecName: d.channel, Recipient: d.recipient, Body: map[string]any{"form": "card", "card": takeoverCard, "of": d.id},
			DataOrigin: "code", SubjectKind: "takeover", SubjectID: r.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// expireTakeovers ends the takeover cards whose hour passed: no takeover, the original task
// continues.
func (c *Core) expireTakeovers(ctx context.Context, tx pgx.Tx, now time.Time, _ *Catalog) error {
	rows, err := tx.Query(ctx, `SELECT id, tenant FROM takeovers WHERE state = 'pending' AND expires_at <= $1 ORDER BY id FOR UPDATE`, now)
	if err != nil {
		return err
	}
	type key struct{ id, tenant string }
	var ks []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.id, &k.tenant); err != nil {
			rows.Close()
			return err
		}
		ks = append(ks, k)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range ks {
		r, err := loadTakeover(ctx, tx, k.tenant, k.id, true)
		if err != nil {
			return err
		}
		if r == nil || r.State != "pending" {
			continue
		}
		if err := c.endTakeover(ctx, tx, now, r, "expired", "", "", "expired"); err != nil {
			return err
		}
	}
	return nil
}

// Defaults of the takeover ceilings from v0.3 (MGR-48).
const (
	defaultTakeoverMonthEUR = 10.0
	defaultTakeoverMaxEUR   = 1.5
)

// takeoverLine is the budget line of a takeover: the tenant's takeover line in v0.2, its manager
// line from v0.3, outside the sev1 reserve (MGR-21, MGR-48).
func takeoverLine() string {
	if claimsV03() {
		return "manager"
	}
	return "takeover"
}

// takeoverFits checks the ceilings of v0.3: the estimate within takeover.max_eur, and with the
// tenant's other takeovers of the calendar month, staged, pending or approved, within
// takeover.manager_month_eur (MGR-48). Before v0.3 it always fits.
func takeoverFits(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, estimate int64, self ...string) (bool, error) {
	if !claimsV03() {
		return true, nil
	}
	t := mmap(mmap(tv.Raw, "spec"), "takeover")
	maxOne, monthMax := defaultTakeoverMaxEUR, defaultTakeoverMonthEUR
	if v, ok := mnum(t, "max_eur"); ok {
		maxOne = v
	}
	if v, ok := mnum(t, "manager_month_eur"); ok {
		monthMax = v
	}
	if estimate > money.FromEUR(maxOne) {
		return false, nil
	}
	day := tenantDay(tv, now)
	month := time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())
	var used int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(estimate_micros), 0) FROM takeovers WHERE tenant = $1 AND created_at >= $2
		AND state IN ('staged', 'pending', 'approved') AND NOT id = ANY($3)`, tv.Name, month, append([]string{}, self...)).Scan(&used); err != nil {
		return false, err
	}
	return used+estimate <= money.FromEUR(monthMax), nil
}

// takeoverRole is the functionary role of a task: the executor role its record names, or the
// first role by name that claims its type and is no seat role (MGR-48).
func takeoverRole(cat *Catalog, typ string, rec []byte) string {
	var r struct {
		Role string `json:"executor_role"`
	}
	_ = json.Unmarshal(rec, &r)
	if r.Role != "" {
		return r.Role
	}
	for _, name := range slices.Sorted(mapKeys(cat.Roles)) {
		if slices.Contains(cat.Roles[name].Claims, typ) && !slices.Contains(managerProfiles, profileOf(cat, name)) {
			return name
		}
	}
	return ""
}

// takeoverEvalPassed says whether the eval index has a report of the role on the seat profile
// without a security failure: an entry of the role whose fingerprint is the role's static
// fingerprint with the seat profile as its base model (MGR-48).
func takeoverEvalPassed(cat *Catalog, tenant, role, profile string) (bool, error) {
	if _, ok := cat.Roles[role]; role == "" || !ok {
		return false, nil
	}
	fp, err := takeoverFingerprint(cat, tenant, role, profile)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(catalogEvalReports(cat), func(e evalReport) bool {
		return e.SubjectKind == "role" && e.SubjectName == role && e.Fingerprint == fp && e.Cases > 0 && e.SecurityFailures == 0
	}), nil
}

// takeoverFingerprint is the static fingerprint of a functionary role on the seat profile, the
// subject of the eval report a takeover needs (MGR-48, PRV-3).
func takeoverFingerprint(cat *Catalog, tenant, role, profile string) (string, error) {
	r := cat.Roles[role]
	types := map[string]string{}
	for _, typ := range r.Claims {
		types[typ] = cat.Hashes[manifest.Key{Kind: "TaskType", Name: typ}]
	}
	pin := ""
	position, _ := positionOf(cat, role, "")
	if p := personaOf(mmap(mmap(mmap(tenantSpecOf(cat, "Governance", tenant), "positions"), position), "persona")); p.pinned() {
		pin = p.SHA256
	}
	raw := cat.RawGlobals[manifest.Key{Kind: "Role", Name: role}]
	return staticFingerprint(cat.Hashes[manifest.Key{Kind: "Role", Name: role}], types, pin, profile, mstr(mmap(mmap(raw, "spec"), "models"), "analysis"),
		mstr(mmap(mmap(raw, "spec"), "runtime"), "driver"), r.Skills)
}

// takeoverOf is the original task a takeover task replaces, or "" (MGR-48).
func takeoverOf(rec []byte) string {
	var r struct {
		Of string `json:"takeover_of"`
	}
	_ = json.Unmarshal(rec, &r)
	return r.Of
}
