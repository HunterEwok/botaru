// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The decisions on a campaign: the presses on its cards, the retry of failed items and P1
// campaign.control and campaign.get.

// CampaignControlRequest is p1-campaign-control-request: a button of a campaign card.
type CampaignControlRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	N       int    `json:"n,omitempty"`
	Nonce   string `json:"nonce"`
}

// CampaignControlResult is p1-campaign-control-response.
type CampaignControlResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// decideCampaign is P1 decide on the subject campaign: approve starts a proposed campaign, or
// continues a paused one; reject ends a proposed one (ORC-2, APR-4, APR-9).
func (c *Core) decideCampaign(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	var command string
	switch req.Decision {
	case "approve":
		command = "approve"
	case "reject":
		command = "reject"
	case "full_list":
		// Not a decision: the list of every item goes to the person who pressed (ORC-2).
		return c.campaignFullList(ctx, w, ob, req)
	default:
		return nil, refuse("input_schema_invalid", "a campaign is approved or rejected")
	}
	res, err := c.campaignCommand(ctx, w, ob, req.ID, req.Nonce, command, 0)
	if err != nil {
		return nil, err
	}
	return &DecideResult{SubjectKind: "campaign", ID: req.ID, Decision: req.Decision, State: res.State}, nil
}

// CampaignControl is P1 campaign.control: pause, resume, skip, cancel or retry_failed by a
// button of a campaign card, on behalf of the person who pressed it.
func (c *Core) CampaignControl(ctx context.Context, w *Worker, ob *OnBehalf, req CampaignControlRequest) (*CampaignControlResult, error) {
	if ob == nil {
		return nil, refuse("forbidden", "campaign.control needs Botaru-On-Behalf and Botaru-Chat")
	}
	switch req.Command {
	case "pause", "resume", "cancel", "retry_failed":
		if req.N != 0 {
			return nil, refuse("input_schema_invalid", "only skip names an item")
		}
	case "skip":
		if req.N < 1 {
			return nil, refuse("input_schema_invalid", "skip names an item")
		}
	default:
		return nil, refuse("input_schema_invalid", "unknown command")
	}
	return c.campaignCommand(ctx, w, *ob, req.ID, req.Nonce, req.Command, req.N)
}

