// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

const (
	// The deadlines of the proposal flow when the tenant sets none (PRP-6, COM-6).
	defaultParkTTL       = 30 * 24 * time.Hour
	defaultRecommendTTL  = 28 * 24 * time.Hour
	defaultReviewWaitMax = 7 * 24 * time.Hour
	// roleReviewShare is the most of the review line one author role may take a month (COM-6).
	roleReviewShare = 0.3
	reviewSource    = "proposal"
)

// tenantSpecOf is the spec of the first manifest of a kind in a tenant, by name.
func tenantSpecOf(cat *Catalog, kind, tenant string) map[string]any {
	var keys []manifest.Key
	for k := range cat.RawGlobals {
		if k.Kind == kind && k.Tenant == tenant {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	spec, _ := cat.RawGlobals[keys[0]]["spec"].(map[string]any)
	return spec
}

// lensDef is a skeleton lens of spec/model/lenses.yaml.
type lensDef struct {
	ID, ReviewType string
	Veto, KindNone bool
}

func (c *Core) lensDefs() map[string]lensDef {
	out := map[string]lensDef{}
	reg, err := c.spec.Registry("spec/model/lenses.yaml")
	if err != nil {
		return out
	}
	for _, e := range reg.Entries {
		if e["since"] != "v0.1" || e["subject"] != "proposal" {
			continue
		}
		out[asString(e["id"])] = lensDef{ID: asString(e["id"]), ReviewType: asString(e["review_type"]), Veto: e["veto"] == true, KindNone: e["kind_none"] == true}
	}
	return out
}

// lensExecutor is who reviews a lens: the fallback role of the charter seats of the lens (the
// seat holders come with bot identity), or nothing (COM-7).
type lensExecutor struct {
	Lens, Type, Role, Source string
}

// commissionLenses are the lenses a proposal is reviewed by, with their executors: the lenses
// of the charter seats and the veto lenses always; for kind none only value and security
// (COM-7, COM-8, COM-14). A lens whose executor would break independence has none (COM-2).
func (c *Core) commissionLenses(cat *Catalog, tenant string, kindNone bool, authorRole string) ([]string, map[string]lensExecutor) {
	defs := c.lensDefs()
	gov := tenantSpecOf(cat, "Governance", tenant)
	com, _ := gov["commission"].(map[string]any)
	seats, _ := com["seats"].([]any)
	lenses := []string{}
	exec := map[string]lensExecutor{}
	add := func(l string) {
		if !slices.Contains(lenses, l) {
			lenses = append(lenses, l)
		}
	}
	for _, l := range slices.Sorted(mapKeys(defs)) {
		if defs[l].Veto {
			add(l)
		}
	}
	for _, s := range seats {
		sm, _ := s.(map[string]any)
		l := asString(sm["lens"])
		d, ok := defs[l]
		if !ok {
			continue
		}
		add(l)
		if _, has := exec[l]; has {
			continue
		}
		role := asString(sm["fallback_role"])
		if c.independentReviewer(cat, role, d.ReviewType, authorRole) {
			exec[l] = lensExecutor{Lens: l, Type: d.ReviewType, Role: role, Source: "fallback_role"}
		}
	}
	if kindNone {
		lenses = slices.DeleteFunc(lenses, func(l string) bool { return !defs[l].KindNone })
	}
	sort.Strings(lenses)
	return lenses, exec
}

// independentReviewer: the reviewer role is not the author role, claims the review type, and
// its model profile is not the author's analysis profile (COM-2).
func (c *Core) independentReviewer(cat *Catalog, role, typ, authorRole string) bool {
	r, ok := cat.Roles[role]
	if !ok || role == authorRole || !slices.Contains(r.Claims, typ) {
		return false
	}
	if a, ok := cat.Roles[authorRole]; ok && a.Models["analysis"] != "" && r.Models["base"] == a.Models["analysis"] {
		return false
	}
	return true
}

// proposalLoop moves proposals by the transition table (PRP-6): code checks, reviews and the
// verdict, and the deadlines.
func (c *Core) proposalLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var ids []string
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE state IN ('screened', 'in_review', 'recommended', 'parked') ORDER BY created_at, id`)
		if err != nil {
			return err
		}
		ids, err = pgx.CollectRows(rows, pgx.RowTo[string])
		return err
	}); err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.stepProposal(ctx, tx, now, cat, id) }); err != nil {
			errs = append(errs, fmt.Errorf("proposal %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// proposalRow is a proposal record under its lock.
type proposalRow struct {
	ID, Tenant, Role, State string
	Rec                     map[string]any
	StateAt                 time.Time
}

func (p *proposalRow) body() proposalBody {
	var b proposalBody
	raw, _ := json.Marshal(p.Rec["body"])
	_ = json.Unmarshal(raw, &b)
	return b
}

func (p *proposalRow) kind() string { return p.body().Change.Kind }

func (p *proposalRow) reason() string { return asString(p.Rec["state_reason"]) }

// moveProposal writes a transition of the table with its reason and an event.
func (c *Core) moveProposal(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow, state, reason string) error {
	from := p.State
	p.Rec["state"] = state
	if reason != "" {
		p.Rec["state_reason"] = reason
	} else {
		p.Rec["state_reason"] = nil
	}
	p.State, p.StateAt = state, now
	raw, err := json.Marshal(p.Rec)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE proposals SET state = $2, record = $3, updated_at = $4, state_at = $4 WHERE id = $1`, p.ID, state, raw, now); err != nil {
		return err
	}
	data := map[string]any{"proposal": p.ID, "from": from, "to": state}
	if reason != "" {
		data["reason"] = reason
	}
	return c.appendEvent(ctx, tx, now, p.Tenant, Event{Type: "proposal.state", Subject: p.ID, Data: data})
}

