// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// An external program is a service principal with its own key and groups, like a person (D-47).
// The deployment issues the key and keeps only its hash with the tenant and the key id; the
// core takes the tenant and the principal from that credential, never from the body (TEN-2):
// the principal is the one with the identity api:<key_id> in the tenant (IDN-4). The program
// calls P1 enqueue, task.get and cancel.

// Program is an authenticated external program.
type Program struct {
	Tenant    string
	Principal string
	KeyID     string
}

// AuthenticateProgram finds the service principal of a program key of the deployment.
func (c *Core) AuthenticateProgram(ctx context.Context, tenant, keyID string) (*Program, error) {
	id, err := principalByIdentity(ctx, c.st.Pool, tenant, "api:"+keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("unauthenticated", "the program key has no principal")
	}
	if err != nil {
		return nil, err
	}
	var kind, state string
	if err := c.st.Pool.QueryRow(ctx, `SELECT kind, state FROM principals WHERE tenant = $1 AND id = $2`, tenant, id).Scan(&kind, &state); err != nil {
		return nil, err
	}
	if kind != "service" {
		return nil, refuse("unauthenticated", "the program key names no service principal")
	}
	if state != "active" {
		// A turned-off program reads and cancels nothing either (IDM-1).
		return nil, refuse("person_inactive", "the program is turned off")
	}
	return &Program{Tenant: tenant, Principal: id, KeyID: keyID}, nil
}

// EnqueueRequest is enqueue-request.
type EnqueueRequest struct {
	Body           map[string]any `json:"body"`
	IdempotencyKey string         `json:"idempotency_key"`
	Refs           []string       `json:"refs,omitempty"`
	Priority       int            `json:"priority,omitempty"`
	To             *struct {
		Group string `json:"group"`
	} `json:"to,omitempty"`
	Scope []string `json:"scope,omitempty"`
	Mode  string   `json:"mode,omitempty"`
}

// TaskRef is p1-enqueue-response.
type TaskRef struct {
	ID       string `json:"id"`
	Existing bool   `json:"existing"`
}

// ProgramEnqueue is P1 enqueue of an external program (TEN-2, IDM-1, TEN-7, PRM-6, BUD-1,
// DSP-7): the initiator is the program's principal, the request fields only narrow. A refusal
// takes no key and is written to the audit inside the enqueue.
func (c *Core) ProgramEnqueue(ctx context.Context, p *Program, req EnqueueRequest) (*TaskRef, error) {
	// The dry run of PRM-11 is deferred: the field is refused, never served.
	if req.Mode != "" {
		return nil, refuse("version_unsupported", "mode: validate (PRM-11) is not served by this version")
	}
	// A program's root task has no parent scope to narrow (PRM-6).
	if len(req.Scope) > 0 {
		return nil, refuse("input_schema_invalid", "scope narrows only a child's scope")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[p.Tenant]
	if tv == nil {
		return nil, refuse("not_ready", "the tenant is not applied")
	}
	typ, _ := req.Body["type"].(string)
	var out *TaskRef
	var refusal *Problem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out, refusal = nil, nil
		// A program has no bot group it may address: every group gets the same refusal as a
		// foreign or missing one (DSP-4).
		if req.To != nil {
			refusal = refuse("group_invalid", "no such group for this sender")
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: p.Tenant, Kind: "authorize", Actor: Actor{Principal: p.Principal, Via: "core"},
				Command: "task.enqueue", Target: &Target{Kind: "task_type", ID: orDash(typ)}, OK: false, Reason: refusal.Reason,
			})
			return err
		}
		// The request only lowers the type's class, and class 2 is only a person's direct
		// request: a program's task takes the type's class, 3 by default (DSP-5).
		prio := cat.TaskTypes[typ].Dispatch.PriorityClass
		if prio == 0 {
			prio = 3
		}
		prio = max(req.Priority, prio)
		res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
			Tenant: p.Tenant, Type: typ, Body: req.Body, IdempotencyKey: req.IdempotencyKey, Initiator: p.Principal,
			EnqueuedBy: "program:api:" + p.KeyID, Priority: prio, Source: "interactive", Refs: req.Refs, OwnRefs: true,
		})
		if err != nil {
			return err
		}
		if res.Rejected != nil {
			refusal = res.Rejected
			return nil
		}
		out = &TaskRef{ID: res.TaskID, Existing: res.Existing}
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

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// programActor is the audit actor of a program's call.
func programActor(p *Program) Actor { return Actor{Principal: p.Principal, Via: "core"} }

