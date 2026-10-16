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
// person opened "Details" of the card: details_required: true of its row (APR-14). When the
// registry cannot be read it fails closed: every decision needs "Details".
func (c *Core) detailsRequired(cardKind, decision string) bool {
	if _, err := c.spec.Registry("spec/model/decision-points.yaml"); err != nil {
		return true
	}
	d, _ := c.decisionPoint(cardKind)["decisions"].(map[string]any)
	row, _ := d[decision].(map[string]any)
	return row["details_required"] == true
}

// requireDetails refuses a decision with details_not_opened when the decision of this card
// kind needs "Details" and this person did not open them for the subject before deciding
// (APR-14). An opening is the record of P1 card.details in card_views; approvals and proposals
// call it after their holds and expiry and before they record the decision; the other subjects
// check missingDetails at the same point, so a new subject with a listed decision only needs
// its card.details to write the opening. The refusal is recorded in the
// audit and the card answer asks to open "Details" first (card.answer.details_first).
func (c *Core) requireDetails(ctx context.Context, tx pgx.Tx, now time.Time, tenant, subjectKind, id, cardKind, decision, principal string) error {
	if !c.detailsRequired(cardKind, decision) {
		return nil
	}
	opened, err := detailsOpened(ctx, tx, subjectKind, id, principal, time.Time{})
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

// missingDetails says whether a decision on a card of this kind is refused for want of
// "Details" (APR-14): its row needs them and the person did not open them. The subjects that
// record details_opened in their decision (promotion cases, attributions, the audit sample)
// read the opening themselves since their card was shown and refuse with their own audit.
func (c *Core) missingDetails(cardKind, decision string, opened bool) bool {
	return !opened && c.detailsRequired(cardKind, decision)
}

// detailsOpened says whether a person opened "Details" of a subject's card (the P1
// card.details record in card_views) at or after since; the zero since counts any opening.
func detailsOpened(ctx context.Context, q store.Querier, subjectKind, id, principal string, since time.Time) (bool, error) {
	var ok bool
	var err error
	if since.IsZero() {
		err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM card_views WHERE subject_kind = $1 AND subject_id = $2 AND principal = $3)`,
			subjectKind, id, principal).Scan(&ok)
	} else {
		err = q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM card_views WHERE subject_kind = $1 AND subject_id = $2 AND principal = $3 AND opened_at >= $4)`,
			subjectKind, id, principal, since).Scan(&ok)
	}
	return ok, err
}

// maxPause is how long the pause_action holds extend one approval in total (HLD-8, D-108).
const maxPause = 7 * 24 * time.Hour

// pauseScopes are the scopes that hold approvals, on which a person may set pause_action
// (HLD-8).
var pauseScopes = []string{"tenant", "role", "tasktype", "action", "host", "resource", "bot_group", "bot"}