func (c *Core) saveProposal(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	raw, err := json.Marshal(p.Rec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE proposals SET record = $2, updated_at = $3 WHERE id = $1`, p.ID, raw, now)
	return err
}

func (c *Core) stepProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, id string) error {
	p := &proposalRow{ID: id}
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT tenant, role, state, record, state_at FROM proposals WHERE id = $1 FOR UPDATE SKIP LOCKED`, id).
		Scan(&p.Tenant, &p.Role, &p.State, &raw, &p.StateAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &p.Rec); err != nil {
		return err
	}
	tv := cat.Tenants[p.Tenant]
	if tv == nil {
		return nil
	}
	ins := tv.Spec.Insights
	switch p.State {
	case "screened":
		return c.screenProposal(ctx, tx, now, cat, tv, p)
	case "in_review":
		return c.reviewProposal(ctx, tx, now, cat, tv, p)
	case "recommended":
		if now.Sub(p.StateAt) > durationOr(ins.RecommendTTL, defaultRecommendTTL) {
			return c.moveProposal(ctx, tx, now, p, "expired", "")
		}
	case "parked":
		if now.Sub(p.StateAt) > durationOr(ins.ParkTTL, defaultParkTTL) {
			return c.moveProposal(ctx, tx, now, p, "expired", "")
		}
		// Money for reviews came back: the proposal is screened again (COM-6).
		if p.reason() == "review_budget" {
			lenses, exec := c.commissionLenses(cat, p.Tenant, p.kind() == "none", p.Role)
			if c.reviewsFit(ctx, tx, now, cat, tv, p, lenses, exec) == nil {
				return c.moveProposal(ctx, tx, now, p, "screened", "")
			}
		}
	}
	return nil
}

