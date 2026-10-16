// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Holds (spec/lifecycles/hold.md). A hold only narrows: it grants no permission, deletes no
// data and changes no manifest (HLD-2). It acts from the next check (HLD-3): every check point
// below asks heldBy in the transaction of the transition, before the permission formula
// (PRM-2). This build has the check points enqueue (no_enqueue), claim (no_claim,
// cancel_running), heartbeat (cancel_running) and the delivery handoff (no_delivery); the
// effects of the ports it does not serve yet (no_tool, no_action, no_message, no_control) are
// recorded and act when those ports come.

// HoldScope is the scope of a hold: a kind from spec/model/holds.yaml, the tenant and the name of
// the scope object. The scope farm has no tenant and no name; the scope tenant has no name.
type HoldScope struct {
	Kind   string `json:"kind"`
	Tenant string `json:"tenant,omitempty"`
	Name   string `json:"name,omitempty"`
}

// HoldSetRequest is p2-hold-set-request.
type HoldSetRequest struct {
	Op      string    `json:"op"`
	Scope   HoldScope `json:"scope"`
	Effects []string  `json:"effects"`
	Reason  string    `json:"reason"`
	Until   *string   `json:"until"`
}

// HoldReleaseRequest is p2-hold-release-request.
type HoldReleaseRequest struct {
	Op string `json:"op"`
	ID string `json:"id"`
}

// TenantRequest is p2-tenant-request.
type TenantRequest struct {
	Op     string `json:"op"`
	Tenant string `json:"tenant"`
	State  string `json:"state"`
	Limit  int    `json:"limit"`
	After  string `json:"after"`
}

// Hold is the hold record (spec/model/hold.schema.json).
type Hold struct {
	ID         string    `json:"id"`
	Tenant     string    `json:"tenant"`
	Scope      HoldScope `json:"scope"`
	Effects    []string  `json:"effects"`
	Reason     string    `json:"reason"`
	Until      *string   `json:"until"`
	Author     string    `json:"author"`
	AuthorKind string    `json:"author_kind"`
	Source     string    `json:"source,omitempty"`
	State      string    `json:"state"`
	CreatedAt  string    `json:"created_at"`
	ReleasedBy string    `json:"released_by,omitempty"`
	EndedAt    string    `json:"ended_at,omitempty"`
}

// HoldList is p2-hold-list-response.
type HoldList struct {
	Items []Hold `json:"items"`
}

// HoldActor is who sets or releases a hold, with the rank of spec/model/holds.yaml: operator
// (farmctl with an operator credential; the credential of farm reaches every tenant), tenant
// admin (a person of the tenant, PRM-15) or contain_auto (core code inside a tenant). A model,
// worker, route or schedule is never a hold actor (HLD-2).
type HoldActor struct {
	Principal string
	Rank      string // operator, tenant_admin, contain_auto
	Tenant    string // the tenant of the credential or of the person; farm for a farm operator
	Via       string // farmctl, channel, system
	Source    string // the auto-stop rule or incident class of a code hold
}

// OperatorActor is the hold actor of a P2 operator credential.
func OperatorActor(op Operator) HoldActor {
	return HoldActor{Principal: op.Principal, Rank: "operator", Tenant: op.Tenant, Via: "farmctl"}
}

// farmScope reports whether a hold of this scope kind is a record of the service tenant farm
// and an operator operation only (HLD-1, PRM-14).
func farmScope(kind string) bool { return kind == "farm" || kind == "seat" }

// recordTenant is the tenant whose record a hold becomes (TEN-1).
func (s HoldScope) recordTenant() string {
	if farmScope(s.Kind) {
		return "farm"
	}
	return s.Tenant
}

// key is the stored form of a scope: a worker id is kept in lowercase, as principal
// references write it.
func (s HoldScope) key() scopeRef {
	name := s.Name
	if s.Kind == "worker" {
		name = strings.ToLower(name)
	}
	if s.Kind == "farm" {
		return scopeRef{kind: "farm"}
	}
	return scopeRef{kind: s.Kind, tenant: s.Tenant, name: name}
}

// aheadHolds are the scopes and effects of a later version that this build implements: the
// pause of approvals set by a person (HLD-8) and the scope of one schedule (CAP-4).
var aheadHolds = map[string]bool{"pause_action": true, "schedule": true}

// scopeRef is one scope a check point asks about.
type scopeRef struct{ kind, tenant, name string }

