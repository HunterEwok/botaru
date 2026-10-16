// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// The P2 operations of attributions and of an incident's cause (GOV-13, GOV-36, GOV-38, GOV-41,
// MGR-8). The skeleton has no schema for attribution.list and attribution.create, so this build
// sets their shape: AttributionRequest and AttributionView. Only a person acts: the operator's
// principal must be a human of the tenant with governance.attribute (attribution.create) or
// incident.manage (incident.cause); a bot has no path to either (GOV-13).

// AttributionRequest is the body of P2 attribution.list and attribution.create.
type AttributionRequest struct {
	Tenant string `json:"tenant,omitempty"`
	// Kind is change (the default), rejected_proposal or script_run.
	Kind      string `json:"kind,omitempty"`
	Incident  string `json:"incident,omitempty"`
	Proposal  string `json:"proposal,omitempty"`
	ScriptRun string `json:"script_run,omitempty"`
	// State filters attribution.list.
	State string `json:"state,omitempty"`
}

// AttributionView is one attribution as people see it: metadata and codes, no model text.
type AttributionView struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	Incident       string `json:"incident"`
	Proposal       string `json:"proposal,omitempty"`
	ScriptRun      string `json:"script_run,omitempty"`
	Rule           string `json:"rule"`
	State          string `json:"state"`
	CreatedBy      string `json:"created_by"`
	CreatedAt      string `json:"created_at"`
	Answer         string `json:"answer,omitempty"`
	AnsweredBy     string `json:"answered_by,omitempty"`
	SelfAssessment bool   `json:"self_assessment,omitempty"`
	Answerable     []any  `json:"answerable,omitempty"`
	ManagerAgainst bool   `json:"manager_against,omitempty"`
	// Card says whether a card of the candidate went to people; Created says whether
	// attribution.create wrote it now.
	Card    bool `json:"card"`
	Created bool `json:"created,omitempty"`
}

// attributionKinds are the kinds of attribution.v2.
var attributionKinds = []string{"change", "rejected_proposal", "script_run"}

// AttributionList is P2 attribution.list: the attributions of the tenant, newest first, at most
// 200, optionally of one kind, state or incident.
func (c *Core) AttributionList(ctx context.Context, op Operator, req AttributionRequest) (map[string]any, error) {
	tname, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	if req.Kind != "" && !slices.Contains(attributionKinds, req.Kind) {
		return nil, refuse("input_schema_invalid", "kind is change, rejected_proposal or script_run")
	}
	items := []AttributionView{}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT id FROM attributions WHERE tenant = $1 AND ($2 = '' OR kind = $2) AND ($3 = '' OR state = $3)
			AND ($4 = '' OR lower(incident) = lower($4)) ORDER BY created_at DESC, id DESC LIMIT 200`, tname, req.Kind, req.State, req.Incident)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range list {
			v, err := attributionView(ctx, tx, id)
			if err != nil {
				return err
			}
			if v != nil {
				items = append(items, *v)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	req.Tenant = tname
	return map[string]any{"tenant": req.Tenant, "items": items}, nil
}

func attributionView(ctx context.Context, tx pgx.Tx, id string) (*AttributionView, error) {
	a, err := loadAttribution(ctx, tx, id, false)
	if err != nil || a == nil {
		return nil, err
	}
	v := &AttributionView{ID: a.ID, Kind: a.Kind, Incident: a.Incident, Proposal: a.Proposal, ScriptRun: a.ScriptRun, Rule: asString(a.Rec["rule"]),
		State: a.State, CreatedBy: asString(a.Rec["created_by"]), CreatedAt: asString(a.Rec["created_at"]), SelfAssessment: a.Rec["self_assessment"] == true,
		Answerable: asList(a.Rec["answerable"]), ManagerAgainst: a.Rec["manager_against"] == true}
	if ans := mmapAny(a.Rec["answer"]); ans != nil {
		v.Answer, v.AnsweredBy = asString(ans["decision"]), asString(ans["by"])
	}
	err = tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE subject_kind = 'attribution' AND subject_id = $1 AND kind = ANY($2))`,
		a.cardSubject(), attributionCards).Scan(&v.Card)
	return v, err
}