// screenProposal runs the code checks in their order before any review: the empty change,
// feasibility, a veto lens executor and the review money (COM-5, COM-6, COM-7). Schema,
// evidence and duplicates were checked at creation.
func (c *Core) screenProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow) error {
	b := p.body()
	if noOp(cat, tv, b.Change) {
		return c.moveProposal(ctx, tx, now, p, "dismissed", "no_op")
	}
	if ok, err := hasDecider(ctx, tx, tv, b.Category); err != nil {
		return err
	} else if !ok {
		return c.moveProposal(ctx, tx, now, p, "dismissed", "other")
	}
	lenses, exec := c.commissionLenses(cat, p.Tenant, b.Change.Kind == "none", p.Role)
	defs := c.lensDefs()
	for _, l := range lenses {
		if _, ok := exec[l]; !ok && defs[l].Veto {
			return c.moveProposal(ctx, tx, now, p, "parked", "veto_lens_missing")
		}
	}
	// A proposal that cannot start its reviews waits at most review_wait_max, then parks.
	wait := func() error {
		if now.Sub(p.StateAt) > durationOr(tv.Spec.Insights.ReviewWaitMax, defaultReviewWaitMax) {
			return c.moveProposal(ctx, tx, now, p, "parked", "review_budget")
		}
		return nil
	}
	if pr := c.reviewsFit(ctx, tx, now, cat, tv, p, lenses, exec); pr != nil {
		return wait()
	}
	// The reviews go out together under a savepoint: either every lens task with its reserve,
	// or none (COM-6).
	sp, err := tx.Begin(ctx)
	if err != nil {
		return err
	}
	reviews, reserved, err := c.enqueueReviews(ctx, sp, now, cat, tv, p, lenses, exec)
	if err != nil {
		_ = sp.Rollback(ctx)
		var pr *Problem
		if errors.As(err, &pr) {
			return wait()
		}
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return err
	}
	p.Rec["reviews"] = reviews
	p.Rec["review_reserved_eur"] = reserved
	p.Rec["review_started_at"] = fmtTime(now)
	return c.moveProposal(ctx, tx, now, p, "in_review", "")
}

// reviewsFit checks the review line for the sum of the review estimates and the author role's
// share of the line this month (COM-6).
func (c *Core) reviewsFit(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow, lenses []string, exec map[string]lensExecutor) *Problem {
	sum := 0.0
	for _, l := range lenses {
		if e, ok := exec[l]; ok {
			sum += cat.TaskTypes[e.Type].TreeBudgetEUR
		}
	}
	if pr := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", extra: money.FromEUR(sum), line: "review"}); pr != nil {
		return pr
	}
	line := tv.Spec.Budgets.Lines["review"]
	if line.MonthEUR == nil {
		return refuse("budget_exhausted", "the review line is 0")
	}
	var used float64
	start := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum((record->>'review_reserved_eur')::float8), 0) FROM proposals
		WHERE tenant = $1 AND role = $2 AND id <> $3 AND (record->>'review_started_at')::timestamptz >= $4`, p.Tenant, p.Role, p.ID, start).Scan(&used); err != nil {
		return refuse("not_ready", "store")
	}
	if used+sum > roleReviewShare**line.MonthEUR+1e-9 {
		return refuse("budget_exhausted", "the author role's share of the review line is used up")
	}
	return nil
}

// enqueueReviews enqueues one review task per lens with an executor, by the key
// system:review:<proposal>:<lens> (COM-2, IDM-2), with the input block the core builds
// (COM-3); the task goes only to the lens executor role and never to the author's worker.
func (c *Core) enqueueReviews(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow, lenses []string, exec map[string]lensExecutor) ([]any, float64, error) {
	input := reviewInput(p)
	reviews := []any{}
	reserved := 0.0
	origin, _ := p.Rec["origin"].(map[string]any)
	for _, l := range lenses {
		e, ok := exec[l]
		if !ok {
			continue
		}
		body := map[string]any{"type": e.Type, "goal": "Review the proposal through the " + l + " lens.", "params": map[string]any{"input": input}}
		res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: p.Tenant, Type: e.Type, Body: body,
			IdempotencyKey: "system:review:" + p.ID + ":" + l, Initiator: "system", EnqueuedBy: "system:proposal", Source: reviewSource, SourceID: p.ID})
		if err != nil {
			return nil, 0, err
		}
		if res.Rejected != nil {
			return nil, 0, res.Rejected
		}
		extra := map[string]any{"lens": l, "proposal": p.ID, "executor_role": e.Role, "not_worker": asString(origin["worker"])}
		if mark := asString(p.Rec["data_mark"]); mark != "" {
			if mo, ok := c.markOrder(mark); ok {
				if to, ok := c.markOrder(cat.TaskTypes[e.Type].DataClass); !ok || mo > to {
					extra["data_mark"] = mark
				}
			}
		}
		raw, err := json.Marshal(extra)
		if err != nil {
			return nil, 0, err
		}
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb WHERE id = $1`, res.TaskID, raw); err != nil {
			return nil, 0, err
		}
		reserved += cat.TaskTypes[e.Type].TreeBudgetEUR
		reviews = append(reviews, map[string]any{"lens": l, "task": res.TaskID, "executor": e.Source, "state": "pending"})
	}
	return reviews, reserved, nil
}

