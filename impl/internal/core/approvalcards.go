// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Approval cards: their chats and approvers, the card body and its updates, and P1 card details and remind.

// cardChat is a direct chat of a person in a channel of the tenant: the first channel, by
// name, whose identity kind the person has. A Telegram private chat has the id of the user.
func cardChat(ctx context.Context, q store.Querier, tv *TenantView, principal string) (string, Chat, bool, error) {
	rows, err := q.Query(ctx, `SELECT identity FROM identities WHERE tenant = $1 AND principal = $2 ORDER BY identity`, tv.Name, principal)
	if err != nil {
		return "", Chat{}, false, err
	}
	var idents []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return "", Chat{}, false, err
		}
		idents = append(idents, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", Chat{}, false, err
	}
	names := make([]string, 0, len(tv.Channels))
	for n := range tv.Channels {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		ch := tv.Channels[n]
		if ch.Direction == "out" || ch.IdentityKind == "" {
			continue
		}
		for _, id := range idents {
			if ext, ok := strings.CutPrefix(id, ch.IdentityKind+":"); ok {
				return n, Chat{Kind: "direct", ExternalID: ext}, true, nil
			}
		}
	}
	return "", Chat{}, false, nil
}

// approvers are the active people of the tenant with action.approve:<a>, without the
// initiator unless the tenant allows self approval (APR-9, decision-points approval_card).
func approvers(ctx context.Context, q store.Querier, now time.Time, tv *TenantView, a *approvalRow) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT DISTINCT m.principal FROM memberships m JOIN principals p ON p.tenant = m.tenant AND p.id = m.principal
		WHERE m.tenant = $1 AND m.state = 'active' AND (m.until IS NULL OR m.until > $2) AND p.state = 'active' AND p.kind = 'human'
		ORDER BY m.principal`, tv.Name, now)
	if err != nil {
		return nil, err
	}
	var all []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	for _, p := range all {
		// With two approvers the initiator is never one of them (APR-16).
		if selfApproveBarred(tv, p, a) {
			continue
		}
		info, err := loadPrincipal(ctx, q, now, tv.Name, p)
		if err != nil {
			return nil, err
		}
		if mayApprove(tv, groupClosure(tv, info.Groups), a) {
			out = append(out, p)
		}
	}
	return out, nil
}

// selfApproveBarred says whether the principal is the initiator of the approval and may not
// approve it: the tenant forbids self approval, as a production tenant always does by lint, or
// the action needs two approvers (APR-9, APR-16).
func selfApproveBarred(tv *TenantView, principal string, a *approvalRow) bool {
	return principal == a.Initiator && (!tv.Spec.SelfApprove || a.Approvals == 2)
}

// approveRefusal is the permission decision of an approval press: why the person may not
// approve it, or "" (APR-4, APR-9). authorize.eval of the test bench runs it too.
func approveRefusal(tv *TenantView, groups []string, principal string, a *approvalRow) string {
	if !mayApprove(tv, groups, a) {
		return "the person has no action.approve for this action"
	}
	if selfApproveBarred(tv, principal, a) {
		return "the initiator does not approve their own request"
	}
	return ""
}

// planCards plans the card to every approver and the copy to the initiator (APR-3, APR-19). A
// person without a direct chat in a channel of the tenant gets no card.
func (c *Core) planCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow) error {
	people, err := approvers(ctx, tx, now, tv, a)
	if err != nil {
		return err
	}
	for _, p := range people {
		if err := c.planCard(ctx, tx, now, tv, a, approvalCard, p, false); err != nil {
			return err
		}
	}
	if strings.HasPrefix(a.Initiator, "human:") {
		if err := c.planCard(ctx, tx, now, tv, a, initiatorCopy, a.Initiator, false); err != nil {
			return err
		}
	}
	_, err = tx.Exec(ctx, `UPDATE approvals SET cards_at = $2, updated_at = $2 WHERE id = $1`, a.ID, now)
	return err
}

// planCard plans one card delivery. The body names only the subject: the card itself is drawn
// from the record when a channel takes the delivery (APR-3).
func (c *Core) planCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *approvalRow, kind, person string, reminder bool) error {
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	body := map[string]any{"form": "card", "card": kind}
	if reminder {
		body["reminder"] = true
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Task: a.SourceTask, Kind: kind, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: body, DataOrigin: "code",
		SubjectKind: "approval", SubjectID: a.ID,
	})
	return err
}

// planCardUpdates plans a card_update for every delivered card of the approval, so each
// recipient sees its new state (spec/lifecycles/approval.md).
func (c *Core) planCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, a *approvalRow) error {
	// Every card message gets its update: the first card, a reminder, the initiator copy.
	rows, err := tx.Query(ctx, `SELECT id, kind, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'approval' AND subject_id = $1 AND kind IN ('approval_card', 'approval_initiator_copy') AND state IN ('planned', 'sending', 'delivered')
		ORDER BY seq`, a.ID)
	if err != nil {
		return err
	}
	type target struct{ id, kind, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.kind, &t.channel, &t.chat, &t.recipient); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: a.Tenant, Task: a.SourceTask, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": t.kind, "of": t.id},
			DataOrigin: "code", SubjectKind: "approval", SubjectID: a.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// buildCard draws a card from the approval record by the code template (APR-3, APR-12, APR-19).
func (c *Core) buildCard(ctx context.Context, q store.Querier, cat *Catalog, a *approvalRow, kind string, reminder bool) (map[string]any, error) {
	act, _ := c.actionOf(cat, a.Action)
	title := act.Title
	if title == "" {
		title = a.Action
	}
	req := map[string]any{"initiator": a.Initiator, "source": a.RequestedBy["kind"]}
	for _, k := range []string{"role", "task", "ref"} {
		if v, ok := a.RequestedBy[k]; ok {
			req[k] = v
		}
	}
	// Principals carry no display name in 0.1: the card shows the principal id.
	display := cut(a.Initiator, 80)
	card := map[string]any{
		"card": kind, "subject_kind": "approval", "id": a.ID, "nonce": a.Nonce, "title": cut(title, 200),
		"args": a.Args, "requested_by": req, "initiator_display": display, "untrusted_seen": a.UntrustedSeen,
		"risk": a.Risk, "expires_at": fmtTime(a.ExpiresAt), "state": cardState(a),
		"footer": map[string]any{"why_you": "action.approve:" + a.Action, "until": fmtTime(a.ExpiresAt), "on_silence": "the action is not executed"},
	}
	if a.UntrustedSource != "" {
		card["untrusted_source"] = cut(a.UntrustedSource, 120)
	}
	if len(a.Context) > 0 {
		card["context"] = a.Context
	}
	// The card shows who has already approved: each vote of two approvers (APR-16).
	if by := approvedBy(a); len(by) > 0 {
		card["approved_by"] = by
	}
	if reminder {
		card["reminder"] = true
	}
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "approval", "id": a.ID, "nonce": a.Nonce}
	}
	buttons := []any{}
	if a.State == "pending" {
		switch kind {
		case approvalCard:
			buttons = append(buttons, button("approve"), button("reject"), button("details"))
		case initiatorCopy:
			card["footer"] = map[string]any{"why_you": "the initiator of the request", "until": fmtTime(a.ExpiresAt), "on_silence": "the action is not executed"}
			buttons = append(buttons, button("remind"))
		}
	}
	card["buttons"] = buttons
	return card, nil
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// approvalContext is the context line: the closed result fields of the source attempt that
// the type marks x-botaru-read-by: code, without model text (APR-19).
func approvalContext(tt manifest.TaskType, result map[string]any) map[string]any {
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	out := map[string]any{}
	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s, _ := props[k].(map[string]any)
		if s["x-botaru-read-by"] != "code" || len(out) >= 16 {
			continue
		}
		switch v := result[k].(type) {
		case string:
			if len([]rune(v)) <= 120 {
				out[k] = v
			}
		case float64, int, int64, bool:
			out[k] = v
		}
	}
	return out
}

// CardOp is p1-card-request: a press of an operation button of a card: Details or Remind approvers.
type CardOp struct {
	Op          string `json:"op"`
	SubjectKind string `json:"subject_kind"`
	ID          string `json:"id"`
	Nonce       string `json:"nonce"`
}

// CardDetails is P1 card.details: the card fields from the code template; it records the
// opening for details_opened (APR-11). There is no model text in an approval record.
func (c *Core) CardDetails(ctx context.Context, w *Worker, ob *OnBehalf, req CardOp) (map[string]any, error) {
	if ob == nil {
		return nil, refuse("forbidden", "card.details needs Botaru-On-Behalf and Botaru-Chat")
	}
	if req.SubjectKind == "proposal" {
		return c.proposalDetails(ctx, w, *ob, req)
	}
	if req.SubjectKind == "governance_case" || req.SubjectKind == "attribution" {
		return c.govDetails(ctx, w, *ob, req)
	}
	if req.SubjectKind == "script" {
		return c.scriptDetails(ctx, w, *ob, req)
	}
	if req.SubjectKind != "approval" {
		return nil, refuse("not_found", "this build has card details only for approvals and proposals")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		a, err := loadApproval(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such approval")
		}
		if err != nil {
			return err
		}
		if a.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, *ob, "approval", a.ID, []string{approvalCard, initiatorCopy})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO card_views (subject_kind, subject_id, principal, opened_at) VALUES ('approval', $1, $2, $3) ON CONFLICT DO NOTHING`,
			a.ID, press.principal, now); err != nil {
			return err
		}
		act, _ := c.actionOf(cat, a.Action)
		fields := map[string]any{
			"action": a.Action, "title": act.Title, "args": a.Args, "risk": a.Risk, "state": a.State,
			"requested_by": a.RequestedBy, "untrusted_seen": a.UntrustedSeen, "expires_at": fmtTime(a.ExpiresAt),
		}
		if len(a.Context) > 0 {
			fields["context"] = a.Context
		}
		if a.UntrustedSource != "" {
			fields["untrusted_source"] = a.UntrustedSource
		}
		out = map[string]any{"template": "card.details.approval", "fields": fields, "deliver_to": "same_chat"}
		return nil
	})
	return out, err
}