// approvalHoldScopes are the hold scopes of a pending approval: the farm, the tenant, the
// action, the role and type of the request, the bot of the attempt that asked and the bot groups
// of its worker, and the machine
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
	// The bot groups of the worker whose attempt asked, as they stand now (BGR-1).
	if tv := cat.Tenants[a.Tenant]; tv != nil && a.SourceTask != "" && len(tv.BotGroups) > 0 {
		var worker string
		err := tx.QueryRow(ctx, `SELECT worker_id FROM leases WHERE work_id = $1 ORDER BY lease_no DESC LIMIT 1`, a.SourceTask).Scan(&worker)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		if worker != "" {
			_, groups, err := workerGroups(ctx, tx, tv, worker)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, err
			}
			for _, g := range groups {
				scopes = append(scopes, scopeRef{kind: "bot_group", tenant: a.Tenant, name: g})
			}
		}
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
		// The pause starts with the hold, or with the approval when the hold is older.
		var start time.Time
		if err := tx.QueryRow(ctx, `SELECT created_at FROM holds WHERE id = $1`, held).Scan(&start); err != nil {
			return false, err
		}
		start = maxTime(start, a.CreatedAt)
		if start.After(now) {
			start = now
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET paused_since = $2, updated_at = $3 WHERE id = $1`, a.ID, start, now); err != nil {
			return false, err
		}
		a.PausedSince = &start
		if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.paused", Subject: a.ID,
			Data: map[string]any{"approval": a.ID, "action": a.Action, "hold": held}}); err != nil {
			return false, err
		}
		return true, c.planCardUpdates(ctx, tx, now, a)
	}
	if a.PausedSince == nil {
		return false, nil
	}
	// The pause is the union of the intervals of the pause_action holds on the scopes since
	// the pause began, each up to its until or release, and never past now.
	rows, err := tx.Query(ctx, `SELECT greatest(h.created_at, $4), least(coalesce(h.ended_at, h.until, $5), coalesce(h.until, $5), $5) FROM holds h
		JOIN unnest($1::text[], $2::text[], $3::text[]) AS s(k, t, n) ON h.scope_kind = s.k AND h.scope_tenant = s.t AND h.scope_name = s.n
		WHERE 'pause_action' = ANY(h.effects) AND coalesce(h.ended_at, h.until, $5) > $4 AND h.created_at < $5 ORDER BY 1`,
		scopeCols(scopes, 0), scopeCols(scopes, 1), scopeCols(scopes, 2), *a.PausedSince, now)
	if err != nil {
		return false, err
	}
	var d time.Duration
	var curStart, curEnd time.Time
	for rows.Next() {
		var from, to time.Time
		if err := rows.Scan(&from, &to); err != nil {
			rows.Close()
			return false, err
		}
		if !to.After(from) {
			continue
		}
		if curEnd.IsZero() || from.After(curEnd) {
			d += curEnd.Sub(curStart)
			curStart, curEnd = from, to
		} else if to.After(curEnd) {
			curEnd = to
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	d += curEnd.Sub(curStart)
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

// defaultNoncriticalPerDay is attention.noncritical_per_day when the tenant sets none (APR-15).
const defaultNoncriticalPerDay = 10

// decisionKinds are the card kinds of the rows of kind decision in decision-points.yaml: the
// cards that count toward a person's daily limit (APR-15).
func (c *Core) decisionKinds() []string {
	reg, err := c.spec.Registry("spec/model/decision-points.yaml")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range reg.Entries {
		if id, _ := e["id"].(string); e["kind"] == "decision" && e["attention_limit"] != false {
			out = append(out, id)
		}
	}
	return out
}

// cardLimit applies the card limit per person per day at the hand-out of a card (APR-15). A
// card of a decision row with urgent: false waits in planned until the next local day of the
// tenant once the person got noncritical_per_day decision cards today, urgent ones included,
// and the excess is counted per tenant and day without names; deferred cards go out in order
// of the expiry of their subject. It reports whether the delivery waits. A card that goes out
// rewrites, once, the expiry of its subject to count from this first delivery. Notices and
// card updates are not cards of the limit.
func (c *Core) cardLimit(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, id, kind, recipient, subjectKind, subjectID, exec string) (bool, error) {
	if !c.limitedCard(kind, subjectID) {
		return false, nil
	}
	limit := defaultNoncriticalPerDay
	if p := tv.Spec.Attention.NoncriticalPerDay; p != nil {
		limit = *p
	}
	day := tenantDay(tv, now)
	// One count per person at a time, so two claims cannot both pass the limit.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300107))`, tv.Name+"/"+recipient); err != nil {
		return false, err
	}
	var got int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM deliveries WHERE tenant = $1 AND recipient = $2 AND handed_at >= $3 AND kind = ANY($4)`,
		tv.Name, recipient, day, c.decisionKinds()).Scan(&got); err != nil {
		return false, err
	}
	if got >= limit {
		next := day.AddDate(0, 0, 1)
		// A subject whose card has not gone out yet does not expire while it waits: its
		// expiry counts at least from the day the card may go (APR-15). The order of the
		// waiting cards takes the expiry as it stands after that.
		if err := deferSubjectExpiry(ctx, tx, kind, subjectKind, subjectID, next); err != nil {
			return false, err
		}
		exp, err := c.subjectExpiry(ctx, tx, tv, subjectKind, subjectID)
		if err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2, card_expires_at = $3 WHERE id = $1`, id, next, exp); err != nil {
			return false, err
		}
		_, err = tx.Exec(ctx, `INSERT INTO card_excess (tenant, day, count) VALUES ($1, $2, 1) ON CONFLICT (tenant, day) DO UPDATE SET count = card_excess.count + 1`,
			tv.Name, day.Format(time.DateOnly))
		return true, err
	}
	// Cards that waited go out in order of the expiry of their subject.
	var earlier bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries o, deliveries d WHERE d.id = $1 AND d.card_expires_at IS NOT NULL
		AND o.tenant = d.tenant AND o.recipient = d.recipient AND o.exec_name = $2 AND o.state = 'planned' AND o.id <> d.id
		AND o.card_expires_at < d.card_expires_at AND (o.run_at IS NULL OR o.run_at <= $3))`, id, exec, now).Scan(&earlier); err != nil {
		return false, err
	}
	return earlier, nil
}

// limitedCard says whether a delivery is a card of the card limit, whose first delivery sets
// the expiry of its subject (APR-15).
func (c *Core) limitedCard(kind, subjectID string) bool {
	row := c.decisionPoint(kind)
	return row != nil && row["kind"] == "decision" && row["attention_limit"] != false && row["urgent"] != true && subjectID != ""
}

// deferSubjectExpiry moves the expiry of a subject without a first delivery of its card to
// count from next at the earliest. It covers a proposal's presentation, a memory record's
// deadline and the presentation (and so the expiry) of a promotion case or an attribution; a
// person's request keeps its deadline, which counts from the first delivery. The first delivery
// sets the presentation again to its own time, earlier or later than this. self_measured_card
// has its own window, which counts from its first delivery (firstCardDelivery).
func deferSubjectExpiry(ctx context.Context, tx pgx.Tx, kind, subjectKind, id string, next time.Time) error {
	if kind == selfMeasuredCard {
		return nil
	}
	const fresh = `NOT EXISTS (SELECT 1 FROM card_first_deliveries f WHERE f.subject_kind = $3 AND f.subject_id = $1)`
	var err error
	switch subjectKind {
	case "governance_case":
		_, err = tx.Exec(ctx, `UPDATE governance_cases SET expires_at = expires_at + ($2 - presented_at), presented_at = $2
			WHERE id = $1 AND presented_at IS NOT NULL AND expires_at IS NOT NULL AND presented_at < $2 AND `+fresh, id, next, subjectKind)
	case "attribution":
		_, err = tx.Exec(ctx, `UPDATE attributions SET presented_at = $2 WHERE id = $1 AND presented_at IS NOT NULL AND presented_at < $2 AND `+fresh, id, next, subjectKind)
	case "proposal":
		_, err = tx.Exec(ctx, `UPDATE proposals SET presented_at = greatest(presented_at, $2) WHERE id = $1 AND presented_at IS NOT NULL AND `+fresh, id, next, subjectKind)
	case "memory_record":
		_, err = tx.Exec(ctx, `UPDATE memory_items SET decide_by = greatest(decide_by, $2::timestamptz + $4::interval) WHERE id = $1 AND decide_by IS NOT NULL AND `+fresh,
			id, next, subjectKind, memoryCardTTL)
	}
	return err
}

// subjectExpiry is the expiry of the subject of a non-urgent card, or nil.
func (c *Core) subjectExpiry(ctx context.Context, tx pgx.Tx, tv *TenantView, subjectKind, id string) (*time.Time, error) {
	var at *time.Time
	var err error
	switch subjectKind {
	case "proposal":
		var presented *time.Time
		err = tx.QueryRow(ctx, `SELECT coalesce(presented_at, state_at) FROM proposals WHERE id = $1`, id).Scan(&presented)
		if presented != nil {
			t := presented.Add(durationOr(tv.Spec.Insights.DecideTTL, defaultDecideTTL))
			at = &t
		}
	case "memory_record":
		err = tx.QueryRow(ctx, `SELECT decide_by FROM memory_items WHERE id = $1`, id).Scan(&at)
	case "subject_request":
		err = tx.QueryRow(ctx, `SELECT deadline_at FROM subject_requests WHERE id = $1`, id).Scan(&at)
	case "governance_case":
		err = tx.QueryRow(ctx, `SELECT expires_at FROM governance_cases WHERE id = $1`, id).Scan(&at)
	case "attribution":
		var presented *time.Time
		err = tx.QueryRow(ctx, `SELECT presented_at FROM attributions WHERE id = $1`, id).Scan(&presented)
		if presented != nil {
			t := presented.Add(attributionTTL)
			at = &t
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return at, err
}

// firstCardDelivery rewrites, once, the expiry of a subject of a non-urgent card in the
// transaction of the first delivery of its card to any recipient, so the deadline counts from
// it (APR-15): the presentation time of a proposal, the deadline of a memory record set to the
// first delivery plus its card TTL, the deadline of a person's request moved by the time
// since it was created, and the presentation of a promotion case (its expiry moved with it) or
// an attribution, from which its 7 days count; the presentation becomes this delivery even when
// a recipient over the limit moved it to a later day, so "Details" opened now counts (APR-14).
// self_measured_card is a later card on a proposal that was already presented: its mark has
// the card kind as subject kind, and its 7-day window in the proposal record counts from it.
func (c *Core) firstCardDelivery(ctx context.Context, tx pgx.Tx, now time.Time, kind, subjectKind, id string) error {
	if kind == selfMeasuredCard {
		subjectKind = selfMeasuredCard
	}
	tag, err := tx.Exec(ctx, `INSERT INTO card_first_deliveries (subject_kind, subject_id, delivered_at) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		subjectKind, id, now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	switch subjectKind {
	case "proposal":
		// The decide_ttl counts from the presentation, which becomes this first delivery; a
		// proposal taken back from presentation (presented_at NULL) keeps none.
		_, err = tx.Exec(ctx, `UPDATE proposals SET presented_at = $2 WHERE id = $1 AND presented_at IS NOT NULL`, id, now)
	case "memory_record":
		_, err = tx.Exec(ctx, `UPDATE memory_items SET decide_by = $2::timestamptz + $3::interval WHERE id = $1 AND decide_by IS NOT NULL`,
			id, now, memoryCardTTL)
	case "subject_request":
		_, err = tx.Exec(ctx, `UPDATE subject_requests SET deadline_at = deadline_at + ($2 - created_at) WHERE id = $1 AND $2 > created_at`, id, now)
	case "governance_case":
		_, err = tx.Exec(ctx, `UPDATE governance_cases SET expires_at = expires_at + ($2 - presented_at), presented_at = $2
			WHERE id = $1 AND presented_at IS NOT NULL AND expires_at IS NOT NULL`, id, now)
	case "attribution":
		_, err = tx.Exec(ctx, `UPDATE attributions SET presented_at = $2 WHERE id = $1 AND presented_at IS NOT NULL`, id, now)
	case selfMeasuredCard:
		_, err = tx.Exec(ctx, `UPDATE proposals SET record = jsonb_set(record, '{self_measured_card,at}', to_jsonb($2::text))
			WHERE id = $1 AND record ? 'self_measured_card'`, id, fmtTime(now))
	}
	return err
}