// reviewInput is the data block of a review: closed fields of the body, the values the core
// reread, the change in code words, the author level and the marks; never the note, the
// author's identity or other lenses' scores (COM-2, COM-3).
func reviewInput(p *proposalRow) map[string]any {
	body, _ := p.Rec["body"].(map[string]any)
	in := map[string]any{}
	for _, k := range []string{"category", "problem", "change", "benefit", "cost", "risk"} {
		if v, ok := body[k]; ok {
			in[k] = v
		}
	}
	origin, _ := p.Rec["origin"].(map[string]any)
	in["evidence"] = p.Rec["evidence_seen"]
	in["author_level"] = origin["level"]
	in["self_evidence"] = p.Rec["self_evidence"] == true
	us, _ := p.Rec["untrusted_seen"].(map[string]any)
	in["untrusted_seen"] = us["value"] == true
	in["guarded"] = p.Rec["guarded"] == true
	in["conflicts_with"] = p.Rec["conflicts_with"]
	return in
}

// reviewProposal reads the fate of each lens task: done with its result, failed, or late after
// review_deadline; when no review is pending the code verdict moves the proposal (COM-4).
func (c *Core) reviewProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, p *proposalRow) error {
	policy := policyFrom(tenantSpecOf(cat, "Commission", p.Tenant))
	started, _ := time.Parse(time.RFC3339, asString(p.Rec["review_started_at"]))
	deadline := started.Add(durationOr(policy.ReviewDeadline, 24*time.Hour))
	reviews, _ := p.Rec["reviews"].([]any)
	pending := false
	changed := false
	for _, r := range reviews {
		rm := r.(map[string]any)
		if rm["state"] != "pending" {
			continue
		}
		var state string
		var result []byte
		if err := tx.QueryRow(ctx, `SELECT state, result FROM tasks WHERE id = $1`, rm["task"]).Scan(&state, &result); err != nil {
			return err
		}
		switch {
		case state == "completed" && now.After(deadline):
			rm["state"], changed = "late", true
		case state == "completed":
			var res map[string]any
			if err := json.Unmarshal(result, &res); err != nil || c.spec.Validate("urn:botaru:schema:review-result:1", res) != nil || res["lens"] != rm["lens"] || res["shadow"] == true {
				rm["state"] = "schema_invalid"
			} else {
				rm["state"], rm["result"] = "done", res
			}
			changed = true
		case state == "failed" || state == "canceled":
			rm["state"], changed = "failed", true
		case now.After(deadline):
			rm["state"], changed = "late", true
		default:
			pending = true
		}
	}
	if pending {
		if changed {
			return c.saveProposal(ctx, tx, now, p)
		}
		return nil
	}
	b := p.body()
	in := verdictInput{KindNone: b.Change.Kind == "none", Guarded: p.Rec["guarded"] == true, VetoLens: map[string]bool{}}
	risk, _ := mmapAny(p.Rec["body"])["risk"].(map[string]any)
	in.Risk = asString(risk["level"])
	if in.Risk == "" {
		in.Risk = "low"
	}
	us, _ := p.Rec["untrusted_seen"].(map[string]any)
	in.Untrusted = us["value"] == true
	defs := c.lensDefs()
	lenses, _ := c.commissionLenses(cat, p.Tenant, in.KindNone, p.Role)
	in.Lenses = lenses
	for _, l := range lenses {
		in.VetoLens[l] = defs[l].Veto
	}
	for _, r := range reviews {
		rm := r.(map[string]any)
		lr := lensReview{Lens: asString(rm["lens"]), State: asString(rm["state"])}
		if res, ok := rm["result"].(map[string]any); ok {
			n, _ := toFloat(res["score"])
			lr.Score, lr.Veto, lr.Shadow = int(n), res["veto"] == true, res["shadow"] == true
		}
		in.Reviews = append(in.Reviews, lr)
	}
	v := verdict(policy, in)
	v["computed_at"] = fmtTime(now)
	p.Rec["verdict"] = v
	state, reason, _ := strings.Cut(asString(v["outcome"]), "(")
	return c.moveProposal(ctx, tx, now, p, state, strings.TrimSuffix(reason, ")"))
}

