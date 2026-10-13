// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
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
}

// Done is done-response.
type Done struct {
	OK       bool   `json:"ok"`
	ID       string `json:"id,omitempty"`
	AuditSeq int64  `json:"audit_seq,omitempty"`
	State    string `json:"state,omitempty"`
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
	if _, _, err := c.requireTenant(ctx, op.Tenant); err != nil {
		return nil, err
	}
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var state string
		err := tx.QueryRow(ctx, `SELECT state FROM principals WHERE tenant = $1 AND id = $2 FOR UPDATE`, op.Tenant, req.Subject).Scan(&state)
		exists := err == nil
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
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
			if _, err := tx.Exec(ctx, `INSERT INTO principals (tenant, id, kind, state, locale, created_by, created_at) VALUES ($1, $2, $3, 'active', $4, $5, $6)`,
				op.Tenant, req.Subject, kind, nullable(req.Locale), op.Principal, now); err != nil {
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
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: req.Op, Target: &Target{Kind: "principal", ID: req.Subject}, OK: true,
			After: map[string]any{"state": state},
		})
		out = Done{OK: true, AuditSeq: seq, State: state}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
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
}

// Grant runs grant and ungrant: a membership of a principal in a group of the tenant.
func (c *Core) Grant(ctx context.Context, op Operator, req GrantRequest) (*Done, error) {
	tv, _, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	if _, ok := tv.Groups[req.Group]; !ok {
		return nil, refuse("not_found", "no such group in the tenant")
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var ok bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM principals WHERE tenant = $1 AND id = $2)`, op.Tenant, req.Subject).Scan(&ok); err != nil {
			return err
		}
		if !ok {
			return refuse("not_found", "no such principal")
		}
		var until any
		if req.Until != "" {
			t, err := time.Parse(time.RFC3339Nano, req.Until)
			if err != nil {
				return refuse("input_schema_invalid", "until")
			}
			until = t
		}
		switch req.Op {
		case "grant":
			if _, err := tx.Exec(ctx, `INSERT INTO memberships (tenant, principal, grp, state, until, created_at) VALUES ($1, $2, $3, 'active', $4, $5)
				ON CONFLICT (tenant, principal, grp) DO UPDATE SET until = EXCLUDED.until`, op.Tenant, req.Subject, req.Group, until, now); err != nil {
				return err
			}
		case "ungrant":
			if _, err := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant = $1 AND principal = $2 AND grp = $3`, op.Tenant, req.Subject, req.Group); err != nil {
				return err
			}
		default:
			return refuse("input_schema_invalid", "unknown op")
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: req.Op, Target: &Target{Kind: "membership", ID: req.Subject + "/" + req.Group}, OK: true,
		})
		out = Done{OK: true, AuditSeq: seq}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
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
