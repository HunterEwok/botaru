// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// A person's decision on an approval: P1 decide from the chat the card went to (APR-4).

// OnBehalf is who pressed a card button and from which chat (on-behalf schema): the channel
// passes it in the headers Botaru-On-Behalf, Botaru-Chat and Botaru-Message.
type OnBehalf struct {
	Identity string
	Chat     Chat
	Message  string
}

// ParseOnBehalf reads the on-behalf headers; it reports false when they are missing or malformed.
func ParseOnBehalf(identity, chat, message string) (OnBehalf, bool) {
	kind, ext, ok := strings.Cut(chat, ":")
	if identity == "" || !ok || ext == "" || (kind != "direct" && kind != "group") || len(identity) > 200 || len(ext) > 128 || len(message) > 128 {
		return OnBehalf{}, false
	}
	if !strings.Contains(identity, ":") {
		return OnBehalf{}, false
	}
	return OnBehalf{Identity: identity, Chat: Chat{Kind: kind, ExternalID: ext}, Message: message}, true
}

// DecideRequest is p1-decide-request.
type DecideRequest struct {
	SubjectKind string `json:"subject_kind"`
	ID          string `json:"id"`
	Nonce       string `json:"nonce"`
	Decision    string `json:"decision"`
	ReasonCode  string `json:"reason_code,omitempty"`
	// Choice is the seat a person chose on promotion_card (GOV-12).
	Choice string `json:"choice,omitempty"`
}

// DecideResult is p1-decide-response.
type DecideResult struct {
	SubjectKind string `json:"subject_kind"`
	ID          string `json:"id"`
	Decision    string `json:"decision"`
	State       string `json:"state,omitempty"`
}

// cardPress is a checked press of a card button: the person and the card delivery to them.
type cardPress struct {
	tv          *TenantView
	principal   string
	groups      []string
	deliveredAt time.Time
}

// checkPress checks a press on behalf of a person (spec/bindings/http/README.md, APR-4): the
// identity kind of the channel's driver, an active human with strong identity in the channel's
// tenant, and a card of this subject delivered by this channel to this person in this chat, on
// this message.
func (c *Core) checkPress(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, ob OnBehalf, subjectKind, id string, kinds []string) (*cardPress, error) {
	if w.Kind != "channel" {
		return nil, refuse("forbidden", "only a channel presses a card")
	}
	tv := cat.Tenants[w.Tenant]
	if tv == nil {
		return nil, refuse("not_ready", "the tenant is not applied")
	}
	ch, ok := tv.Channels[w.Target]
	if !ok || ch.IdentityKind == "" || !strings.HasPrefix(ob.Identity, ch.IdentityKind+":") {
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: w.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "channel"},
			Command: "decide", Target: &Target{Kind: subjectKind, ID: id}, OK: false, Reason: "identity_kind",
		}); err != nil {
			return nil, err
		}
		return nil, refuse("forbidden", "the identity kind does not match the channel driver")
	}
	if ch.IdentityAssurance != "strong" {
		return nil, refuse("forbidden", "a decision needs a channel with strong identity")
	}
	if ob.Chat.Kind != "direct" && ch.MembershipAssurance != "enforced" {
		return nil, refuse("forbidden", "a group chat without enforced membership takes no decisions")
	}
	principal, err := principalByIdentity(ctx, tx, w.Tenant, ob.Identity)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("forbidden", "the person is not a subject of the tenant")
	}
	if err != nil {
		return nil, err
	}
	p, err := loadPrincipal(ctx, tx, now, w.Tenant, principal)
	if err != nil {
		return nil, err
	}
	if p.State != "active" || p.Kind != "human" {
		return nil, refuse("forbidden", "only an active person decides")
	}
	var at time.Time
	err = tx.QueryRow(ctx, `SELECT updated_at FROM deliveries WHERE subject_kind = $1 AND subject_id = $2 AND kind = ANY($3) AND exec_kind = 'channel'
		AND exec_name = $4 AND recipient = $5 AND target_id = $6 AND state = 'delivered' AND ($7 = '' OR external_id = $7)
		ORDER BY seq LIMIT 1`, subjectKind, id, kinds, w.Target, principal, ob.Chat.ID(), ob.Message).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("forbidden", "no card of this subject was delivered to this person in this chat")
	}
	if err != nil {
		return nil, err
	}
	return &cardPress{tv: tv, principal: principal, groups: groupClosure(tv, p.Groups), deliveredAt: at}, nil
}