func mmapAny(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

// hasDecider says whether the tenant has an active person who may decide on the category
// (COM-5 feasibility, DIG-3).
func hasDecider(ctx context.Context, tx pgx.Tx, tv *TenantView, category string) (bool, error) {
	var groups []string
	for _, g := range slices.Sorted(mapKeys(tv.Groups)) {
		if hasCap(tv, groupClosure(tv, []string{g}), "proposal.decide:"+category) {
			groups = append(groups, g)
		}
	}
	if len(groups) == 0 {
		return false, nil
	}
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM principals p JOIN memberships m ON m.tenant = p.tenant AND m.principal = p.id
		WHERE p.tenant = $1 AND p.state = 'active' AND p.kind = 'human' AND m.state = 'active' AND (m.until IS NULL OR m.until > botaru_now()) AND m.grp = ANY($2)`,
		tv.Name, groups).Scan(&n)
	return n > 0, err
}

// noOp says whether every value of a manifest change is already in place (COM-5).
func noOp(cat *Catalog, tv *TenantView, ch proposalChange) bool {
	if ch.Kind != "manifest_change" || ch.ManifestChange == nil {
		return false
	}
	for _, op := range ch.ManifestChange.Ops {
		cur, ok := currentValue(cat, tv, op)
		if !ok || fmt.Sprint(cur) != fmt.Sprint(op.Value) {
			return false
		}
	}
	return true
}

// currentValue is the value of the field a set operation names, as applied now.
func currentValue(cat *Catalog, tv *TenantView, op manifestOp) (any, bool) {
	switch op.Target.Kind {
	case "Schedule":
		raw := cat.RawGlobals[manifest.Key{Kind: "Schedule", Tenant: tv.Name, Name: op.Target.Name}]
		return pathValue(raw, op.Field)
	case "Tenant":
		return pathValue(tv.Raw, op.Field)
	}
	return nil, false
}

// pathValue reads spec.a.b or spec.a[].b (the single element) from a manifest.
func pathValue(m map[string]any, path string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(path, ".") {
		list := strings.HasSuffix(part, "[]")
		part = strings.TrimSuffix(part, "[]")
		mm, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = mm[part]; !ok {
			return nil, false
		}
		if list {
			l, ok := cur.([]any)
			if !ok || len(l) != 1 {
				return nil, false
			}
			cur = l[0]
		}
	}
	return cur, true
}