// personWith checks that the operator's principal is an active person of the tenant whose groups
// hold capability.
func (c *Core) personWith(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, principal, capability string) error {
	if !strings.HasPrefix(principal, "human:") {
		return refuse("forbidden", "only a person does this")
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return refuse("forbidden", "the person is not a subject of the tenant")
	}
	if err != nil {
		return err
	}
	if p.Kind != "human" || p.State != "active" || !hasCap(tv, groupClosure(tv, p.Groups), capability) {
		return refuse("forbidden", "the person has no %s", capability)
	}
	return nil
}

// AttributionCreate is P2 attribution.create (GOV-13): a person with governance.attribute links
// an incident of the tenant to an executed change while its attribution window is open; the
// candidate has the rule human and goes on attribution_card like one code found. Links to
// rejected proposals and to script runs are found only by code (GOV-38, TRN-7): for these kinds,
// and for a change that already has a candidate for the incident, the operation presents the
// existing candidate instead: its card goes to the people who decide it when none went yet. In
// shadow this is how a person confirms a link to a rejected proposal, which then counts only in
// the statistics (GOV-36). A dismissed incident takes no new candidate.
func (c *Core) AttributionCreate(ctx context.Context, op Operator, req AttributionRequest) (*AttributionView, error) {
	tenant, err := viewTenant(op, req.Tenant)
	if err != nil {
		return nil, err
	}
	kind := req.Kind
	if kind == "" {
		kind = "change"
	}
	if !slices.Contains(attributionKinds, kind) || req.Incident == "" || (kind == "script_run") != (req.ScriptRun != "") || (kind != "script_run") != (req.Proposal != "") {
		return nil, refuse("input_schema_invalid", "attribution.create needs the incident and the proposal, or the script run for kind script_run")
	}
	if kind == "rejected_proposal" && !claimsV03() {
		return nil, refuse("input_schema_invalid", "links to rejected proposals arrive with v0.3")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil, refuse("not_found", "no such tenant")
	}
	var out *AttributionView
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300107, hashtext($1))`, tenant); err != nil {
			return err
		}
		if err := c.personWith(ctx, tx, now, tv, op.Principal, "governance.attribute"); err != nil {
			return err
		}
		inc, err := loadIncident(ctx, tx, tenant, req.Incident, false)
		if err != nil {
			return err
		}
		if inc == nil {
			return refuse("not_found", "no such incident in the tenant")
		}
		if inc.State == "dismissed" {
			return refuse("conflict", "the incident is declared false; it takes no candidates")
		}
		var id string
		err = tx.QueryRow(ctx, `SELECT id FROM attributions WHERE incident = $1 AND kind = $2 AND state <> 'canceled'
			AND (lower(proposal) = lower($3) OR lower(script_run) = lower($4))`, inc.ID, kind, req.Proposal, req.ScriptRun).Scan(&id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		created := false
		switch {
		case id != "":
		case kind != "change":
			return refuse("not_found", "only code finds a link of kind %s, and it found none for this incident", kind)
		default:
			p, err := readProposal(ctx, tx, req.Proposal)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && p.Tenant != tenant {
				return refuse("not_found", "no such proposal in the tenant")
			}
			if err != nil {
				return err
			}
			realized := parseTime(asString(mmapAny(p.Rec["realization"])["realized_at"]))
			if realized.IsZero() || inc.OpenedAt.Before(realized) {
				return refuse("conflict", "the change was not executed before the incident opened")
			}
			open, err := attributionWindowOpen(ctx, tx, now, cat, tenant, p.ID, realized)
			if err != nil {
				return err
			}
			if !open {
				return refuse("expired", "the attribution window of the change is closed")
			}
			if err := c.createAttribution(ctx, tx, now, tv, inc.ID, p.ID, "change", "human", op.Principal); err != nil {
				return err
			}
			if err := tx.QueryRow(ctx, `SELECT id FROM attributions WHERE incident = $1 AND proposal = $2`, inc.ID, p.ID).Scan(&id); err != nil {
				return err
			}
			created = true
		}
		a, err := loadAttribution(ctx, tx, id, true)
		if err != nil {
			return err
		}
		if !created {
			if err := c.presentAttribution(ctx, tx, now, tv, a); err != nil {
				return err
			}
		}
		if out, err = attributionView(ctx, tx, id); err != nil {
			return err
		}
		out.Created = created
		return nil
	})
	return out, err
}

// presentAttribution sends the card of an open candidate to the people who decide it when none
// went yet: for a link to a rejected proposal also in shadow, where code sends none by itself
// (GOV-36, GOV-40).
func (c *Core) presentAttribution(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *attributionRow) error {
	if !slices.Contains([]string{"candidate", "unknown", "rejected"}, a.State) {
		return nil
	}
	subject := a.cardSubject()
	card := cardOfAttributionKind(a.Kind)
	var sent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM deliveries WHERE subject_kind = 'attribution' AND subject_id = $1 AND kind = $2)`,
		subject, card).Scan(&sent); err != nil || sent {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE attributions SET presented_at = $2 WHERE id = $1`, subject, now); err != nil {
		return err
	}
	if err := c.govAudit(ctx, tx, now, a.Tenant, "attribution.present", "attribution", subject, "", map[string]any{"kind": a.Kind}); err != nil {
		return err
	}
	if a.Kind == "script_run" {
		return c.planScriptLinkCards(ctx, tx, now, tv, a.ID, a.ScriptRun, nil)
	}
	_, err := c.planGovCards(ctx, tx, now, tv, "attribution", subject, card, "governance.attribute", proposalMark(ctx, tx, a.Proposal), nil)
	return err
}

// IncidentCauseRequest is p2-incident-cause-request.
type IncidentCauseRequest struct {
	Op       string `json:"op,omitempty"`
	Incident string `json:"incident"`
	Cause    string `json:"cause"`
}

// incidentCauses are the codes of an incident's cause (spec/model/incident.schema.json).
var incidentCauses = []string{"external_load", "attack", "unknown"}

// IncidentCause is P2 incident.cause, from v0.3 (MGR-8, GOV-38, GOV-41): a person with
// incident.manage records the cause of an incident of the tenant; a later call replaces it. The
// code, the person and the time go into the record and the audit, which only people see. With an
// external cause the incident gets no new link to a rejected proposal, its open candidates of
// that kind are canceled in the same transaction, and nobody answers for it: the departures that
// wait for its links end (GOV-41, GOV-42). A departure that already happened stays.
func (c *Core) IncidentCause(ctx context.Context, op Operator, req IncidentCauseRequest) (*Done, error) {
	if !claimsV03() {
		return nil, refuse("not_found", "incident.cause arrives with v0.3")
	}
	if !slices.Contains(incidentCauses, req.Cause) || req.Incident == "" {
		return nil, refuse("input_schema_invalid", "the cause is external_load, attack or unknown")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[op.Tenant]
	if tv == nil {
		return nil, refuse("not_found", "no such tenant")
	}
	var out Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		if err := c.personWith(ctx, tx, now, tv, op.Principal, "incident.manage"); err != nil {
			return err
		}
		r, err := loadIncident(ctx, tx, op.Tenant, req.Incident, true)
		if err != nil {
			return err
		}
		if r == nil {
			return refuse("not_found", "no such incident in the tenant")
		}
		before := r.Record["cause"]
		r.Record["cause"], r.Record["cause_by"], r.Record["cause_at"] = req.Cause, op.Principal, fmtTime(now)
		if err := r.save(ctx, tx, now); err != nil {
			return err
		}
		audit, err := c.appendAudit(ctx, tx, now, Audit{Tenant: op.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "incident.cause", Target: &Target{Kind: "incident", ID: r.ID}, OK: true,
			Before: map[string]any{"cause": before}, After: map[string]any{"cause": req.Cause}})
		if err != nil {
			return err
		}
		out = Done{OK: true, ID: r.ID, State: r.State, AuditSeq: audit}
		if !slices.Contains(externalCauses, req.Cause) {
			return nil
		}
		rows, err := tx.Query(ctx, `SELECT id FROM attributions WHERE incident = $1 AND kind = 'rejected_proposal' AND state IN ('candidate', 'unknown', 'rejected')
			ORDER BY id FOR UPDATE`, r.ID)
		if err != nil {
			return err
		}
		list, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, id := range list {
			a, err := loadAttribution(ctx, tx, id, true)
			if err != nil || a == nil {
				return err
			}
			if err := c.moveAttribution(ctx, tx, now, a, "canceled", op.Principal, "attribution.canceled"); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `UPDATE link_departures d SET state = 'canceled', reason = 'external_cause', updated_at = $2
			FROM attributions a WHERE a.id = d.attribution AND a.incident = $1 AND d.state = 'waiting'`, r.ID, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}