// checkHoldRequest checks the scope, effects, reason and until against the closed lists of the
// claimed version.
func (c *Core) checkHoldRequest(now time.Time, req HoldSetRequest) (*time.Time, error) {
	reg, err := c.spec.Registry("spec/model/holds.yaml")
	if err != nil {
		return nil, err
	}
	known := func(id, kind string) error {
		e, ok := reg.Entry(id)
		if !ok || e["kind"] != kind {
			return refuse("input_schema_invalid", "unknown hold %s %s", kind, id)
		}
		// The scope governance (a case pause) is of v0.2, which this build implements (GOV-14).
		if since, _ := e["since"].(string); since != "" && spec.VersionLater(since, spec.Version) && !aheadHolds[id] && !(kind == "scope" && id == "governance") {
			return refuse("input_schema_invalid", "the hold %s %s is of %s; this core claims %s", kind, id, since, spec.Version)
		}
		return nil
	}
	s := req.Scope
	if err := known(s.Kind, "scope"); err != nil {
		return nil, err
	}
	switch {
	case s.Kind == "farm":
		if s.Tenant != "" || s.Name != "" {
			return nil, refuse("input_schema_invalid", "the scope farm has no tenant and no name")
		}
	case s.Kind == "tenant":
		if s.Tenant == "" || s.Name != "" {
			return nil, refuse("input_schema_invalid", "the scope tenant has a tenant and no name")
		}
	case s.Kind == "seat":
		if s.Tenant != "farm" || s.Name == "" {
			return nil, refuse("input_schema_invalid", "the scope seat is of tenant farm and has a name")
		}
	default:
		if s.Tenant == "" || s.Name == "" {
			return nil, refuse("input_schema_invalid", "the scope %s has a tenant and a name", s.Kind)
		}
	}
	if len(req.Effects) == 0 {
		return nil, refuse("input_schema_invalid", "a hold has effects")
	}
	for _, e := range req.Effects {
		if err := known(e, "effect"); err != nil {
			return nil, err
		}
		// pause_action goes on a scope that holds approvals (HLD-8).
		if e == "pause_action" && !slices.Contains(pauseScopes, s.Kind) {
			return nil, refuse("input_schema_invalid", "pause_action is set on a scope that holds approvals, not on %s", s.Kind)
		}
	}
	reasons, err := c.spec.Registry("spec/model/reasons.yaml")
	if err != nil {
		return nil, err
	}
	if !reasons.Has(req.Reason) {
		return nil, refuse("input_schema_invalid", "unknown reason %s", req.Reason)
	}
	if req.Until == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *req.Until)
	if err != nil {
		return nil, refuse("input_schema_invalid", "until")
	}
	if !t.After(now) {
		return nil, refuse("input_schema_invalid", "until is not in the future by the store clock")
	}
	return &t, nil
}

// mayActOn checks that the actor reaches the tenant of a scope: a farm operator reaches every
// tenant, anyone else only their own (TEN-2); a farm or seat scope is the operator's alone
// (PRM-14).
func mayActOn(a HoldActor, s HoldScope) error {
	if farmScope(s.Kind) {
		if a.Rank != "operator" {
			return refuse("operator_only", "a hold of scope %s is an operator operation", s.Kind)
		}
		if a.Tenant != "farm" {
			return refuse("forbidden", "a hold of scope %s needs the operator credential of farm", s.Kind)
		}
		return nil
	}
	if a.Rank == "operator" && a.Tenant == "farm" {
		return nil
	}
	if s.Tenant != a.Tenant {
		return refuse("tenant_mismatch", "the scope is of another tenant")
	}
	return nil
}

// personCap checks a capability of a person of the tenant (PRM-15): hold.set:<scope> and
// hold.release are privileged and come only through farmctl (PRM-13).
func (c *Core) personCap(ctx context.Context, tx pgx.Tx, now time.Time, a HoldActor, cp string) error {
	tv, _, err := c.requireTenant(ctx, a.Tenant)
	if err != nil {
		return err
	}
	p, err := loadPrincipal(ctx, tx, now, a.Tenant, a.Principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("forbidden", "unknown person")
	}
	if err != nil {
		return err
	}
	if p.State != "active" || p.Kind != "human" {
		return refuse("forbidden", "only an active person sets or releases a hold")
	}
	if !hasCap(tv, groupClosure(tv, p.Groups), cp) {
		pr := refuse("no_group_cap", "the person has no %s", cp)
		pr.Needs = groupsWithCap(tv, cp)
		return pr
	}
	return nil
}

