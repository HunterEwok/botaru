// SPDX-License-Identifier: AGPL-3.0-only

package api

import (
	"context"

	"github.com/HunterEwok/botaru/impl/internal/core"
)

// p2People serves the P2 task views, invitations, worker rotation and unblocking, budget.set and
// service.control; it reports false for an operation it does not serve.
func (s *Server) p2People(ctx context.Context, oper core.Operator, op string, m map[string]any) (any, bool, error) {
	switch op {
	case "task.show", "task.cancel":
		req, err := decode[core.P2TaskRequest](m)
		if err != nil {
			return nil, true, err
		}
		if op == "task.show" {
			out, err := s.c.TaskShow(ctx, oper, req)
			return out, true, err
		}
		out, err := s.c.TaskCancel(ctx, oper, req)
		return out, true, err
	case "task.list", "team.tasks", "about":
		req, err := decode[core.TaskListRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.TaskList(ctx, oper, req)
		return out, true, err
	case "invite.create", "invite.confirm", "invite.revoke":
		req, err := decode[core.InviteRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.Invite(ctx, oper, req)
		return out, true, err
	case "worker.rotate", "worker.unblock":
		req, err := decode[core.WorkerRequest](m)
		if err != nil {
			return nil, true, err
		}
		if op == "worker.rotate" {
			out, err := s.c.WorkerRotate(ctx, oper, req)
			return out, true, err
		}
		out, err := s.c.WorkerUnblock(ctx, oper, req)
		return out, true, err
	case "budget.set":
		req, err := decode[core.BudgetSetRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.BudgetSet(ctx, oper, req)
		return out, true, err
	case "service.control":
		req, err := decode[core.ServiceControlRequest](m)
		if err != nil {
			return nil, true, err
		}
		out, err := s.c.ServiceControl(ctx, oper, req)
		return out, true, err
	}
	return nil, false, nil
}