// campaignCommand applies a person's press on a campaign card. "Pause" and "Cancel" are open to
// the initiator, the people who decide the campaign and the tenant admin; approve, reject,
// "Continue" and "Exclude item" are decisions of a person who decides it, never the initiator
// without self approval; "Retry failed" is the initiator's (ORC-1, ORC-2, ORC-4).
func (c *Core) campaignCommand(ctx context.Context, w *Worker, ob OnBehalf, id, nonce, command string, item int) (*CampaignControlResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *CampaignControlResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cmp, err := loadCampaign(ctx, tx, w.Tenant, id, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such campaign")
		}
		if err != nil {
			return err
		}
		if cmp.Nonce != nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		// The Continue button of a campaign that started without a card is on its progress
		// card and resumes it (ORC-2).
		if command == "approve" && cmp.State == "paused" && !cmp.Card {
			command = "resume"
		}
		kinds := []string{campaignApprovalCard}
		if command == "pause" || command == "cancel" || command == "retry_failed" || command == "resume" && !cmp.Card {
			kinds = []string{campaignApprovalCard, campaignProgressCard, campaignSummaryKind}
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "campaign", cmp.ID, kinds)
		if err != nil {
			return err
		}
		deny := func(reason, detail string) error {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: cmp.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "campaign.control", Target: &Target{Kind: "campaign", ID: cmp.ID}, OK: false, Reason: reason,
				After: map[string]any{"command": command},
			}); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		decider := mayDecideCampaign(press.tv, press.groups, cmp) && (press.principal != cmp.Initiator || press.tv.Spec.SelfApprove)
		out = &CampaignControlResult{ID: cmp.ID}
		ok := func(reason string) error {
			// An item that ends inside the transaction may already have moved the campaign on.
			if err := tx.QueryRow(ctx, `SELECT state FROM campaigns WHERE id = $1`, cmp.ID).Scan(&cmp.State); err != nil {
				return err
			}
			out.State = cmp.State
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: cmp.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "campaign.control", Target: &Target{Kind: "campaign", ID: cmp.ID}, OK: true, Reason: reason,
				After: map[string]any{"command": command, "state": cmp.State},
			})
			return err
		}
		switch command {
		case "approve", "reject":
			if !decider {
				return deny("forbidden", "the person does not decide this campaign")
			}
			if cmp.State == "paused" && command == "approve" {
				return c.resumeCampaign(ctx, tx, now, cat, cmp, press.principal, deny, ok)
			}
			if cmp.State != "proposed" {
				if cmp.DecidedBy == press.principal {
					out.State = cmp.State
					return nil
				}
				return refuse("already_decided", "the campaign is decided")
			}
			if cmp.ExpiresAt != nil && !cmp.ExpiresAt.After(now) {
				if err := c.rejectCampaign(ctx, tx, now, cmp, "expired"); err != nil {
					return err
				}
				return deny("expired", "the card has expired")
			}
			// Holds and lost rights are checked before the decision is written, so a refused
			// approval leaves no decided_by and no decision.made.
			if command == "approve" {
				if held, err := heldBy(ctx, tx, now, []string{"no_action"}, campaignScopes(cmp)...); err != nil {
					return err
				} else if held != "" && cmp.Rec.action() != "" {
					if err := c.rejectCampaign(ctx, tx, now, cmp, "held"); err != nil {
						return err
					}
					return deny("held", "a hold cancels the campaign")
				}
				// A pause_action hold keeps the card waiting: no decision while it holds (HLD-3).
				if held, err := heldBy(ctx, tx, now, []string{"pause_action"}, campaignScopes(cmp)...); err != nil {
					return err
				} else if held != "" && cmp.Rec.action() != "" {
					return deny("held", "a hold pauses the action")
				}
				if stop, err := c.campaignRights(ctx, tx, now, press.tv, cmp); err != nil {
					return err
				} else if stop != "" {
					if err := c.rejectCampaign(ctx, tx, now, cmp, stop); err != nil {
						return err
					}
					return deny("forbidden", "the initiator no longer has the rights")
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE campaigns SET decided_by = $2, updated_at = $3 WHERE id = $1`, cmp.ID, press.principal, now); err != nil {
				return err
			}
			cmp.DecidedBy = press.principal
			if err := c.appendEvent(ctx, tx, now, cmp.Tenant, Event{Type: "decision.made", Subject: cmp.ID, Data: map[string]any{
				"subject": map[string]any{"kind": "campaign", "id": cmp.ID}, "card": campaignApprovalCard, "decision": command,
				"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
				"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now), "response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
				"details_opened": false,
			}}); err != nil {
				return err
			}
			if command == "reject" {
				if err := c.rejectCampaign(ctx, tx, now, cmp, ""); err != nil {
					return err
				}
				return ok("")
			}
			if err := c.startCampaign(ctx, tx, now, cat, press.tv, cmp); err != nil {
				return err
			}
			return ok("")
		case "resume":
			// A campaign that started without a card is resumed by its initiator, who could
			// start it (ORC-2).
			if !decider && (cmp.Card || press.principal != cmp.Initiator) {
				return deny("forbidden", "the person does not decide this campaign")
			}
			return c.resumeCampaign(ctx, tx, now, cat, cmp, press.principal, deny, ok)
		case "skip":
			if !decider {
				return deny("forbidden", "the person does not decide this campaign")
			}
			if !slices.Contains([]string{"proposed", "running", "paused"}, cmp.State) {
				return refuse("already_terminal", "the campaign has ended")
			}
			tag, err := tx.Exec(ctx, `UPDATE campaign_items SET state = 'skipped', ended_at = $3 WHERE campaign = $1 AND n = $2 AND state = 'pending'`, cmp.ID, item, now)
			if err != nil {
				return err
			}
			if tag.RowsAffected() == 0 {
				return refuse("not_found", "no pending item of that number")
			}
			if err := c.campaignCardUpdates(ctx, tx, now, cmp, true); err != nil {
				return err
			}
			if cmp.State == "running" {
				if err := c.releaseItems(ctx, tx, now, cat, cmp); err != nil {
					return err
				}
			}
			return ok("")
		case "pause", "cancel":
			admin := hasCap(press.tv, press.groups, "admin:people")
			if press.principal != cmp.Initiator && !mayDecideCampaign(press.tv, press.groups, cmp) && !admin {
				return deny("forbidden", "only the initiator, a person who decides or an admin")
			}
			if command == "pause" {
				if cmp.State == "paused" {
					return ok("")
				}
				if cmp.State != "running" {
					return refuse("already_terminal", "the campaign is not running")
				}
				if err := c.setCampaignState(ctx, tx, now, cmp, "paused", "human", ""); err != nil {
					return err
				}
				return ok("")
			}
			reason := "admin"
			if press.principal == cmp.Initiator {
				reason = "initiator"
			}
			switch cmp.State {
			case "proposed":
				if err := c.rejectCampaign(ctx, tx, now, cmp, reason); err != nil {
					return err
				}
			case "running", "paused":
				if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
					return err
				}
			default:
				return refuse("already_terminal", "the campaign has ended")
			}
			return ok(reason)
		case "retry_failed":
			if press.principal != cmp.Initiator {
				return deny("forbidden", "only the initiator retries")
			}
			if !slices.Contains([]string{"completed", "failed", "canceled"}, cmp.State) {
				return refuse("already_decided", "the campaign has not ended")
			}
			// Its items kept no arguments past the content period or an erasure (RET-10): every
			// path that strips items marks the campaign purged.
			if cmp.Purged {
				return refuse("already_terminal", "the campaign's content is gone")
			}
			res, err := c.retryCampaign(ctx, tx, now, cat, cmp)
			if err != nil {
				return err
			}
			if res.State == "rejected" {
				return deny(res.Reason, "the retry does not pass the checks")
			}
			out.ID, out.State = res.Campaign, res.State
			_, err = c.appendAudit(ctx, tx, now, Audit{
				Tenant: cmp.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "campaign.control", Target: &Target{Kind: "campaign", ID: cmp.ID}, OK: true,
				After: map[string]any{"command": command, "campaign": res.Campaign},
			})
			return err
		}
		return refuse("input_schema_invalid", "unknown command")
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// resumeCampaign is "Continue": a paused campaign runs again, except under estimate_exceeded,
// and under held only once no hold covers it (ORC-2, ORC-8, ORC-11).
func (c *Core) resumeCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow, who string, deny func(string, string) error, ok func(string) error) error {
	if cmp.State == "running" {
		return ok("")
	}
	if cmp.State != "paused" {
		return refuse("already_terminal", "the campaign is not paused")
	}
	switch cmp.PauseReason {
	case "estimate_exceeded":
		return deny("estimate_exceeded", "a new campaign with a new estimate is needed")
	case "held":
		held, err := heldBy(ctx, tx, now, campaignHoldEffectsOf(cmp), campaignScopes(cmp)...)
		if err != nil {
			return err
		}
		if held != "" {
			return deny("held", "a hold still covers the campaign")
		}
	}
	tv := cat.Tenants[cmp.Tenant]
	stop, err := c.campaignRights(ctx, tx, now, tv, cmp)
	if err != nil {
		return err
	}
	if stop == "" && campaignTwoApprovers(cat, cmp) {
		if err := c.stopTwoApprovers(ctx, tx, now, cat, cmp); err != nil {
			return err
		}
		return ok("admin")
	}
	if stop != "" {
		if err := c.stopCampaign(ctx, tx, now, cat, cmp, stop); err != nil {
			return err
		}
		return ok(stop)
	}
	if cmp.PauseReason == "gate" {
		if err := c.passGate(ctx, tx, cmp); err != nil {
			return err
		}
	}
	if cmp.PauseReason == "no_executor" {
		cmp.Rec.ExecutorFrom = &now
		if _, err := tx.Exec(ctx, `UPDATE campaigns SET record = jsonb_set(record, '{executor_from}', to_jsonb($2::text)) WHERE id = $1`, cmp.ID, fmtTime(now)); err != nil {
			return err
		}
	}
	if err := c.setCampaignState(ctx, tx, now, cmp, "running", "", ""); err != nil {
		return err
	}
	if err := c.releaseItems(ctx, tx, now, cat, cmp); err != nil {
		return err
	}
	_ = who
	return ok("")
}

// campaignTwoApprovers says whether the action of an action campaign now needs two
// approvers, which the one press of a campaign card does not give (APR-16).
func campaignTwoApprovers(cat *Catalog, cmp *campaignRow) bool {
	if cmp.Rec.kind() != "action" {
		return false
	}
	act, ok := cat.Actions[cmp.Rec.action()]
	return ok && act.Approvals >= 2
}

// stopTwoApprovers stops a campaign whose action an admin's catalog change gave a second
// approval: the listed stop reason admin, with the cause in the audit record (APR-16).
func (c *Core) stopTwoApprovers(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow) error {
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: cmp.Tenant, Kind: "command", Actor: Actor{Principal: "system", Via: "core"},
		Command: "campaign.stop", Target: &Target{Kind: "campaign", ID: cmp.ID}, OK: true, Reason: "admin",
		After: map[string]any{"cause": "action_needs_two_approvers", "action": cmp.Rec.action()},
	}); err != nil {
		return err
	}
	return c.stopCampaign(ctx, tx, now, cat, cmp, "admin")
}

// retryCampaign creates the campaign of "Retry failed": the failed, unknown and skipped items of
// an ended campaign with the same template and policy, its untrusted flag, a refs link, no
// continuation and always a new card (ORC-1).
func (c *Core) retryCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow) (campaignOutcome, error) {
	rows, err := tx.Query(ctx, `SELECT resource, args FROM campaign_items WHERE campaign = $1 AND state IN ('failed', 'unknown', 'skipped') ORDER BY n`, cmp.ID)
	if err != nil {
		return campaignOutcome{}, err
	}
	p := campaignPlan{
		Tenant: cmp.Tenant, Initiator: cmp.Initiator, ParentTask: cmp.ParentTask, Role: cmp.Rec.Role, Template: cmp.Rec.Template,
		Kind: cmp.Rec.kind(), Type: cmp.Rec.itemType(), Action: cmp.Rec.action(), Rollout: cmp.Rec.Rollout, Refs: cmp.ID,
		Untrusted: cmp.Rec.Untrusted.Value, Mark: cmp.Rec.DataMark, MaxTier: cmp.Rec.MaxTier, Root: cmp.Rec.Root, Depth: cmp.Rec.Depth,
		Conversation: cmp.Rec.Conversation, Trace: cmp.Rec.Trace, Origin: cmp.Rec.Origin, CoInitiators: cmp.Rec.CoInitiators, WakeTask: cmp.Rec.WakeTask,
		ForceCard: true, Source: "human", Key: "system:retry:" + cmp.ID,
	}
	if cmp.Rec.Untrusted.Source != nil {
		p.UntrustedSource = *cmp.Rec.Untrusted.Source
	}
	for rows.Next() {
		var res string
		var raw []byte
		if err := rows.Scan(&res, &raw); err != nil {
			rows.Close()
			return campaignOutcome{}, err
		}
		var a map[string]any
		_ = json.Unmarshal(raw, &a)
		p.Resources = append(p.Resources, res)
		p.ItemArgs = append(p.ItemArgs, a)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return campaignOutcome{}, err
	}
	if len(p.Resources) == 0 {
		return campaignOutcome{State: "rejected", Reason: "ref_invalid"}, nil
	}
	// The item arguments are the exact ones of the original: the template is not applied twice.
	if p.Kind == "task" {
		p.Template = cloneWithout(p.Template, "params")
	} else {
		// An action item differs from the others only by its target: the shared arguments
		// are the template again, so the card shows them (ORC-2).
		tf := actionTargetParam(cat, cat.Actions[p.Action])
		shared := map[string]any{}
		for k, v := range p.ItemArgs[0] {
			if k != tf {
				shared[k] = v
			}
		}
		p.Template = cloneWithout(p.Template, "args")
		p.Template["args"] = shared
		p.ItemArgs = nil
	}
	return c.admitCampaign(ctx, tx, now, cat, &p)
}

func cloneWithout(m map[string]any, key string) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	return out
}

// CampaignGetRequest is p1-campaign-get-request.
type CampaignGetRequest struct {
	ID string `json:"id"`
}

// CampaignGet is P1 campaign.get: the record without free text for an attempt of its own tree;
// another tenant's or a nonexistent campaign gives the same not_found (PRM-9).
func (c *Core) CampaignGet(ctx context.Context, w *Worker, lease string, req CampaignGetRequest) (map[string]any, error) {
	var out map[string]any
	err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, lease, "campaign.get", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" {
			return refuse("fenced", "a campaign is read under an attempt lease")
		}
		cmp, err := loadCampaign(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such campaign")
		}
		if err != nil {
			return err
		}
		tree, err := loadTree(ctx, tx, l.WorkID)
		if err != nil {
			return err
		}
		if cmp.Rec.Root != tree.Root {
			return refuse("not_found", "no such campaign")
		}
		out, err = cmp.Record(ctx, tx)
		return err
	})
	return out, err
}
