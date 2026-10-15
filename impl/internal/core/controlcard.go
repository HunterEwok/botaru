// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The control card (MGR-26, decision point control_card). A seat operation on a target that is
// not on the code list of its basis and came from a finding is not applied: the core opens a
// card to the people of the tenant with control.approve, drawn from the record without model
// text and marked as coming from untrusted reading. The card expires after an hour and the
// operation is then not applied; an approved one applies through control.apply on behalf of
// the person who decided, with the target origin finding:<id>.

const (
	controlCard    = "control_card"
	controlCardTTL = time.Hour
)

// findingTarget says whether a finding of the tenant for the incident names the target as its
// subject.
func findingTarget(ctx context.Context, tx pgx.Tx, tenant, incident, finding string, t ControlTarget) (bool, error) {
	if finding == "" {
		return false, nil
	}
	// Only a finding of a reading for the operation's own incident counts.
	var kind, id string
	err := tx.QueryRow(ctx, `SELECT subject_kind, subject_id FROM findings WHERE tenant = $1 AND lower(id) = lower($2) AND lower(incident) = lower($3)`,
		tenant, finding, incident).Scan(&kind, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return kind == t.Kind && strings.EqualFold(id, t.ID), nil
}

// openControlCard records the operation on a card and plans it to the tenant's people with
// control.approve. The seat's request is kept as it was; nothing of the model's text is in it.
func (c *Core) openControlCard(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, a controlActor, req ControlRequest, inc *incidentRow, why string) (string, error) {
	raw, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	id := ids.New(ids.Card)
	incident := ""
	if inc != nil {
		incident = inc.ID
	}
	if _, err := tx.Exec(ctx, `INSERT INTO control_cards (id, tenant, request, finding, why, incident, task, acting_bot, epoch, state, nonce, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $12, $5, $6, $7, $8, 'pending', $9, $10, $11, $11)`,
		id, tenant, raw, req.Finding, nullable(incident), nullable(a.Task), nullable(a.Principal), a.Epoch, ids.Secret(16), now.Add(controlCardTTL), now, why); err != nil {
		return "", err
	}
	if tv := cat.Tenants[tenant]; tv != nil {
		chats, err := c.adminChats(ctx, tx, now, tv, "", "control.approve")
		if err != nil {
			return "", err
		}
		for _, ch := range chats {
			if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
				Tenant: tenant, Kind: controlCard, Addr: "admins", TargetKind: "chat",
				TargetID: Chat{Kind: "direct", ExternalID: ch.ExternalID}.ID(), ExecKind: "channel", ExecName: ch.Channel,
				Recipient: ch.Principal, Body: map[string]any{"form": "card", "card": controlCard}, DataOrigin: "code",
				SubjectKind: "control", SubjectID: id,
			}); err != nil {
				return "", err
			}
		}
	}
	_, err = c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "seat", Role: managerSeatID}, Command: "control.card",
		Target: &Target{Kind: req.Target.Kind, ID: req.Target.ID}, OK: true,
		After: map[string]any{"op": req.Op, "card": id, "incident": nullable(incident), "origin": cardOrigin(req.Finding), "why": why, "result": "card", "epoch": a.Epoch},
	})
	return id, err
}

// cardOrigin is the target origin of a card's operation: the finding it came from, or the code
// list of its basis.
func cardOrigin(finding string) string {
	if finding == "" {
		return "code"
	}
	return "finding:" + finding
}

// controlCardRow is a control card as the store keeps it.
type controlCardRow struct {
	ID, Tenant, Finding, Why, Incident, State, Nonce, DecidedBy string
	Request                                                     ControlRequest
	Expires                                                     time.Time
	// FindingFacts are the closed fields of the finding: its kind, confidence and numbers,
	// never its note.
	FindingFacts map[string]any
}

