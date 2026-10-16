// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// ServiceControlRequest is p2-service-control-request.
type ServiceControlRequest struct {
	Service string `json:"service"`
	Verb    string `json:"verb"`
}

// ServiceControl is P2 service.control: the /service <verb> <service> of a person with their own
// capability service.control:<service> (PRM-18, APR-18), here the person of the P2 credential.
// The checks are those of the channel command: a service of the catalog declared controllable
// and a verb of its control list, the person's right, holds, rate limits and one operation per
// service at a time. A verb of no_card runs under a service_grant with the person as author and
// exactly one write call; any other verb opens an approval on the person's behalf, decided by
// another person. Only an active human principal gives the command. The answer is the closed
// service-control-result: an accepted grant answers done with the state unknown, since the
// toolhost runs the call afterwards; the outcome is in the audit and the grant's task.
func (c *Core) ServiceControl(ctx context.Context, op Operator, req ServiceControlRequest) (*serviceResult, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out *serviceResult
	var refusal *Problem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out, refusal = nil, nil
		actor := Actor{Principal: op.Principal, Via: "farmctl"}
		deny := func(code, reason string, needs ...string) error {
			out = &serviceResult{Code: code, Needs: needs}
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: tv.Name, Kind: "authorize", Actor: actor, Command: "service.control",
				Target: &Target{Kind: "service", ID: req.Service}, OK: false, Reason: reason,
			})
			return err
		}
		svc, ok := findControllable(cat, tv.Name, req.Service)
		if !ok || !slices.Contains(svc.Control, req.Verb) {
			return refuse("input_schema_invalid", "no such controllable service or verb")
		}
		p, err := loadPrincipal(ctx, tx, now, tv.Name, op.Principal)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if p == nil || p.State != "active" || p.Kind != "human" {
			// The command is accepted only from a person (PRM-18); the refusal is audited and
			// the transaction commits it.
			refusal = refuse("forbidden", "service.control is given by an active person")
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: tv.Name, Kind: "authorize", Actor: actor, Command: "service.control",
				Target: &Target{Kind: "service", ID: req.Service}, OK: false, Reason: "forbidden",
			})
			return err
		}
		groups := groupClosure(tv, p.Groups)
		cp := "service.control:" + req.Service
		if !hasCap(tv, groups, cp) {
			return deny("forbidden", "forbidden", cp)
		}
		s := serviceCall{Tenant: tv.Name, Service: svc, Verb: req.Verb, Initiator: op.Principal, Groups: groups, Person: op.Principal}
		if code, err := c.serviceGuards(ctx, tx, now, s); err != nil {
			return err
		} else if code != "" {
			if code == "held" {
				code = "forbidden"
			}
			return deny(code, code)
		}
		argsHash, err := jcs.Hash(s.args())
		if err != nil {
			return err
		}
		if !slices.Contains(svc.NoCard, req.Verb) {
			id, code, err := c.openApproval(ctx, tx, now, tv, approvalRow{
				Tenant: tv.Name, Initiator: op.Principal, Action: serviceAction, ArgsHash: argsHash, Args: s.args(), Risk: "low",
				RequestedBy: map[string]any{"kind": "human"}, ExpiresAt: now.Add(time.Hour), Mark: c.personMark(tv, groups),
			})
			if err != nil {
				return err
			}
			if code != "pending" {
				// Past the card limit the answer is service_rate_limited (PRM-18).
				return deny(code, code)
			}
			ap, err := loadApproval(ctx, tx, tv.Name, id, true)
			if err != nil {
				return err
			}
			if ap.CardsAt == nil {
				if err := c.planCards(ctx, tx, now, tv, ap); err != nil {
					return err
				}
			}
			out = &serviceResult{Code: "approval_required", ApprovalID: id}
			return nil
		}
		author := map[string]any{"kind": grantAuthorKind(op.Principal), "principal": op.Principal}
		grant, err := c.writeGrant(ctx, tx, now, tv.Name, author, actor, map[string]any{"service": req.Service, "verb": req.Verb}, argsHash)
		if err != nil {
			return err
		}
		// The action task has no reply chat: the answer goes back on P2 (CNT-7).
		taskID, err := c.insertActionTask(ctx, tx, now, actionTaskInput{
			Tenant: tv.Name, Action: serviceAction, Args: s.args(), Initiator: op.Principal, GrantID: grant,
		})
		if err != nil {
			return err
		}
		if _, err := c.insertWriteCall(ctx, tx, now, cat, writeCall{
			Tenant: tv.Name, Task: taskID, Action: serviceAction, Act: serviceActionDef(), Args: s.args(), ArgsHash: argsHash, GrantID: grant,
		}); err != nil {
			return err
		}
		out = &serviceResult{Code: "done", State: "unknown", GrantID: grant}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	return out, nil
}
