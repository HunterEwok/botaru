// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The decisions on a script run: the presses on its cards and P1 script.control and script.get.

// decideScript is P1 decide on the subject script: only the initiator presses "Run", once,
// before the deadline, with script.run when the script needs it (SCR-6); approve on a paused
// run resumes it, reject on a proposed one rejects it.
func (c *Core) decideScript(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	var command string
	switch req.Decision {
	case "approve":
		command = "approve"
	case "reject":
		command = "reject"
	default:
		return nil, refuse("input_schema_invalid", "a script run is approved or rejected")
	}
	res, err := c.scriptCommand(ctx, w, ob, req.ID, req.Nonce, command)
	if err != nil {
		return nil, err
	}
	return &DecideResult{SubjectKind: "script", ID: req.ID, Decision: req.Decision, State: res.State}, nil
}

// ScriptControlRequest is p1-script-control-request: a button of the run card.
type ScriptControlRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
	Nonce   string `json:"nonce"`
}

// ScriptControlResult is p1-script-control-response.
type ScriptControlResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ScriptControl is P1 script.control: "Pause", "Resume" and "Cancel" of the run card, pressed
// by the initiator (SCR-11).
func (c *Core) ScriptControl(ctx context.Context, w *Worker, ob *OnBehalf, req ScriptControlRequest) (*ScriptControlResult, error) {
	if ob == nil {
		return nil, refuse("forbidden", "script.control needs Botaru-On-Behalf and Botaru-Chat")
	}
	switch req.Command {
	case "pause", "resume", "cancel":
	default:
		return nil, refuse("input_schema_invalid", "unknown command")
	}
	return c.scriptCommand(ctx, w, *ob, req.ID, req.Nonce, req.Command)
}

// scriptCommand applies a press on a run card. Every button of the run is the initiator's.
func (c *Core) scriptCommand(ctx context.Context, w *Worker, ob OnBehalf, id, nonce, command string) (*ScriptControlResult, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *ScriptControlResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadRun(ctx, tx, w.Tenant, id, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such script run")
		}
		if err != nil {
			return err
		}
		if r.Nonce != nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		if r.Rec.Purged {
			// A tombstone has no script left to act on (RET-10).
			return refuse("already_terminal", "the run has ended")
		}
		kinds := []string{scriptConfirmCard}
		if command != "approve" && command != "reject" {
			kinds = []string{scriptConfirmCard, scriptSummaryKind}
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "script", r.ID, kinds)
		if err != nil {
			return err
		}
		audit := func(ok bool, reason string) error {
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: r.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "script.control", Target: &Target{Kind: "script", ID: r.ID}, OK: ok, Reason: reason,
				After: map[string]any{"command": command, "state": r.State},
			})
			return err
		}
		deny := func(reason, detail string) error {
			if err := audit(false, reason); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		if press.principal != r.Initiator {
			return deny("forbidden", "only the initiator decides a script run")
		}
		out = &ScriptControlResult{ID: r.ID}
		done := func() error {
			if err := tx.QueryRow(ctx, `SELECT state FROM script_runs WHERE id = $1`, r.ID).Scan(&r.State); err != nil {
				return err
			}
			out.State = r.State
			return audit(true, "")
		}
		terminal := !slices.Contains(scriptLive, r.State)
		switch command {
		case "approve":
			if r.State == "paused" {
				return c.resumeRun(ctx, tx, now, cat, r, deny, done)
			}
			if r.State != "proposed" {
				if terminal {
					return refuse("already_terminal", "the run has ended")
				}
				return refuse("already_decided", "the run was started")
			}
			if !r.ExpiresAt.After(now) {
				if err := c.rejectRun(ctx, tx, now, r, "expired"); err != nil {
					return err
				}
				return &committed{p: refuse("expired", "the card has expired")}
			}
			if r.Rec.NeedsRun && !hasCap(press.tv, press.groups, "script.run") {
				return deny("forbidden", "this script needs script.run")
			}
			defs, err := parseSteps(r.Rec.Script)
			if err != nil {
				return err
			}
			// A read-only run still runs under no_action on its own role (HLD-9).
			if held, err := runHeldBy(ctx, tx, now, r, c.scriptActs(cat, defs), []string{"no_enqueue", "no_action"}, scriptScopes(r, defs)); err != nil {
				return err
			} else if held != "" {
				return deny("held", "a hold covers the script")
			}
			if err := c.decisionEvent(ctx, tx, now, w, ob, press, "script", r.ID, scriptConfirmCard, command); err != nil {
				return err
			}
			reason, err := c.startRun(ctx, tx, now, cat, press.tv, r)
			if err != nil {
				return err
			}
			if reason != "" {
				return &committed{p: refuse(reason, "the run did not start")}
			}
			return done()
		case "reject":
			if r.State != "proposed" {
				if terminal {
					return refuse("already_terminal", "the run has ended")
				}
				return refuse("already_decided", "the run was started")
			}
			if err := c.decisionEvent(ctx, tx, now, w, ob, press, "script", r.ID, scriptConfirmCard, command); err != nil {
				return err
			}
			if err := c.rejectRun(ctx, tx, now, r, ""); err != nil {
				return err
			}
			return done()
		case "pause":
			switch r.State {
			case "running":
				if err := c.setRunState(ctx, tx, now, r, "paused", ""); err != nil {
					return err
				}
			case "paused":
			default:
				return refuse("already_terminal", "the run is not running")
			}
			return done()
		case "resume":
			if r.State == "running" {
				return done()
			}
			if r.State != "paused" {
				return refuse("already_terminal", "the run is not paused")
			}
			return c.resumeRun(ctx, tx, now, cat, r, deny, done)
		case "cancel":
			if terminal {
				return refuse("already_terminal", "the run has ended")
			}
			if r.State == "proposed" {
				if err := c.rejectRun(ctx, tx, now, r, ""); err != nil {
					return err
				}
			} else if err := c.cancelRun(ctx, tx, now, cat, r, "initiator"); err != nil {
				return err
			}
			return done()
		}
		return nil
	})
	return out, err
}

