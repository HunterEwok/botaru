// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Operator is a caller of P2 with operator credentials of the deployment (PRM-14). The
// reference implementation scopes every operator credential to one tenant, because the P2
// request bodies of people operations carry no tenant (TEN-2): farmctl picks the credential of
// the tenant it works on.
type Operator struct {
	Principal string
	Tenant    string
}

// PrincipalRequest is p2-principal-request.
type PrincipalRequest struct {
	Op       string `json:"op"`
	Subject  string `json:"subject"`
	Kind     string `json:"kind"`
	Identity string `json:"identity"`
	Locale   string `json:"locale"`
	// UploadsPersonal declares a service principal's uploads personal (ART-6); unset follows
	// the tenant kind.
	UploadsPersonal *bool `json:"uploads_personal,omitempty"`
}

// Done is done-response.
type Done struct {
	OK       bool   `json:"ok"`
	ID       string `json:"id,omitempty"`
	AuditSeq int64  `json:"audit_seq,omitempty"`
	State    string `json:"state,omitempty"`
	// OperatorSteps are the steps outside the farm the operator does after the operation, as
	// codes (LCY-1, D-103).
	OperatorSteps []string `json:"operator_steps,omitempty"`
}

func (c *Core) requireTenant(ctx context.Context, name string) (*TenantView, *Catalog, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, nil, err
	}
	tv, ok := cat.Tenants[name]
	if !ok || tv.Spec.Kind == "" {
		return nil, nil, refuse("not_found", "tenant is not applied")
	}
	return tv, cat, nil
}

// Principal runs principal.add, principal.off, principal.on and principal.set_identity.
func (c *Core) Principal(ctx context.Context, op Operator, req PrincipalRequest) (*Done, error) {
	tv, _, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2 FOR UPDATE`, op.Tenant, req.Subject).Scan(&state)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		// An offboarded principal is final: no operation leaves the state (LCY-8).
		if exists && state == "offboarded" {
			if req.Op == "principal.offboard" {
				// A repeat gives the same state and changes nothing (IDM-3).
				out = Done{OK: true, State: state}
				return nil
			}
			return refuse("subject_closed", "the principal is offboarded")
		}
		prev := state
		after := map[string]any{}
		switch req.Op {
		case "principal.add":
			if exists {
				// A repeat with the same arguments gives the same answer (IDM-3).
				out = Done{OK: true, ID: "", State: state}
				return nil
			}
			kind := req.Kind
			if kind == "" {
				kind = "human"
			}
			if _, err := tx.Exec(ctx, `INSERT INTO principals (tenant, id, kind, state, locale, created_by, created_at, uploads_personal) VALUES ($1, $2, $3, 'active', $4, $5, $6, $7)`,
				op.Tenant, req.Subject, kind, nullable(req.Locale), op.Principal, now, req.UploadsPersonal); err != nil {
				return err
			}
			state = "active"
			if req.Identity != "" {
				if err := c.setIdentity(ctx, tx, op.Tenant, req.Subject, req.Identity); err != nil {
					return err
				}
			}
		case "principal.off", "principal.on":
			if !exists {
				return refuse("not_found", "no such principal")
			}
			state = map[string]string{"principal.off": "off", "principal.on": "active"}[req.Op]
			if _, err := tx.Exec(ctx, `UPDATE principals SET state = $3 WHERE tenant = $1 AND id = $2`, op.Tenant, req.Subject, state); err != nil {
				return err
			}
			if err := c.revokeWorkOf(ctx, tx, now, tv, req.Subject); err != nil {
				return err
			}
			// The notice goes when the person actually leaves, not on a repeated off (IDN-13).
			if req.Op == "principal.off" && prev != "off" {
				n, err := c.planPrincipalLeft(ctx, tx, now, tv, req.Subject, "person_inactive")
				if err != nil {
					return err
				}
				after["principal_left"] = c.noticeCount(tv.Name, req.Subject, n)
			}
		case "principal.offboard":
			if !exists {
				return refuse("not_found", "no such principal")
			}
			erasure, n, err := c.offboard(ctx, tx, now, tv, op.Principal, req.Subject)
			if err != nil {
				return err
			}
			state = "offboarded"
			after["erasure_request"] = erasure
			after["principal_left"] = c.noticeCount(tv.Name, req.Subject, n)
		case "principal.set_identity":
			if !exists {
				return refuse("not_found", "no such principal")
			}
			if req.Identity == "" {
				return refuse("input_schema_invalid", "identity is required")
			}
			if err := c.setIdentity(ctx, tx, op.Tenant, req.Subject, req.Identity); err != nil {
				return err
			}
		default:
			return refuse("input_schema_invalid", "unknown op")
		}
		after["state"] = state
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: req.Op, Target: &Target{Kind: "principal", ID: req.Subject}, OK: true,
			After: after,
		})
		out = Done{OK: true, AuditSeq: seq, State: state}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// noticeCount is the number of planned principal_left notices for the audit record. With no
// admin with admin:people reachable in a private chat the notice cannot go; the audit record
// shows 0 and the core logs a warning, so the operator learns of the departure (IDN-13).
func (c *Core) noticeCount(tenant, subject string, n int) int {
	if n == 0 {
		c.log.Warn("principal_left has no recipient: no active admin with admin:people has a private chat with a channel of the tenant",
			"tenant", tenant, "subject", subject)
	}
	return n
}

func (c *Core) setIdentity(ctx context.Context, tx pgx.Tx, tenant, subject, identity string) error {
	var owner string
	err := tx.QueryRow(ctx, `SELECT principal FROM identities WHERE tenant = $1 AND identity = $2`, tenant, identity).Scan(&owner)
	if err == nil && owner != subject {
		return refuse("forbidden", "the identity belongs to another principal")
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	// One identity per kind and principal: a new telegram identity replaces the old one.
	kind := identity[:indexByte(identity, ':')+1]
	if _, err := tx.Exec(ctx, `DELETE FROM identities WHERE tenant = $1 AND principal = $2 AND identity LIKE $3 AND identity <> $4`,
		tenant, subject, kind+"%", identity); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO identities (tenant, identity, principal) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`, tenant, identity, subject)
	return err
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return len(s) - 1
}