// CardRemind is P1 card.remind from the initiator copy: the approvers who have not decided get
// a reminder, at most once in 5 minutes per approval (APR-19).
func (c *Core) CardRemind(ctx context.Context, w *Worker, ob *OnBehalf, req CardOp) (map[string]any, error) {
	if ob == nil {
		return nil, refuse("forbidden", "card.remind needs Botaru-On-Behalf and Botaru-Chat")
	}
	if req.SubjectKind != "approval" {
		return nil, refuse("not_found", "this build has cards only for approvals")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		a, err := loadApproval(ctx, tx, w.Tenant, req.ID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such approval")
		}
		if err != nil {
			return err
		}
		if a.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, *ob, "approval", a.ID, []string{initiatorCopy})
		if err != nil {
			return err
		}
		if press.principal != a.Initiator {
			return refuse("forbidden", "only the initiator reminds the approvers")
		}
		if a.State != "pending" {
			return refuse("already_decided", "the approval is decided")
		}
		if a.RemindAt != nil && now.Sub(*a.RemindAt) < remindEvery {
			p := refuse("service_rate_limited", "a reminder was sent less than 5 minutes ago")
			p.RetryAfterS = int((remindEvery - now.Sub(*a.RemindAt)).Seconds()) + 1
			return p
		}
		if _, err := tx.Exec(ctx, `UPDATE approvals SET remind_at = $2, updated_at = $2 WHERE id = $1`, a.ID, now); err != nil {
			return err
		}
		n, err := c.remindApprovers(ctx, tx, now, press.tv, a)
		if err != nil {
			return err
		}
		out = map[string]any{"reminded": n}
		return nil
	})
	return out, err
}