// HoldSet records a hold (HLD-1, HLD-2).
func (c *Core) HoldSet(ctx context.Context, a HoldActor, req HoldSetRequest) (*Done, error) {
	if a.Rank != "operator" && a.Rank != "tenant_admin" && a.Rank != "contain_auto" {
		return nil, refuse("forbidden", "only an operator, a person of the tenant or core code sets a hold")
	}
	if err := mayActOn(a, req.Scope); err != nil {
		return nil, err
	}
	if !farmScope(req.Scope.Kind) {
		if _, _, err := c.requireTenant(ctx, req.Scope.Tenant); err != nil {
			return nil, err
		}
	}
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		until, err := c.checkHoldRequest(now, req)
		if err != nil {
			return err
		}
		if a.Rank == "tenant_admin" {
			if err := c.personCap(ctx, tx, now, a, "hold.set:"+req.Scope.Kind); err != nil {
				return err
			}
		}
		id, seq, err := c.insertHold(ctx, tx, now, a, req.Scope, req.Effects, req.Reason, until)
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: id, AuditSeq: seq, State: "active"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// insertHold writes an active hold with its event and audit record.
func (c *Core) insertHold(ctx context.Context, tx pgx.Tx, now time.Time, a HoldActor, s HoldScope, effects []string, reason string, until *time.Time) (string, int64, error) {
	effects = slices.Clone(effects)
	slices.Sort(effects)
	effects = slices.Compact(effects)
	k := s.key()
	id := ids.New(ids.Hold)
	var u any
	if until != nil {
		u = *until
	}
	if _, err := tx.Exec(ctx, `INSERT INTO holds (id, tenant, scope_kind, scope_tenant, scope_name, effects, reason, until, author, author_kind, source, state, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'active', $12)`,
		id, s.recordTenant(), k.kind, k.tenant, k.name, effects, reason, u, a.Principal, a.Rank, nullable(a.Source), now); err != nil {
		return "", 0, err
	}
	h := Hold{ID: id, Tenant: s.recordTenant(), Scope: k.scope(), Effects: effects, Reason: reason, Author: a.Principal, AuthorKind: a.Rank}
	if until != nil {
		v := fmtTime(*until)
		h.Until = &v
	}
	seq, err := c.holdChanged(ctx, tx, now, h, "hold.set.v1", "hold.set", a, "")
	return id, seq, err
}

// scope turns a stored scope back into the record form.
func (k scopeRef) scope() HoldScope { return HoldScope{Kind: k.kind, Tenant: k.tenant, Name: k.name} }

// holdChanged writes the event and the audit record of a hold transition (HLD-1, AUD-1).
func (c *Core) holdChanged(ctx context.Context, tx pgx.Tx, now time.Time, h Hold, event, command string, a HoldActor, releasedBy string) (int64, error) {
	data := map[string]any{"hold": h.ID, "scope": h.Scope, "effects": h.Effects, "reason": h.Reason, "author_kind": h.AuthorKind}
	if h.Until != nil {
		data["until"] = *h.Until
	} else {
		data["until"] = nil
	}
	if strings.Contains(h.Author, ":") {
		data["author"] = h.Author
	}
	if releasedBy != "" {
		data["released_by"] = releasedBy
	}
	if err := c.appendEvent(ctx, tx, now, h.Tenant, Event{Type: event, Subject: h.ID, Data: data}); err != nil {
		return 0, err
	}
	// A no_tool hold drops the cached tool output it covers (PRM-12).
	if event == "hold.set.v1" && holdPurgesCache(h.Effects) {
		if err := purgeToolCache(ctx, tx, h.Scope); err != nil {
			return 0, err
		}
	}
	// The end of the last suspension resumes the tenant (LCY-1).
	if event == "hold.released.v1" || event == "hold.expired.v1" {
		if err := c.tenantResumed(ctx, tx, now, h); err != nil {
			return 0, err
		}
	}
	// A new hold pauses the campaigns it covers in the same transaction (ORC-11).
	if event == "hold.set.v1" {
		if err := c.pauseHeldCampaigns(ctx, tx, now, h.Scope.Tenant); err != nil {
			return 0, err
		}
		// And the script runs it covers (SCR-10).
		if err := c.pauseHeldRuns(ctx, tx, now, h.Scope.Tenant); err != nil {
			return 0, err
		}
	}
	// A hold on the farm manager seat stops the seat in the same transaction (MGR-15).
	if h.Scope.Kind == "seat" && h.Scope.Name == managerSeatID {
		cat, err := c.Catalog(ctx)
		if err != nil {
			return 0, err
		}
		if err := c.syncHumanStop(ctx, tx, now, cat); err != nil {
			return 0, err
		}
	}
	return c.appendAudit(ctx, tx, now, Audit{
		Tenant: h.Tenant, Kind: "command", Actor: Actor{Principal: a.Principal, Via: a.Via},
		Command: command, Target: &Target{Kind: "hold", ID: h.ID}, OK: true,
		After: map[string]any{"scope": h.Scope, "effects": h.Effects, "reason": h.Reason, "author_kind": h.AuthorKind},
	})
}

// rankOrder is the position of a rank from the top (spec/model/holds.yaml, kind: rank); a code
// hold of contain_auto is released by a tenant admin or the operator.
var rankOrder = map[string]int{"operator": 1, "tenant_admin": 2, "contain_auto": 2, "seat_holder": 3}

// HoldRelease ends an active hold by a principal not below its author in rank (HLD-2). The core
// itself never releases a hold: it ends by until or by a person.
func (c *Core) HoldRelease(ctx context.Context, a HoldActor, req HoldReleaseRequest) (*Done, error) {
	if a.Rank != "operator" && a.Rank != "tenant_admin" {
		return nil, refuse("forbidden", "only a person releases a hold")
	}
	var out Done
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		h, err := loadHold(ctx, tx, req.ID, true)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such hold")
		}
		if err != nil {
			return err
		}
		if err := mayActOn(a, h.Scope); err != nil {
			// A hold of another tenant is not shown to exist (TEN-2).
			if p, ok := AsProblem(err); ok && p.Reason == "tenant_mismatch" {
				return refuse("not_found", "no such hold")
			}
			return err
		}
		if a.Rank == "tenant_admin" {
			// A person is below an operator; a seat holder's hold is released by the holder,
			// by the operator, or by a tenant admin only through control.revert (MGR-15).
			if h.AuthorKind == "operator" || h.AuthorKind == "seat_holder" {
				return refuse("forbidden", "a hold of %s is not released by a tenant admin", h.AuthorKind)
			}
			if err := c.personCap(ctx, tx, now, a, "hold.release"); err != nil {
				return err
			}
		}
		// There is no way back from offboarding: its hold stays (LCY-1).
		if h.Scope.Kind == "tenant" && h.Reason == "offboarding" {
			if state, _, err := tenantState(ctx, tx, h.Scope.Tenant); err != nil {
				return err
			} else if state != "" {
				return refuse("forbidden", "the hold of an offboarded tenant is not released")
			}
		}
		if rankOrder[a.Rank] > rankOrder[h.AuthorKind] {
			return refuse("forbidden", "the hold's author is of a higher rank")
		}
		if h.State != "active" || h.until != nil && !h.until.After(now) {
			// A repeat or a release after the end changes nothing (IDM-3).
			state := h.State
			if state == "active" {
				state = "expired"
			}
			out = Done{OK: true, ID: h.ID, State: state}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE holds SET state = 'released', released_by = $2, ended_at = $3 WHERE id = $1`, h.ID, a.Principal, now); err != nil {
			return err
		}
		seq, err := c.holdChanged(ctx, tx, now, h.Hold, "hold.released.v1", "hold.release", a, a.Principal)
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: h.ID, AuditSeq: seq, State: "released"}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

type holdRow struct {
	Hold
	until *time.Time
}

const holdColumns = `id, tenant, scope_kind, scope_tenant, scope_name, effects, reason, until, author, author_kind, coalesce(source, ''), state, created_at, coalesce(released_by, ''), ended_at`

func scanHold(row pgx.Row) (*holdRow, error) {
	var h holdRow
	var k scopeRef
	var created time.Time
	var ended *time.Time
	if err := row.Scan(&h.ID, &h.Tenant, &k.kind, &k.tenant, &k.name, &h.Effects, &h.Reason, &h.until, &h.Author, &h.AuthorKind,
		&h.Source, &h.State, &created, &h.ReleasedBy, &ended); err != nil {
		return nil, err
	}
	h.Scope = k.scope()
	h.CreatedAt = fmtTime(created)
	if h.until != nil {
		v := fmtTime(*h.until)
		h.Until = &v
	}
	if ended != nil {
		h.EndedAt = fmtTime(*ended)
	}
	return &h, nil
}

func loadHold(ctx context.Context, tx pgx.Tx, id string, lock bool) (*holdRow, error) {
	q := `SELECT ` + holdColumns + ` FROM holds WHERE id = $1`
	if lock {
		q += ` FOR UPDATE`
	}
	return scanHold(tx.QueryRow(ctx, q, id))
}

// HoldList returns the active holds of a tenant, oldest first (HLD-1). The operator of farm
// names the tenant, farm included; anyone else gets their own.
func (c *Core) HoldList(ctx context.Context, a HoldActor, req TenantRequest) (*HoldList, error) {
	tenant := a.Tenant
	if req.Tenant != "" && req.Tenant != a.Tenant {
		if a.Rank != "operator" || a.Tenant != "farm" {
			return nil, refuse("forbidden", "the credential is for another tenant")
		}
		tenant = req.Tenant
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	out := HoldList{Items: []Hold{}}
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT `+holdColumns+` FROM holds WHERE tenant = $1 AND state = 'active' AND (until IS NULL OR until > $2)
			ORDER BY created_at, id LIMIT $3`, tenant, now, limit)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			h, err := scanHold(rows)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, h.Hold)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// heldBy returns the id of the oldest hold in force with one of the effects on one of the