// programActive says whether the program's principal is active.
func programActive(ctx context.Context, tx pgx.Tx, p *Program) (bool, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2`, p.Tenant, p.Principal).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return state == "active", err
}

// ownedRef says whether a reference of a sender's request is an object of the tenant that the
// initiator created or uploaded: its task, a file it uploaded or one its task produced, or its
// campaign (TEN-7). A foreign and a missing object both answer false.
func ownedRef(ctx context.Context, tx pgx.Tx, tenant, initiator, ref string) (bool, error) {
	var n int
	var err error
	switch {
	case strings.HasPrefix(ref, "file:"):
		err = tx.QueryRow(ctx, `SELECT count(*) FROM files f WHERE f.file_ref = $1 AND f.tenant = $2 AND f.state = 'stored' AND f.origin <> 'export'
			AND ((f.origin = 'upload' AND f.producer = $3) OR (f.origin = 'produced' AND EXISTS
				(SELECT 1 FROM tasks t WHERE t.id = f.producer AND t.tenant = $2 AND t.initiator = $3)))`, ref, tenant, initiator).Scan(&n)
	case strings.HasPrefix(ref, "t_"):
		err = tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE id = $1 AND tenant = $2 AND initiator = $3 AND body IS NOT NULL`, ref, tenant, initiator).Scan(&n)
	case strings.HasPrefix(ref, "cmp_"):
		err = tx.QueryRow(ctx, `SELECT count(*) FROM campaigns WHERE id = $1 AND tenant = $2 AND initiator = $3`, ref, tenant, initiator).Scan(&n)
	}
	return n > 0, err
}

// LeaseEnqueue is P1 enqueue of a job or agent worker under its attempt lease: a child of a
// type from the role's may_enqueue, with the checks and the key system:child:<parent>:<body
// hash> of enqueue_child (PRM-6, IDM-2). The child takes everything else from its parent, so
// the request carries only the type, the goal and the params; refs, a group address, a scope
// and attachments are refused (TEN-7: a child gets only the parent's attachments).
func (c *Core) LeaseEnqueue(ctx context.Context, w *Worker, header string, req EnqueueRequest) (*TaskRef, error) {
	if w.Kind != "job" && w.Kind != "agent" {
		return nil, refuse("forbidden", "only a job or agent attempt enqueues a child")
	}
	if req.Mode != "" {
		// The dry run is a sender's check; a lease holder never gets it (PRM-11).
		return nil, refuse("forbidden", "mode: validate is not open to a lease holder (PRM-11)")
	}
	if len(req.Refs) > 0 || req.To != nil || len(req.Scope) > 0 || req.Priority != 0 {
		return nil, refuse("input_schema_invalid", "a child takes refs, address, scope and priority from its parent")
	}
	args := map[string]any{}
	for k, v := range req.Body {
		if k != "type" && k != "goal" && k != "params" {
			return nil, refuse("input_schema_invalid", "a child body has only type, goal and params")
		}
		args[k] = v
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *TaskRef
	var refusal *Problem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out, refusal = nil, nil
		l, _, err := c.heldLease(ctx, tx, w, header, "enqueue", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" || l.State != "active" || !l.Until.After(now) {
			return refuse("fenced", "a child is enqueued under a live attempt lease")
		}
		g := &toolsGrant{Tenant: l.Tenant, Task: l.WorkID, Worker: w.ID, Role: w.Role, LeaseNo: l.LeaseNo, P1: true, ChildKey: req.IdempotencyKey}
		if err := c.loadGrantTask(ctx, tx, g); err != nil {
			return err
		}
		res, err := c.enqueueChild(ctx, tx, now, cat, g, true, args)
		if err != nil {
			return err
		}
		if res.Code != "enqueued" {
			refusal = refuse(res.Code, "the child is refused")
			refusal.Needs = res.Needs
			return nil
		}
		out = &TaskRef{ID: res.ID, Existing: res.Existing}
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