func loadControlCard(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*controlCardRow, error) {
	q := `SELECT id, tenant, finding, why, coalesce(incident, ''), state, nonce, coalesce(decided_by, ''), request, expires_at FROM control_cards WHERE id = $1 AND tenant = $2`
	if lock {
		q += ` FOR UPDATE`
	}
	var r controlCardRow
	var raw []byte
	err := tx.QueryRow(ctx, q, id, tenant).Scan(&r.ID, &r.Tenant, &r.Finding, &r.Why, &r.Incident, &r.State, &r.Nonce, &r.DecidedBy, &raw, &r.Expires)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &r.Request); err != nil {
		return nil, err
	}
	if r.Finding == "" {
		return &r, nil
	}
	var kind string
	var body []byte
	err = tx.QueryRow(ctx, `SELECT kind, body FROM findings WHERE tenant = $1 AND lower(id) = lower($2)`, tenant, r.Finding).Scan(&kind, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		return &r, nil
	}
	if err != nil {
		return nil, err
	}
	var fb map[string]any
	if err := json.Unmarshal(body, &fb); err != nil {
		return nil, err
	}
	r.FindingFacts = map[string]any{"kind": kind}
	if v := mstr(fb, "confidence"); v != "" {
		r.FindingFacts["confidence"] = v
	}
	if v := asMap(fb["values"]); len(v) > 0 {
		r.FindingFacts["values"] = v
	}
	return &r, nil
}

// card draws the control card from the record: the operation, the target, the incident and the
// finding by id, with the label of untrusted reading; never a note or other model text.
func (r *controlCardRow) card() map[string]any {
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "control", "id": r.ID, "nonce": r.Nonce}
	}
	buttons := []any{}
	if r.State == "pending" {
		buttons = append(buttons, button("approve"), button("reject"))
	}
	untrusted := r.Why == "finding" || r.Why == "distrust"
	ctx := map[string]any{"op": r.Request.Op, "target": map[string]any{"kind": r.Request.Target.Kind, "id": r.Request.Target.ID},
		"origin": cardOrigin(r.Finding), "why": r.Why, "label": "farm manager operation for a person's decision"}
	if untrusted {
		ctx["label"] = "manager output from reading tenant data, untrusted"
	}
	if r.Finding != "" {
		ctx["finding"] = r.Finding
	}
	if r.Incident != "" {
		ctx["incident"] = r.Incident
	}
	if r.FindingFacts != nil {
		ctx["finding_facts"] = r.FindingFacts
	}
	card := map[string]any{
		"card": controlCard, "subject_kind": "control", "id": r.ID, "nonce": r.Nonce,
		"title": cut("Farm manager: "+r.Request.Op+" "+r.Request.Target.Kind+" "+r.Request.Target.ID, 200), "state": r.State,
		"buttons": buttons, "context": ctx, "untrusted_seen": untrusted, "expires_at": fmtTime(r.Expires),
		"footer": map[string]any{"why_you": "control.approve", "until": fmtTime(r.Expires), "on_silence": "the operation is not applied"},
	}
	if untrusted {
		card["untrusted_source"] = "finding"
	}
	return card
}

// decideControl is P1 decide on a control card: once, by a person of the tenant with
// control.approve, before the card expires. An approval applies the operation through
// control.apply on behalf of that person, in the same transaction (MGR-26).
func (c *Core) decideControl(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a control card is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadControlCard(ctx, tx, w.Tenant, req.ID, true)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse("not_found", "no such control card")
		}
		if r.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "control", r.ID, []string{controlCard})
		if err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "control", ID: r.ID, Decision: req.Decision, State: r.State}
		if r.State != "pending" {
			if r.DecidedBy == press.principal && (r.State == "rejected") == (req.Decision == "reject") {
				return nil
			}
			if r.State == "expired" {
				return refuse("expired", "the card has expired")
			}
			return refuse("already_decided", "the card is decided")
		}
		if !hasCap(press.tv, press.groups, "control.approve") {
			return refuse("no_group_cap", "the person has no control.approve")
		}
		if !r.Expires.After(now) {
			if err := c.endControlCard(ctx, tx, now, r, "expired", "", "", "expired"); err != nil {
				return err
			}
			return &committed{p: refuse("expired", "the card has expired")}
		}
		state, opID, reason := "rejected", "", ""
		if req.Decision == "approve" && !managerEnabled(cat, r.Tenant) {
			// A tenant that disabled the manager after the card opened gets no seat operation.
			state, reason = "failed", "manager_disabled"
		} else if req.Decision == "approve" {
			// The person applies the operation by the code path of control.apply; the seat's
			// checks of the target do not apply to a person (MGR-26, PRM-14).
			op := Operator{Principal: press.principal, Tenant: r.Tenant}
			d, p, err := c.controlApply(ctx, tx, now, cat, r.Tenant, controlActor{Operator: op, Origin: cardOrigin(r.Finding), ApprovedBy: press.principal}, r.Request)
			if err != nil {
				return err
			}
			state = "applied"
			if p != nil {
				state, reason = "failed", p.Reason
			} else {
				opID = d.ID
			}
		}
		if err := c.endControlCard(ctx, tx, now, r, state, press.principal, opID, reason); err != nil {
			return err
		}
		out.State = state
		if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "decision.made", Subject: r.ID, Data: map[string]any{
			"subject": map[string]any{"kind": "control", "id": r.ID}, "card": controlCard, "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
		}}); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "control", ID: r.ID}, OK: true,
			After: map[string]any{"decision": req.Decision, "state": state, "control": nullable(opID), "reason": nullable(reason)},
		})
		return err
	})
	var cm *committed
	if errors.As(err, &cm) {
		return nil, cm.p
	}
	return out, err
}