// scopes, or "". A hold past its until no longer applies, whether or not the hold loop has
// written its end yet (HLD-1).
func heldBy(ctx context.Context, q store.Querier, now time.Time, effects []string, scopes ...scopeRef) (string, error) {
	return heldByExcept(ctx, q, now, effects, nil, scopes...)
}

// heldByExcept is heldBy that ignores the tenant holds of the given reasons: an export passes
// a suspension or an offboarding, never a hold of another reason or scope (LCY-1).
func heldByExcept(ctx context.Context, q store.Querier, now time.Time, effects, tenantReasons []string, scopes ...scopeRef) (string, error) {
	if tenantReasons == nil {
		tenantReasons = []string{}
	}
	kinds := make([]string, len(scopes))
	tenants := make([]string, len(scopes))
	names := make([]string, len(scopes))
	for i, s := range scopes {
		kinds[i], tenants[i], names[i] = s.kind, s.tenant, s.name
	}
	var id string
	err := q.QueryRow(ctx, `SELECT h.id FROM holds h JOIN unnest($1::text[], $2::text[], $3::text[]) AS s(k, t, n)
		ON h.scope_kind = s.k AND h.scope_tenant = s.t AND h.scope_name = s.n
		WHERE h.state = 'active' AND (h.until IS NULL OR h.until > $4) AND h.effects && $5::text[]
		AND NOT (h.scope_kind = 'tenant' AND h.reason = ANY($6::text[]))
		ORDER BY h.created_at, h.id LIMIT 1`, kinds, tenants, names, now, effects, tenantReasons).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// tenantScopes are the scopes over all work of a tenant.
func tenantScopes(tenant string) []scopeRef {
	return []scopeRef{{kind: "farm"}, {kind: "tenant", tenant: tenant}}
}

// workerScopes are the scopes over the work of one worker: its tenant, role and itself.
func workerScopes(w *Worker) []scopeRef {
	return append(tenantScopes(w.Tenant),
		scopeRef{kind: "role", tenant: w.Tenant, name: w.Role},
		scopeRef{kind: "worker", tenant: w.Tenant, name: strings.ToLower(w.ID)})
}

// holdLoop is the hold loop: hold deadlines, then the auto-stops (HLD-1, HLD-7).
func (c *Core) holdLoop(ctx context.Context) error {
	if err := c.expireHolds(ctx); err != nil {
		return err
	}
	return c.runawaySpend(ctx)
}

// expireHolds ends each hold whose until has come as expired, with its event and
// audit record (HLD-1). It never ends a hold early.
func (c *Core) expireHolds(ctx context.Context) error {
	for {
		done := true
		err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			h, err := scanHold(tx.QueryRow(ctx, `SELECT `+holdColumns+` FROM holds WHERE state = 'active' AND until <= $1
				ORDER BY until, id LIMIT 1 FOR UPDATE SKIP LOCKED`, now))
			if errors.Is(err, pgx.ErrNoRows) {
				return nil
			}
			if err != nil {
				return err
			}
			done = false
			if _, err := tx.Exec(ctx, `UPDATE holds SET state = 'expired', ended_at = until WHERE id = $1`, h.ID); err != nil {
				return err
			}
			h.State = "expired"
			_, err = c.holdChanged(ctx, tx, now, h.Hold, "hold.expired.v1", "hold.expire", HoldActor{Principal: "system", Via: "system"}, "")
			return err
		})
		if err != nil || done {
			return err
		}
	}
}