// approvalCardBody draws the body of a card delivery from the approval record when a channel
// takes it; a card update carries the external id of the message it edits (FEN-7).
func (c *Core) approvalCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	a, err := loadApproval(ctx, tx, tenant, id, false)
	if err != nil {
		return nil, "", err
	}
	cardKind, _ := body["card"].(string)
	reminder, _ := body["reminder"].(bool)
	card, err := c.buildCard(ctx, tx, cat, a, cardKind, reminder)
	if err != nil {
		return nil, "", err
	}
	replaces := ""
	if kind == cardUpdate {
		// The update edits the message of the card delivery it names.
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $6 AND subject_kind = 'approval' AND subject_id = $1 AND kind = $2
			AND exec_name = $3 AND target_id = $4 AND recipient IS NOT DISTINCT FROM $5 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, cardKind, channel, target, nullable(recipient), of).Scan(&ext)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
		if ext == nil {
			return nil, "", nil
		}
		replaces = *ext
	}
	return map[string]any{"form": "card", "card": card}, replaces, nil
}

// approvedBy are the people who approved, in the order of their presses.
func approvedBy(a *approvalRow) []any {
	var out []any
	for _, v := range a.Votes {
		if v.Decision == "approve" {
			out = append(out, v.By)
		}
	}
	if len(out) == 0 && a.State != "pending" && a.Decision == "approve" && a.DecidedBy != "" {
		out = append(out, a.DecidedBy)
	}
	return out
}

// voted says whether a person already pressed this decision on an approval of two approvers.
func (a *approvalRow) voted(principal, decision string) bool {
	for _, v := range a.Votes {
		if v.By == principal && v.Decision == decision {
			return true
		}
	}
	return false
}

// cardState is the state a card shows: paused for a pending approval under pause_action, which
// the channel draws as card.paused (HLD-8), else the approval's state.
func cardState(a *approvalRow) string {
	if a.State == "pending" && a.PausedSince != nil {
		return "paused"
	}
	return a.State
}
