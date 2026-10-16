// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// p2Accountability serves P2 attribution.list and attribution.create (GOV-13) and, from v0.3,
// incident.cause (MGR-8, GOV-38, GOV-41); it reports false for an operation it does not serve.
func (s *Server) p2Accountability(ctx context.Context, oper core.Operator, op string, m map[string]any) (any, bool, error) {
	switch op {
	case "attribution.list", "attribution.create":
		req, err := decode[core.AttributionRequest](m)
		if err != nil {
			return nil, true, err
		}
		if op == "attribution.list" {
			out, err := s.c.AttributionList(ctx, oper, req)
			return out, true, err
		}
		out, err := s.c.AttributionCreate(ctx, oper, req)
		return out, true, err
	case "incident.cause":
		req, err := decode[core.IncidentCauseRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.IncidentCause(ctx, oper, req)
		return out, true, err
	}
	return nil, false, nil
}
