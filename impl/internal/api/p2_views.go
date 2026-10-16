// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// escalationFields are the fields of escalation.list and escalation.show, whose request has no
// schema in the skeleton yet: the fields of p2-tenant-request and p2-object-request.
var escalationFields = map[string][]string{
	"escalation.list": {"op", "tenant", "state", "limit", "after"},
	"escalation.show": {"op", "id"},
}

// p2Views serves the P2 views and controls of campaigns, script runs, proposals and
// escalations; it reports false for an operation it does not serve.
func (s *Server) p2Views(ctx context.Context, oper core.Operator, op string, m map[string]any) (any, bool, error) {
	switch op {
	case "campaign.list", "script.list":
		req, err := decode[core.TenantRequest](m)
		if err != nil {
			return nil, true, err
		}
		if op == "campaign.list" {
			out, err := s.c.CampaignList(ctx, oper, req)
			return out, true, err
		}
		out, err := s.c.ScriptList(ctx, oper, req)
		return out, true, err
	case "campaign.show", "campaign.retry_failed", "script.show", "script.cancel":
		req, err := decode[core.ObjectRequest](m)
		if err != nil {
			return nil, true, err
		}
		var out any
		switch op {
		case "campaign.show":
			out, err = s.c.CampaignShow(ctx, oper, req)
		case "campaign.retry_failed":
			out, err = s.c.CampaignRetryFailed(ctx, oper, req)
		case "script.show":
			out, err = s.c.ScriptShow(ctx, oper, req)
		default:
			out, err = s.c.ScriptCancel(ctx, oper, req)
		}
		return out, true, err
	case "campaign.control":
		req, err := decode[core.P2CampaignControlRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.CampaignControlP2(ctx, oper, req)
		return out, true, err
	case "proposal.list":
		req, err := decode[core.ProposalListRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.ProposalList(ctx, oper, req)
		return out, true, err
	case "proposal.show":
		req, err := decode[core.ProposalShowRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.ProposalShow(ctx, oper, req)
		return out, true, err
	case "escalation.list", "escalation.show":
		for k := range m {
			if !slices.Contains(escalationFields[op], k) {
				return nil, true, &core.Problem{Reason: "input_schema_invalid", Detail: "unknown field " + k}
			}
		}
		if op == "escalation.list" {
			req, err := decode[core.TenantRequest](m)
			if err != nil {
				return nil, true, &core.Problem{Reason: "input_schema_invalid", Detail: "the body does not match p2-tenant-request"}
			}
			out, err := s.c.EscalationList(ctx, oper, req)
			return out, true, err
		}
		req, err := decode[core.ObjectRequest](m)
		if err != nil || req.ID == "" {
			return nil, true, &core.Problem{Reason: "input_schema_invalid", Detail: "the body needs the escalation id"}
		}
		out, err := s.c.EscalationShow(ctx, oper, req)
		return out, true, err
	}
	return nil, false, nil
}
