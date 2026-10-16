// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Cards and their attention rules of v0.2: "Details" before the listed decisions (APR-14) and
// the pause of approvals under a pause_action hold (HLD-8).

// decisionPoint is the row of a card kind in spec/model/decision-points.yaml, or nil.
func (c *Core) decisionPoint(kind string) map[string]any {
	reg, err := c.spec.Registry("spec/model/decision-points.yaml")
	if err != nil {
		return nil
	}
	e, _ := reg.Entry(kind)
	return e
}

// detailsRequired says whether a decision on a card of this kind is accepted only after the
// person opened "Details" of the card: details_required: true of its row (APR-14).
func (c *Core) detailsRequired(cardKind, decision string) bool {
	d, _ := c.decisionPoint(cardKind)["decisions"].(map[string]any)
	row, _ := d[decision].(map[string]any)
	return row["details_required"] == true
}

// requireDetails refuses a decision with details_not_opened when the decision of this card
// kind needs "Details" and this person did not open them for the subject before deciding
// (APR-14). An opening is the record of P1 card.details in card_views; every subject that
// decides on a card calls it before it records the decision, so a new subject with a listed
// decision only needs its card.details to write the opening. The refusal is recorded in the
// audit and the card answer asks to open "Details" first (card.answer.details_first).
func (c *Core) requireDetails(ctx context.Context, tx pgx.Tx, now time.Time, tenant, subjectKind, id, cardKind, decision, principal string) error {
	if !c.detailsRequired(cardKind, decision) {
		return nil
	}
	opened, err := detailsOpened(ctx, tx, subjectKind, id, principal)
	if err != nil || opened {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "decision", Actor: Actor{Principal: principal, Via: "channel"}, Command: "decide",
		Target: &Target{Kind: subjectKind, ID: id}, OK: false, Reason: "details_not_opened",
		After: map[string]any{"decision": decision, "card": cardKind},
	}); err != nil {
		return err
	}
	return &committed{p: refuse("details_not_opened", "open \"Details\" of the card before this decision")}
}

