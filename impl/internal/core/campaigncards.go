// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The approval and progress cards of a campaign and the people who decide it (ORC-2).

// noExecutor says whether no worker took a released item of a read campaign by the start_by
// of its task while the dispatcher marks no other waiting reason on it (markWaiting): the campaign pauses with no_executor and releases nothing more until a person
// resumes it, which gives the items a new start_by window (ORC-4). An action item does not
// wait for a toolhost: its write call ends with error(no_executor) and fails the item (DLV-16).
func noExecutor(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow) (bool, error) {
	if cmp.Rec.kind() != "task" {
		return false, nil
	}
	var stuck bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM campaign_items i JOIN tasks t ON t.id = i.task
		WHERE i.campaign = $1 AND i.state = 'released' AND t.state = 'submitted' AND t.lease_no = 0 AND t.start_by_at IS NOT NULL
		AND NOT (coalesce(t.reason, '') = ANY($4))
		AND greatest(t.start_by_at, $2::timestamptz + (t.start_by_at - i.released_at)) <= $3)`, cmp.ID, cmp.Rec.ExecutorFrom, now, waitingReasons).Scan(&stuck)
	return stuck, err
}

// campaignApprovers are the people who decide a campaign: active humans with
// action.approve:<a>, or task:<type> for a read campaign, without the initiator unless the
// tenant allows self approval (ORC-2, APR-9).
func campaignApprovers(ctx context.Context, q store.Querier, now time.Time, tv *TenantView, cmp *campaignRow) ([]string, error) {
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
		if p == cmp.Initiator && !tv.Spec.SelfApprove {
			continue
		}
		groups, err := principalGroups(ctx, q, now, tv, p)
		if err != nil {
			return nil, err
		}
		if mayDecideCampaign(tv, groups, cmp) {
			out = append(out, p)
		}
	}
	return out, nil
}

// mayDecideCampaign says whether groups hold the right to decide a campaign.
func mayDecideCampaign(tv *TenantView, groups []string, cmp *campaignRow) bool {
	if a := cmp.Rec.action(); a != "" {
		return hasCap(tv, groups, "action.approve:"+a)
	}
	return hasCap(tv, groups, "task:"+cmp.Rec.itemType())
}

// planCampaignCards plans the approval card to every approver. A card that shows only the
// first items carries the "full list" button, whose press gets the list as a separate delivery
// (ORC-2, campaignlist.go).
func (c *Core) planCampaignCards(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, cmp *campaignRow) error {
	people, err := campaignApprovers(ctx, tx, now, tv, cmp)
	if err != nil {
		return err
	}
	for _, p := range people {
		if err := c.planCampaignCard(ctx, tx, now, tv, cmp, campaignApprovalCard, campaignApprovalCard, p); err != nil {
			return err
		}
	}
	_ = cat
	return nil
}

// planCampaignCard plans one card delivery of a campaign to a person's direct chat; the card
// is drawn from the record when a channel takes it.
func (c *Core) planCampaignCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, cmp *campaignRow, kind, card, person string) error {
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	body := map[string]any{"form": "card", "card": card}
	if kind == campaignListKind {
		body["list"] = true
	}
	mark := ""
	if kind == campaignSummaryKind {
		mark = cmp.Rec.DataMark
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Task: cmp.ParentTask, Kind: kind, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: body, DataOrigin: "code", DataMark: mark,
		SubjectKind: "campaign", SubjectID: cmp.ID,
	})
	if err == nil && kind == campaignProgressCard {
		_, err = tx.Exec(ctx, `UPDATE campaigns SET progress_at = $2 WHERE id = $1`, cmp.ID, now)
	}
	return err
}

// campaignCardUpdates edits every sent card of a campaign: always on a state change, and for the
// progress card at most once in 30 seconds otherwise (ORC-10).
func (c *Core) campaignCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow, stateChanged bool) error {
	if !stateChanged {
		if cmp.ProgressAt != nil && now.Sub(*cmp.ProgressAt) < campaignProgressEvery {
			return nil
		}
	}
	rows, err := tx.Query(ctx, `SELECT id, kind, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'campaign' AND subject_id = $1 AND kind IN ('campaign_approval', 'campaign_progress') AND state IN ('planned', 'sending', 'delivered')
		ORDER BY seq`, cmp.ID)
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
	if !stateChanged {
		var progress bool
		for _, t := range ts {
			progress = progress || t.kind == campaignProgressCard
		}
		if !progress {
			return nil
		}
	}
	for _, t := range ts {
		if !stateChanged && t.kind != campaignProgressCard {
			continue
		}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: cmp.Tenant, Task: cmp.ParentTask, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": t.kind, "of": t.id},
			DataOrigin: "code", SubjectKind: "campaign", SubjectID: cmp.ID,
		}); err != nil {
			return err
		}
	}
	at := now
	cmp.ProgressAt = &at
	_, err = tx.Exec(ctx, `UPDATE campaigns SET progress_at = $2 WHERE id = $1`, cmp.ID, now)
	return err
}

// buildCampaignCard draws a campaign card from the record by the code template (ORC-2, ORC-10):
// the action or type, the item count with the first items, the rollout in words, the
// threshold, the estimate, who asked, the untrusted flag and the deadline; the progress card
// adds the counters. No model text reaches it.
func (c *Core) buildCampaignCard(ctx context.Context, q store.Querier, cat *Catalog, cmp *campaignRow, kind string) (map[string]any, error) {
	n, err := campaignCount(ctx, q, cmp.ID)
	if err != nil {
		return nil, err
	}
	var first []any
	rows, err := q.Query(ctx, `SELECT resource FROM campaign_items WHERE campaign = $1 ORDER BY n LIMIT $2`, cmp.ID, campaignItemFirstOnCard)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return nil, err
		}
		if !slices.Contains(first, any(r)) {
			first = append(first, r)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	title, risk, why := cmp.Rec.itemType(), "", "task:"+cmp.Rec.itemType()
	args, _ := cmp.Rec.Template["params"].(map[string]any)
	if a := cmp.Rec.action(); a != "" {
		act, _ := c.actionOf(cat, a)
		title, risk, why = a, act.Risk, "action.approve:"+a
		if act.Title != "" {
			title = act.Title
		}
		args, _ = cmp.Rec.Template["args"].(map[string]any)
	} else if tt, ok := cat.TaskTypes[title]; ok && tt.Title != "" {
		title = tt.Title
	}
	source := "bot"
	if len(cmp.Rec.EnqueuedBy) == 0 {
		source = "human"
	}
	req := map[string]any{"initiator": cmp.Initiator, "source": source}
	if cmp.Rec.Role != "" && source == "bot" {
		req["role"] = cmp.Rec.Role
	}
	if cmp.ParentTask != "" {
		req["task"] = cmp.ParentTask
	}
	ctxLine := n.asMap()
	ctxLine["estimate_eur"] = strconv.FormatFloat(cmp.Rec.Reserve.EUR, 'f', 2, 64)
	if cmp.Rec.OverDayLimit {
		ctxLine["over_day_limit"] = true
	}
	if cmp.PauseReason != "" {
		ctxLine["pause_reason"] = cmp.PauseReason
	}
	if cmp.StopReason != "" {
		ctxLine["stop_reason"] = cmp.StopReason
	}
	card := map[string]any{
		"card": kind, "subject_kind": "campaign", "id": cmp.ID, "nonce": cmp.Nonce, "title": cut(title, 200),
		"requested_by": req, "initiator_display": cut(cmp.Initiator, 80), "untrusted_seen": cmp.Rec.Untrusted.Value, "state": cmp.State,
		"campaign": map[string]any{"count": n.total(), "first": first, "rollout": cut(cmp.Rec.Rollout.code(n.total()), 200),
			"threshold": cmp.Rec.Rollout.threshold(n.total()), "plan_hash": cmp.PlanHash},
		"context": ctxLine,
	}
	if len(args) > 0 {
		card["args"] = args
	}
	if risk != "" {
		card["risk"] = risk
	}
	if cmp.Rec.Untrusted.Source != nil {
		card["untrusted_source"] = cut(*cmp.Rec.Untrusted.Source, 120)
	}
	if cmp.DecidedBy != "" {
		card["approved_by"] = []any{cmp.DecidedBy}
	}
	footer := map[string]any{"why_you": why, "on_silence": "the campaign does not start"}
	if cmp.ExpiresAt != nil {
		footer["until"] = fmtTime(*cmp.ExpiresAt)
		if cmp.State == "proposed" {
			card["expires_at"] = fmtTime(*cmp.ExpiresAt)
		}
	}
	if kind == campaignProgressCard {
		footer = map[string]any{"why_you": "the initiator of the campaign", "on_silence": "nothing"}
	}
	card["footer"] = footer
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "campaign", "id": cmp.ID, "nonce": cmp.Nonce}
	}
	buttons := []any{}
	switch cmp.State {
	case "proposed":
		if kind == campaignApprovalCard {
			buttons = append(buttons, button("approve"), button("reject"))
		}
	case "running":
		buttons = append(buttons, button("pause"), button("cancel"))
	case "paused":
		// Continue on the initiator's progress card works only for a campaign without a card.
		if cmp.PauseReason != "estimate_exceeded" && (kind != campaignProgressCard || !cmp.Card) {
			buttons = append(buttons, button("approve"))
		}
		buttons = append(buttons, button("cancel"))
	}
	// The card shows the first items; the full list comes on a press (ORC-2).
	if kind == campaignApprovalCard && n.total() > campaignItemFirstOnCard {
		buttons = append(buttons, button("full_list"))
	}
	card["buttons"] = buttons
	return card, nil
}

// campaignCardBody draws a campaign card, its full list or its update when a channel takes the
// delivery; an update names the card message it edits.
func (c *Core) campaignCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	cmp, err := loadCampaign(ctx, tx, tenant, id, false)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		return nil, "", err
	}
	cardKind, _ := body["card"].(string)
	if kind == campaignListKind {
		var list []string
		rows, err := tx.Query(ctx, `SELECT resource FROM campaign_items WHERE campaign = $1 ORDER BY n`, cmp.ID)
		if err != nil {
			return nil, "", err
		}
		for rows.Next() {
			var r string
			if err := rows.Scan(&r); err != nil {
				rows.Close()
				return nil, "", err
			}
			list = append(list, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, "", err
		}
		return map[string]any{"form": "text_code", "text_key": "card.campaign.list", "params": map[string]any{"count": len(list), "items": strings.Join(list, ", ")}}, "", nil
	}
	card, err := c.buildCampaignCard(ctx, tx, cat, cmp, cardKind)
	if err != nil {
		return nil, "", err
	}
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $6 AND subject_kind = 'campaign' AND subject_id = $1 AND kind = $2
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
