// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The cards of a script run: the confirmation card with its updates and the gate card (SCR-6).

// planScriptCard plans one run card delivery to a person's direct chat, drawn from the record
// when a channel takes it; the run card goes to the initiator in private even when the
// assignment came from a group (SCR-6, D-48).
func (c *Core) planScriptCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, r *scriptRun, kind, card, person string) error {
	if tv == nil {
		return nil
	}
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	// The run card shows the draft's parameters and arguments, so it carries the run's mark.
	mark := r.Rec.DataMark
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Task: r.Anchor, Kind: kind, Addr: "approval_initiator", TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: map[string]any{"form": "card", "card": card}, DataOrigin: "code",
		DataMark: mark, SubjectKind: "script", SubjectID: r.ID,
	})
	return err
}

// scriptCardUpdates edits the sent run cards: always on a run state change, and on step changes
// at most once in 30 seconds.
func (c *Core) scriptCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, stateChanged bool) error {
	if !stateChanged && r.ProgressAt != nil && now.Sub(*r.ProgressAt) < scriptProgressEvery {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'script' AND subject_id = $1 AND kind = $2 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, r.ID, scriptConfirmCard)
	if err != nil {
		return err
	}
	type target struct{ id, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.channel, &t.chat, &t.recipient); err != nil {
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
			Tenant: r.Tenant, Task: r.Anchor, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": scriptConfirmCard, "of": t.id},
			DataOrigin: "code", SubjectKind: "script", SubjectID: r.ID,
		}); err != nil {
			return err
		}
	}
	at := now
	r.ProgressAt = &at
	_, err = tx.Exec(ctx, `UPDATE script_runs SET progress_at = $2 WHERE id = $1`, r.ID, now)
	return err
}

// buildScriptCard draws the run card from the record in code words (SCR-6): the step count, all
// steps in order with their targets and marks, the estimate, the untrusted flag with its source,
// plan_hash, the deadline and the bottom line; no model text reaches it.
func (c *Core) buildScriptCard(ctx context.Context, q store.Querier, cat *Catalog, r *scriptRun, kind string) (map[string]any, error) {
	steps, err := parseSteps(r.Rec.Script)
	if err != nil {
		return nil, err
	}
	rows, err := loadSteps(ctx, q, r.ID)
	if err != nil {
		return nil, err
	}
	list := make([]any, 0, len(steps))
	for i, s := range steps {
		e := map[string]any{"n": i + 1, "id": s.ID, "kind": s.Kind}
		switch {
		case s.Type != "":
			e["type"] = s.Type
		case s.Action != "":
			e["action"] = s.Action
		}
		if s.Kind == "fanout" {
			e["targets"] = len(r.Rec.fanoutList(s.ID))
		} else if s.Kind != "confirm" {
			e["targets"] = 1
		}
		// The person sees what each step acts on before Run (SCR-6).
		if s.Kind == "fanout" {
			if l := r.Rec.fanoutList(s.ID); len(l) > 0 {
				e["resources"] = l
			}
		}
		if len(s.Args) > 0 {
			e["args"] = s.Args
		}
		if len(s.Params) > 0 {
			e["params"] = s.Params
		}
		if s.When != nil {
			e["condition"] = true
		}
		if len(s.Needs) > 0 {
			e["needs"] = s.Needs
		}
		if m := r.Rec.Marks[s.ID]; len(m) > 0 {
			e["marks"] = m
		}
		if i < len(rows) {
			e["state"] = rows[i].State
		}
		list = append(list, e)
	}
	ctxLine := map[string]any{
		"steps": len(steps), "estimate_eur": strconv.FormatFloat(r.Rec.Estimate.EUR, 'f', 2, 64), "tree_tasks": r.Rec.Estimate.TreeTasks,
		"plan_hash": r.PlanHash, "script_run_required": r.Rec.NeedsRun,
	}
	if r.Reason != "" {
		ctxLine["reason"] = r.Reason
	}
	card := map[string]any{
		"card": kind, "subject_kind": "script", "id": r.ID, "nonce": r.Nonce, "title": fmt.Sprintf("Script of %d steps", len(steps)),
		"args": map[string]any{"steps": list}, "requested_by": map[string]any{"initiator": r.Initiator, "source": "human", "task": r.Anchor},
		"initiator_display": cut(r.Initiator, 80), "untrusted_seen": r.Rec.Untrusted.Value, "state": r.State, "context": ctxLine,
	}
	if r.Anchor == "" {
		delete(card["requested_by"].(map[string]any), "task")
	}
	if src := r.Rec.Untrusted.source(); r.Rec.Untrusted.Value && src != "" {
		card["untrusted_source"] = cut(src, 120)
	}
	footer := map[string]any{"why_you": "the initiator of the script", "on_silence": "the run is rejected and nothing runs"}
	if r.State == "proposed" {
		card["expires_at"] = fmtTime(r.ExpiresAt)
		footer["until"] = fmtTime(r.ExpiresAt)
	} else {
		footer["on_silence"] = "nothing"
		if r.DeadlineAt != nil {
			footer["until"] = fmtTime(*r.DeadlineAt)
		}
	}
	card["footer"] = footer
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "script", "id": r.ID, "nonce": r.Nonce}
	}
	buttons := []any{}
	if kind == scriptConfirmCard {
		switch r.State {
		case "proposed":
			buttons = append(buttons, button("approve"), button("reject"))
		case "running":
			buttons = append(buttons, button("pause"), button("cancel"))
		case "paused":
			buttons = append(buttons, button("approve"), button("cancel"))
		}
	}
	card["buttons"] = buttons
	return card, nil
}