// endControlCard puts a card in its final state, plans an update of every delivered copy so
// none keeps its buttons, and logs in the card's incident an outcome that applied nothing; an
// applied operation logs its own line (MGR-13, MGR-26).
func (c *Core) endControlCard(ctx context.Context, tx pgx.Tx, now time.Time, r *controlCardRow, state, by, op, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE control_cards SET state = $2, decided_by = $3, control_op = $4, reason = $5, updated_at = $6 WHERE id = $1`,
		r.ID, state, nullable(by), nullable(op), nullable(reason), now); err != nil {
		return err
	}
	rows, err := tx.Query(ctx, `SELECT id, exec_name, target_id, coalesce(recipient, '') FROM deliveries
		WHERE subject_kind = 'control' AND subject_id = $1 AND kind = $2 AND state IN ('planned', 'sending', 'delivered') ORDER BY seq`, r.ID, controlCard)
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
			ExecKind: "channel", ExecName: d.channel, Recipient: d.recipient, Body: map[string]any{"form": "card", "card": controlCard, "of": d.id},
			DataOrigin: "code", SubjectKind: "control", SubjectID: r.ID,
		}); err != nil {
			return err
		}
	}
	if r.Incident == "" || state == "applied" {
		return nil
	}
	t := r.Request.Target
	return c.noticeLog(ctx, tx, now, r.Incident, noticeEntry{Kind: "card", Op: r.Request.Op, Target: &t, State: state}, true)
}

// controlCardBody draws a control card or its update at hand-out; an update names the
// delivered card message it edits.
func controlCardBody(ctx context.Context, tx pgx.Tx, tenant, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	r, err := loadControlCard(ctx, tx, tenant, id, false)
	if err != nil || r == nil {
		return nil, "", err
	}
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $5 AND subject_kind = 'control' AND subject_id = $1 AND kind = $6
			AND exec_name = $2 AND target_id = $3 AND recipient IS NOT DISTINCT FROM $4 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, channel, target, nullable(recipient), of, controlCard).Scan(&ext)
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

// expireControlCards ends the control cards whose hour passed without a decision: the
// operation is not applied (MGR-26).
func (c *Core) expireControlCards(ctx context.Context, _ *Catalog) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		return c.endPendingCards(ctx, tx, now, `state = 'pending' AND expires_at <= $1`, "expired", "expired", now)
	})
}

// endPendingCards ends the pending control cards a condition selects.
func (c *Core) endPendingCards(ctx context.Context, tx pgx.Tx, now time.Time, where, state, reason string, args ...any) error {
	rows, err := tx.Query(ctx, `SELECT id, tenant FROM control_cards WHERE `+where+` ORDER BY id FOR UPDATE`, args...)
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
		r, err := loadControlCard(ctx, tx, k.tenant, k.id, true)
		if err != nil {
			return err
		}
		if r == nil || r.State != "pending" {
			continue
		}
		if err := c.endControlCard(ctx, tx, now, r, state, "", "", reason); err != nil {
			return err
		}
	}
	return nil
}