// GrantRequest is p2-grant-request.
type GrantRequest struct {
	Op      string `json:"op"`
	Subject string `json:"subject"`
	Group   string `json:"group"`
	Until   string `json:"until"`
	// Code is the person's verification code that the operator checked by voice or in person;
	// RequestedBy the admin who asked for a privileged grant in writing (PRM-13).
	Code        string `json:"code,omitempty"`
	RequestedBy string `json:"requested_by,omitempty"`
}

// Grant runs grant and ungrant: a membership of a principal in a group of the tenant.
func (c *Core) Grant(ctx context.Context, op Operator, req GrantRequest) (*Done, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	if _, ok := tv.Groups[req.Group]; !ok {
		return nil, refuse("not_found", "no such group in the tenant")
	}
	// A privileged group needs the verification code and, on a production tenant, the written
	// request of another admin; until the code is checked the membership is pending (PRM-13).
	privileged := c.privilegedNeedsCode(tv, cat, req.Group)
	var out Done
	var refusal *Problem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2 FOR UPDATE`, op.Tenant, req.Subject).Scan(&state)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such principal")
		}
		if err != nil {
			return err
		}
		// An offboarded principal gets no group (LCY-8).
		if state == "offboarded" && req.Op == "grant" {
			return refuse("subject_closed", "the principal is offboarded")
		}
		var until any
		if req.Until != "" {
			t, err := time.Parse(time.RFC3339Nano, req.Until)
			if err != nil {
				return refuse("input_schema_invalid", "until")
			}
			until = t
		}
		mstate := "active"
		switch req.Op {
		case "grant":
			if privileged {
				if p, err := c.privilegedGrantCheck(ctx, tx, now, tv, op, req); err != nil || p != nil {
					// The refusal keeps its audit record: the transaction commits it.
					refusal = p
					return err
				}
				if req.Code == "" {
					mstate = "pending"
				}
			}
			// A pending membership becomes active only with the code; an active one stays.
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, until, created_at, granted_by, requested_by) VALUES ($1, $2, $3, $6, $4, $5, $7, $8)
				ON CONFLICT (tenant, principal, grp) DO UPDATE SET until = EXCLUDED.until,
				state = CASE WHEN memberships.state = 'active' THEN 'active' ELSE EXCLUDED.state END,
				pending_until = CASE WHEN memberships.state = 'active' OR EXCLUDED.state = 'active' THEN NULL ELSE memberships.pending_until END,
				granted_by = EXCLUDED.granted_by, requested_by = coalesce(EXCLUDED.requested_by, memberships.requested_by)`,
				op.Tenant, req.Subject, req.Group, until, now, mstate, op.Principal, nullable(req.RequestedBy)); err != nil {
				return err
			}
			var invite *string
			if err := tx.QueryRow(ctx, `SELECT state, invite FROM memberships WHERE tenant = $1 AND principal = $2 AND grp = $3`, op.Tenant, req.Subject, req.Group).Scan(&mstate, &invite); err != nil {
				return err
			}
			// The code checked here also confirms the invitation the membership waited on, once
			// none of its groups waits any more (PRM-13).
			if invite != nil && mstate == "active" {
				if _, err := tx.Exec(ctx, `UPDATE invitations SET state = 'confirmed' WHERE id = $1 AND state = 'used'
					AND NOT EXISTS (SELECT 1 FROM memberships WHERE tenant = $2 AND principal = $3 AND invite = $1 AND state = 'pending')`,
					*invite, op.Tenant, req.Subject); err != nil {
					return err
				}
			}
		case "ungrant":
			if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant = $1 AND principal = $2 AND grp = $3`, op.Tenant, req.Subject, req.Group); err != nil {
				return err
			}
			if err := c.revokeWorkOf(ctx, tx, now, tv, req.Subject); err != nil {
				return err
			}
		default:
			return refuse("input_schema_invalid", "unknown op")
		}
		var after map[string]any
		if req.Op == "grant" {
			after = map[string]any{"state": mstate}
			if privileged {
				after["privileged"] = true
			}
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: req.Op, Target: &Target{Kind: "membership", ID: req.Subject + "/" + req.Group}, OK: true, After: after,
		})
		if err != nil {
			return err
		}
		out = Done{OK: true, AuditSeq: seq}
		if req.Op == "grant" {
			out.State = mstate
		}
		// Every admin hears of an active grant and of a removal of a privileged group (PRM-13).
		if privileged && (req.Op == "ungrant" || mstate == "active") {
			return c.notifyAdmins(ctx, tx, now, tv, "notice.privileged_"+req.Op, req.Subject, req.Group, op.Principal)
		}
		// And of a grant and a removal of a delegated group (PRM-16), as from a chat.
		if c.delegatedGroup(tv, cat, req.Group) {
			return c.notifyAdmins(ctx, tx, now, tv, "notice.delegated_"+req.Op, req.Subject, req.Group, op.Principal)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if refusal != nil {
		return nil, refusal
	}
	return &out, nil
}

// unprivilegedCaps are the capability forms that never make a group privileged
// (spec/model/capabilities.yaml), bare or with their parameter; task:<type> is privileged from
// tier 1. kb.write is privileged: never as well (PRM-13, KNW-4).
var unprivilegedCaps = map[string]bool{"task.read": true, "team.view": true, "msg.send": true, "kb.write": true}

func unprivilegedCap(cp string) bool {
	form, _, _ := strings.Cut(cp, ":")
	return unprivilegedCaps[form]
}

// privilegedGroup applies the PRM-13 rule to a group and the groups it inherits: a
// capability marked privileged, a task type of tier 1 or higher, or a data class above
// internal. Anything unknown counts as privileged.
func privilegedGroup(tv *TenantView, cat *Catalog, name string, seen map[string]bool) bool {
	if seen[name] {
		return false
	}
	seen[name] = true
	g, ok := tv.Groups[name]
	if !ok {
		return true
	}
	for _, cp := range g.Caps {
		if unprivilegedCap(cp) {
			continue
		}
		typ, isTask := strings.CutPrefix(cp, "task:")
		if !isTask {
			return true
		}
		tt, ok := cat.TaskTypes[typ]
		if !ok || tt.Tier >= 1 {
			return true
		}
	}
	for _, d := range g.Data {
		if d != "summary" && d != "internal" {
			return true
		}
	}
	for _, in := range g.Inherits {
		if privilegedGroup(tv, cat, in, seen) {
			return true
		}
	}
	return false
}

// principalInfo is what the permission check reads about a principal.
type principalInfo struct {
	ID     string
	Kind   string
	State  string
	Locale string
	Groups []string
}

func loadPrincipal(ctx context.Context, q store.Querier, now time.Time, tenant, id string) (*principalInfo, error) {
	p := &principalInfo{ID: id}
	var locale *string
	err := q.QueryRow(ctx, `SELECT kind, state, locale FROM principals WHERE tenant = $1 AND id = $2`, tenant, id).Scan(&p.Kind, &p.State, &locale)
	if err != nil {
		return nil, err
	}
	if locale != nil {
		p.Locale = *locale
	}
	rows, err := q.Query(ctx, `SELECT grp FROM memberships WHERE tenant = $1 AND principal = $2 AND state = 'active' AND (until IS NULL OR until > $3) ORDER BY grp`, tenant, id, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, err
		}
		p.Groups = append(p.Groups, g)
	}
	return p, rows.Err()
}

func principalByIdentity(ctx context.Context, q store.Querier, tenant, identity string) (string, error) {
	var id string
	err := q.QueryRow(ctx, `SELECT principal FROM identities WHERE tenant = $1 AND identity = $2`, tenant, identity).Scan(&id)
	return id, err
}