// buildGateCard draws the card of a confirm step (subject script_gate).
func buildGateCard(r *scriptRun, s *stepRow, approversOf string) map[string]any {
	why := "the initiator of the script"
	if approversOf != "" {
		why = "action.approve:" + approversOf
	}
	card := map[string]any{
		"card": scriptGateCard, "subject_kind": "script_gate", "id": s.Gate, "nonce": s.Nonce, "title": fmt.Sprintf("Continue the script at step %d?", s.N),
		"args":              map[string]any{"run": r.ID, "step": s.ID, "n": s.N},
		"requested_by":      map[string]any{"initiator": r.Initiator, "source": "script_step", "ref": r.ID + ":" + s.ID},
		"initiator_display": cut(r.Initiator, 80), "untrusted_seen": s.Untrusted.Value, "state": s.State,
		"footer": map[string]any{"why_you": why, "on_silence": "the step is declined and the steps after it are skipped"},
	}
	if src := s.Untrusted.source(); s.Untrusted.Value && src != "" {
		card["untrusted_source"] = cut(src, 120)
	}
	if s.ExpiresAt != nil {
		card["expires_at"] = fmtTime(*s.ExpiresAt)
		card["footer"].(map[string]any)["until"] = fmtTime(*s.ExpiresAt)
	}
	buttons := []any{}
	if s.State == "waiting" && r.State == "running" || s.State == "waiting" && r.State == "paused" {
		for _, code := range []string{"approve", "reject"} {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "script_gate", "id": s.Gate, "nonce": s.Nonce})
		}
	}
	card["buttons"] = buttons
	return card
}

// scriptCardBody draws a run card, its update, its summary or a confirm step card when a
// channel takes the delivery.
func (c *Core) scriptCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, subjectKind, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	var card map[string]any
	if subjectKind == "script_gate" {
		var runID string
		var n int
		err := tx.QueryRow(ctx, `SELECT s.run, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run WHERE s.gate = $1 AND r.tenant = $2`, id, tenant).Scan(&runID, &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		r, err := loadRun(ctx, tx, tenant, runID, false)
		if err != nil {
			return nil, "", err
		}
		steps, err := loadSteps(ctx, tx, runID)
		if err != nil {
			return nil, "", err
		}
		if r.Rec.Purged {
			return nil, "", nil
		}
		defs, err := parseSteps(r.Rec.Script)
		if err != nil {
			return nil, "", err
		}
		card = buildGateCard(r, steps[n-1], defs[n-1].approversOf())
	} else {
		r, err := loadRun(ctx, tx, tenant, id, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, "", nil
		}
		if err != nil {
			return nil, "", err
		}
		if r.Rec.Purged {
			return nil, "", nil
		}
		cardKind, _ := body["card"].(string)
		if card, err = c.buildScriptCard(ctx, tx, cat, r, cardKind); err != nil {
			return nil, "", err
		}
		if kind == scriptSummaryKind {
			card["buttons"] = []any{}
		}
	}
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $5 AND subject_kind = $6 AND subject_id = $1
			AND exec_name = $2 AND target_id = $3 AND recipient IS NOT DISTINCT FROM $4 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, channel, target, nullable(recipient), of, subjectKind).Scan(&ext)
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

// gateCardUpdates edits the sent cards of a confirm step after its decision.
func (c *Core) gateCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, r *scriptRun, s *stepRow) error {
	rows, err := tx.Query(ctx, `SELECT id, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'script_gate' AND subject_id = $1 AND kind = $2 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, s.Gate, scriptGateCard)
	if err != nil {
		return err
	}
	type target struct{ id, channel, chat, recipient string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.channel, &t.chat, &t.recipient); err != nil {
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
			Tenant: r.Tenant, Task: r.Anchor, Kind: cardUpdate, Addr: "approval_initiator", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": scriptGateCard, "of": t.id},
			DataOrigin: "code", SubjectKind: "script_gate", SubjectID: s.Gate,
		}); err != nil {
			return err
		}
	}
	return nil
}