// Decide is P1 decide on the subject approval (APR-4): once, by a person with the right at the
// moment of the decision, from the chat of the card. An approval executes in the same
// transaction (APR-7).
func (c *Core) Decide(ctx context.Context, w *Worker, ob *OnBehalf, req DecideRequest) (*DecideResult, error) {
	if ob == nil {
		return nil, refuse("forbidden", "decide needs Botaru-On-Behalf and Botaru-Chat")
	}
	switch req.SubjectKind {
	case "approval":
	case "memory_record":
		return c.decideMemory(ctx, w, *ob, req)
	case "subject_request":
		return c.decideSubject(ctx, w, *ob, req)
	case "proposal":
		return c.decideProposal(ctx, w, *ob, req)
	case "control":
		return c.decideControl(ctx, w, *ob, req)
	case "campaign":
		return c.decideCampaign(ctx, w, *ob, req)
	case "script":
		return c.decideScript(ctx, w, *ob, req)
	case "script_gate":
		return c.decideGate(ctx, w, *ob, req)
	case "takeover":
		return c.decideTakeover(ctx, w, *ob, req)
	case "governance_case":
		return c.decideCase(ctx, w, *ob, req)
	case "attribution":
		return c.decideAttribution(ctx, w, *ob, req)
	default:
		return nil, refuse("not_found", "this build decides approvals, memory records, a person's requests, proposals and control cards")
	}
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "an approval is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
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
		press, err := c.checkPress(ctx, tx, now, cat, w, *ob, "approval", a.ID, []string{approvalCard})
		if err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "approval", ID: a.ID, Decision: req.Decision}
		if a.State != "pending" {
			// A repeat of the same decision by the same person gets the same answer (IDM-3).
			if a.DecidedBy == press.principal && a.Decision == req.Decision || a.voted(press.principal, req.Decision) {
				out.State = a.State
				return nil
			}
			if a.State == "expired" {
				return refuse("expired", "the card has expired")
			}
			return refuse("already_decided", "the approval is decided")
		}
		deny := func(reason, detail string) error {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: a.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "decide", Target: &Target{Kind: "approval", ID: a.ID}, OK: false, Reason: reason,
				After: map[string]any{"decision": req.Decision},
			}); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		if detail := approveRefusal(press.tv, press.groups, press.principal, a); detail != "" {
			return deny("forbidden", detail)
		}
		// A repeat of a vote of two approvers changes nothing (IDM-3).
		if a.voted(press.principal, req.Decision) {
			out.State = a.State
			return nil
		}
		scopes, err := c.approvalHoldScopes(ctx, tx, cat, a)
		if err != nil {
			return err
		}
		if held, err := heldBy(ctx, tx, now, []string{"no_action"}, scopes...); err != nil {
			return err
		} else if held != "" {
			if err := c.closeApproval(ctx, tx, now, a, "canceled", "held"); err != nil {
				return err
			}
			return &committed{p: refuse("held", "a hold cancels pending approvals")}
		}
		// Under pause_action a press changes nothing; a pause that ended moves the deadline
		// first (HLD-8).
		if paused, err := c.syncPause(ctx, tx, now, a, scopes); err != nil {
			return err
		} else if paused {
			return &committed{p: refuse("held", "a hold pauses decisions on this approval")}
		}
		if !a.ExpiresAt.After(now) {
			if err := c.closeApproval(ctx, tx, now, a, "expired", "expired"); err != nil {
				return err
			}
			return &committed{p: refuse("expired", "the card has expired")}
		}
		// Holds and the deadline come first; then "Details" before a listed decision (APR-14).
		if err := c.requireDetails(ctx, tx, now, a.Tenant, "approval", a.ID, approvalCard, req.Decision, press.principal); err != nil {
			return err
		}
		var views int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM card_views WHERE subject_kind = 'approval' AND subject_id = $1 AND principal = $2`, a.ID, press.principal).Scan(&views); err != nil {
			return err
		}
		// Two approvers: each press is a vote of its own, and the initiator is checked again at
		// every press (APR-16).
		final := true
		if a.Approvals == 2 {
			if reason, err := c.initiatorLost(ctx, tx, now, press.tv, a); err != nil {
				return err
			} else if reason != "" {
				if err := c.closeApproval(ctx, tx, now, a, "canceled", reason); err != nil {
					return err
				}
				out.State = a.State
				return nil
			}
			a.Votes = append(a.Votes, approvalVote{By: press.principal, Decision: req.Decision, At: fmtTime(now)})
			if _, err := tx.Exec(ctx, `UPDATE approvals SET votes = $2, updated_at = $3 WHERE id = $1`, a.ID, mustJSON(a.Votes), now); err != nil {
				return err
			}
			final = req.Decision == "reject" || len(approvedBy(a)) >= 2
		}
		if final {
			if _, err := tx.Exec(ctx, `UPDATE approvals SET decided_by = $2, decided_at = $3, decision = $4, updated_at = $3 WHERE id = $1`,
				a.ID, press.principal, now, req.Decision); err != nil {
				return err
			}
			a.DecidedBy, a.Decision, a.DecidedAt = press.principal, req.Decision, &now
		}
		ev := map[string]any{
			"subject": map[string]any{"kind": "approval", "id": a.ID}, "card": approvalCard, "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0), "details_opened": views > 0,
		}
		if req.ReasonCode != "" {
			ev["reason_code"] = req.ReasonCode
		}
		if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "decision.made", Subject: a.ID, Data: ev}); err != nil {
			return err
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: a.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "approval", ID: a.ID}, OK: true,
			After: map[string]any{"decision": req.Decision, "action": a.Action},
		}); err != nil {
			return err
		}
		if req.Decision == "reject" {
			if err := c.closeApproval(ctx, tx, now, a, "rejected", ""); err != nil {
				return err
			}
			out.State = "rejected"
			return nil
		}
		if !final {
			// The first of two approvals: the cards show who approved, the request waits for a
			// second, different person.
			out.State = "pending"
			return c.planCardUpdates(ctx, tx, now, a)
		}
		if err := c.executeApproval(ctx, tx, now, cat, press.tv, a); err != nil {
			return err
		}
		out.State = a.State
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