// resumeRun continues a paused run: the deadline has not passed and the initiator is active
// (SCR-11).
func (c *Core) resumeRun(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *scriptRun, deny func(string, string) error, done func() error) error {
	if r.DeadlineAt != nil && !r.DeadlineAt.After(now) {
		return deny("deadline_exceeded", "the run deadline has passed")
	}
	// A hold that still covers the run keeps it paused (SCR-10).
	if held, err := c.runHeld(ctx, tx, now, cat, r); err != nil {
		return err
	} else if held != "" {
		return deny("held", "a hold covers the run")
	}
	if err := c.setRunState(ctx, tx, now, r, "running", ""); err != nil {
		return err
	}
	if err := c.releaseSteps(ctx, tx, now, cat, r); err != nil {
		return err
	}
	return done()
}

// decisionEvent writes decision.made for a press on a card (APR-11).
func (c *Core) decisionEvent(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, ob OnBehalf, press *cardPress, kind, id, card, decision string) error {
	return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "decision.made", Subject: id, Data: map[string]any{
		"subject": map[string]any{"kind": kind, "id": id}, "card": card, "decision": decision,
		"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
		"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now), "response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0),
		"details_opened": false,
	}})
}

// decideGate is P1 decide on the subject script_gate: "Continue" makes the confirm step ok,
// "Stop" declines it; the initiator decides, or a person with action.approve:<a> when the step
// says so (SCR-11).
func (c *Core) decideGate(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "approve" && req.Decision != "reject" {
		return nil, refuse("input_schema_invalid", "a confirm step is approved or rejected")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var runID string
		var n int
		err := tx.QueryRow(ctx, `SELECT s.run, s.n FROM script_steps s JOIN script_runs r ON r.id = s.run WHERE s.gate = $1 AND r.tenant = $2`,
			req.ID, w.Tenant).Scan(&runID, &n)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such confirm step")
		}
		if err != nil {
			return err
		}
		r, err := loadRun(ctx, tx, w.Tenant, runID, true)
		if err != nil {
			return err
		}
		steps, err := loadSteps(ctx, tx, runID)
		if err != nil {
			return err
		}
		if r.Rec.Purged || n > len(steps) {
			// A tombstone has no script left to read the approvers from (RET-10).
			return refuse("already_terminal", "the run has ended")
		}
		s := steps[n-1]
		if s.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "script_gate", s.Gate, []string{scriptGateCard})
		if err != nil {
			return err
		}
		defs, err := parseSteps(r.Rec.Script)
		if err != nil {
			return err
		}
		if n > len(defs) {
			return refuse("already_terminal", "the run has ended")
		}
		may := press.principal == r.Initiator
		if a := defs[n-1].approversOf(); a != "" {
			may = hasCap(press.tv, press.groups, "action.approve:"+a)
		}
		if !may {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: r.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"}, Command: "decide",
				Target: &Target{Kind: "script_gate", ID: s.Gate}, OK: false, Reason: "forbidden",
			}); err != nil {
				return err
			}
			return &committed{p: refuse("forbidden", "the person does not decide this step")}
		}
		if !slices.Contains(scriptLive, r.State) {
			return refuse("already_terminal", "the run has ended")
		}
		if s.State != "waiting" {
			return refuse("already_decided", "the step is decided")
		}
		if err := c.decisionEvent(ctx, tx, now, w, ob, press, "script_gate", s.Gate, scriptGateCard, req.Decision); err != nil {
			return err
		}
		state := "ok"
		if req.Decision == "reject" {
			state = "declined"
		}
		if err := c.endStep(ctx, tx, now, r, s, state, "", nil, scriptUntrusted{}); err != nil {
			return err
		}
		if err := c.gateCardUpdates(ctx, tx, now, r, s); err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "script_gate", ID: s.Gate, Decision: req.Decision, State: state}
		return nil
	})
	return out, err
}

// ScriptGetRequest is p1-script-get-request.
type ScriptGetRequest struct {
	ID string `json:"id"`
}

// ScriptGet is P1 script.get: the run record without free text for an attempt of its own tree;
// another tenant's or a nonexistent run gives the same not_found (PRM-9).
func (c *Core) ScriptGet(ctx context.Context, w *Worker, lease string, req ScriptGetRequest) (map[string]any, error) {
	var out map[string]any
	err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, lease, "script.get", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" {
			return refuse("fenced", "a script run is read under an attempt lease")
		}
		r, err := loadRun(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such script run")
		}
		if err != nil {
			return err
		}
		tree, err := loadTree(ctx, tx, l.WorkID)
		if err != nil {
			return err
		}
		if r.Rec.Root != tree.Root || r.Rec.Purged {
			return refuse("not_found", "no such script run")
		}
		out, err = r.Record(ctx, tx)
		return err
	})
	return out, err
}