// detailsOpened says whether a person opened "Details" of a subject's card.
func detailsOpened(ctx context.Context, q store.Querier, subjectKind, id, principal string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM card_views WHERE subject_kind = $1 AND subject_id = $2 AND principal = $3)`,
		subjectKind, id, principal).Scan(&ok)
	return ok, err
}

// maxPause is how long the pause_action holds extend one approval in total (HLD-8, D-108).
const maxPause = 7 * 24 * time.Hour

// pauseScopes are the scopes that hold approvals, on which a person may set pause_action
// (HLD-8).
var pauseScopes = map[string]bool{"tenant": true, "role": true, "tasktype": true, "action": true, "host": true, "resource": true, "bot_group": true, "bot": true}

// approvalHoldScopes are the hold scopes of a pending approval: the farm, the tenant, the
// action, the role and type of the request, the bot of the attempt that asked, and the machine
// and catalog resource of the write call its arguments name (HLD-3, HLD-8, MON-9).
func (c *Core) approvalHoldScopes(ctx context.Context, tx pgx.Tx, cat *Catalog, a *approvalRow) ([]scopeRef, error) {
	typ, bot := "", ""
	if a.SourceTask != "" {
		var t, b *string
		err := tx.QueryRow(ctx, `SELECT type, record->>'acting_bot' FROM tasks WHERE id = $1`, a.SourceTask).Scan(&t, &b)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		typ, bot = deref(t), deref(b)
	}
	role, _ := a.RequestedBy["role"].(string)
	scopes := approvalScopes(a.Tenant, a.Action, role, typ)
	if bot != "" {
		scopes = append(scopes, scopeRef{kind: "bot", tenant: a.Tenant, name: bot})
	}
	if act, ok := c.actionOf(cat, a.Action); ok && a.Action != serviceAction {
		if pl, ok := cat.Plugins[act.Executor.Plugin]; ok {
			if t, ok := pl.Tool(act.Executor.Tool); ok {
				params := map[string]map[string]any{}
				props, _ := act.Args["properties"].(map[string]any)
				for k, v := range props {
					if m, ok := v.(map[string]any); ok {
						params[k] = m
					}
				}
				if target, _, err := c.writeTarget(cat, a.Tenant, t, params, a.Args); err == nil {
					scopes = append(scopes, targetScopes(a.Tenant, target)...)
				}
			}
		}
	}
	return scopes, nil
}

// syncPause keeps the pause of a pending approval in step with the pause_action holds on its
// scopes and reports whether it is paused now (HLD-8). A pause that begins is recorded and the
// cards get a card_update with card.paused; a pause that ended moves the deadline forward by
// the time the approval was pending under the holds, at most 7 days in total over all pauses,
// and the cards get the new deadline.
func (c *Core) syncPause(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow, scopes []scopeRef) (bool, error) {
	held, err := heldBy(ctx, tx, now, []string{"pause_action"}, scopes...)
	if err != nil {
		return false, err
	}
	if held != "" {
		if a.PausedSince != nil {
			return true, nil
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET paused_since = $2, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
			return false, err
		}
		a.PausedSince = &now
		if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.paused", Subject: a.ID,
			Data: map[string]any{"approval": a.ID, "action": a.Action, "hold": held}}); err != nil {
			return false, err
		}
		return true, c.planCardUpdates(ctx, tx, now, a)
	}
	if a.PausedSince == nil {
		return false, nil
	}
	// The pause lasted until the last pause_action hold on the scopes ended, by its until or
	// by a release, and never past now.
	end := now
	var last *time.Time
	if err := tx.QueryRow(ctx, `SELECT max(least(coalesce(h.ended_at, h.until), $5)) FROM holds h
		JOIN unnest($1::text[], $2::text[], $3::text[]) AS s(k, t, n) ON h.scope_kind = s.k AND h.scope_tenant = s.t AND h.scope_name = s.n
		WHERE 'pause_action' = ANY(h.effects) AND coalesce(h.ended_at, h.until) > $4`,
		scopeCols(scopes, 0), scopeCols(scopes, 1), scopeCols(scopes, 2), *a.PausedSince, now).Scan(&last); err != nil {
		return false, err
	}
	if last != nil && last.Before(end) {
		end = *last
	}
	d := max(end.Sub(*a.PausedSince), 0)
	if left := maxPause - time.Duration(a.PausedTotalS)*time.Second; d > left {
		d = max(left, 0)
	}
	d = d.Truncate(time.Second)
	a.ExpiresAt = a.ExpiresAt.Add(d)
	a.PausedTotalS += int64(d / time.Second)
	a.PausedSince = nil
	if _, err := tx.Exec(ctx, `UPDATE approvals SET paused_since = NULL, paused_total_s = $2, expires_at = $3, updated_at = $4 WHERE id = $1`,
		a.ID, a.PausedTotalS, a.ExpiresAt, now); err != nil {
		return false, err
	}
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.resumed", Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "expires_at": fmtTime(a.ExpiresAt)}}); err != nil {
		return false, err
	}
	return false, c.planCardUpdates(ctx, tx, now, a)
}

// pausedTooLong says whether a paused approval has been pending under pause_action holds for
// 7 days in total, so it expires even while the hold lasts (HLD-8, D-108).
func (a *approvalRow) pausedTooLong(now time.Time) bool {
	if a.PausedSince == nil {
		return false
	}
	return time.Duration(a.PausedTotalS)*time.Second+now.Sub(*a.PausedSince) >= maxPause
}

// scopeCols is one column of a list of scopes: 0 the kind, 1 the tenant, 2 the name.
func scopeCols(scopes []scopeRef, col int) []string {
	out := make([]string, len(scopes))
	for i, s := range scopes {
		out[i] = [3]string{s.kind, s.tenant, s.name}[col]
	}
	return out
}

// doctorTwoApprovers warns about the actions of risk high with approvals: 2 in a production
// tenant that fewer than two active people other than the initiator may approve (APR-16): the
// value is the number of such actions. A person who may both request and approve is counted
// as the initiator, so such an action needs three approvers.
func (c *Core) doctorTwoApprovers(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, add func(DoctorCheck)) error {
	tv := cat.Tenants[tenant]
	if tv == nil || tv.Spec.Kind != "" && tv.Spec.Kind != "production" {
		return nil
	}
	short := 0
	for _, name := range slices.Sorted(mapKeys(cat.Actions)) {
		a := cat.Actions[name]
		if a.Risk != "high" || a.Approvals != 2 {
			continue
		}
		approvers, err := peopleWithCap(ctx, tx, now, tv, "action.approve:"+name)
		if err != nil {
			return err
		}
		requesters, err := peopleWithCap(ctx, tx, now, tv, "action.request:"+name)
		if err != nil {
			return err
		}
		n := len(approvers)
		for _, p := range approvers {
			if slices.Contains(requesters, p) {
				n--
				break
			}
		}
		if n < 2 {
			short++
		}
	}
	ch := DoctorCheck{ID: "approvals.two_approvers_short", OK: short == 0, Severity: "info", Value: short, Tenant: tenant}
	if short > 0 {
		ch.Severity = "warn"
	}
	add(ch)
	return nil
}
