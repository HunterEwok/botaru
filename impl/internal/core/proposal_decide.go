// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

const (
	// The deadlines of the decision and the realization when the tenant sets none (PRP-6,
	// PRP-18, decision-points proposal_card).
	defaultDecideTTL = 14 * 24 * time.Hour
	defaultRealizeBy = 7 * 24 * time.Hour
	// maxDefers is how often "Postpone" may be pressed (DIG-3).
	maxDefers = 2
	// trialDays is the window of a change trial and of its values before (PRP-21).
	trialDays = 14
	maxGuards = 5
)

// errNoTrial is a proposal whose trial cannot be measured from the catalog.
var errNoTrial = errors.New("no trial")

// decideProposal is P1 decide on the subject proposal (DIG-3, APR-4): "Accept", "Reject" with a
// reason, "Postpone" at most twice, by a person with proposal.decide:<category> from the chat of
// the card. "Accept" creates exactly one object of the ordinary path on behalf of that person
// and the trial record in the same transaction (PRP-4, PRP-18, PRP-21).
func (c *Core) decideProposal(ctx context.Context, w *Worker, ob OnBehalf, req DecideRequest) (*DecideResult, error) {
	if req.Decision != "accept" && req.Decision != "reject" && req.Decision != "defer" && req.Decision != "count" && !slices.Contains(sampleDecisions, req.Decision) {
		return nil, refuse("input_schema_invalid", "a proposal is accepted, rejected or postponed")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out *DecideResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var tenant string
		if err := tx.QueryRow(ctx, `SELECT tenant FROM proposals WHERE id = $1`, req.ID).Scan(&tenant); errors.Is(err, pgx.ErrNoRows) || err == nil && tenant != w.Tenant {
			return refuse("not_found", "no such proposal")
		} else if err != nil {
			return err
		}
		p, err := loadProposal(ctx, tx, req.ID)
		if err != nil {
			return err
		}
		if p.Nonce == "" || p.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "proposal", p.ID, []string{proposalCard, proposalSampleCard, selfMeasuredCard})
		if err != nil {
			return err
		}
		out = &DecideResult{SubjectKind: "proposal", ID: p.ID, Decision: req.Decision}
		deny := func(reason, detail string) error {
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: p.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "decide", Target: &Target{Kind: "proposal", ID: p.ID}, OK: false, Reason: reason,
				After: map[string]any{"decision": req.Decision},
			}); err != nil {
				return err
			}
			return &committed{p: refuse(reason, "%s", detail)}
		}
		var views int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM card_views WHERE subject_kind = 'proposal' AND subject_id = $1 AND principal = $2`, p.ID, press.principal).Scan(&views); err != nil {
			return err
		}
		if req.Decision == "count" {
			// "Count it" on self_measured_card (GOV-14).
			out.State, err = c.countSelfMeasured(ctx, tx, now, press, p, deny)
			if err != nil {
				return err
			}
			return c.planProposalCardUpdates(ctx, tx, now, p)
		}
		if _, sampled := mmapAny(p.Rec["sample"])["month"]; sampled {
			// An item of the audit sample has its own branch (DIG-6).
			before := p.State
			state, err := c.decideSample(ctx, tx, now, cat, press, p, req, views, deny)
			if err != nil || state == before {
				out.State = state
				return err
			}
			if _, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: p.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
				Command: "decide", Target: &Target{Kind: "proposal", ID: p.ID}, OK: true,
				After: map[string]any{"decision": req.Decision, "state": state},
			}); err != nil {
				return err
			}
			out.State = state
			return c.planProposalCardUpdates(ctx, tx, now, p)
		}
		if slices.Contains(sampleDecisions, req.Decision) {
			return refuse("input_schema_invalid", "the proposal is not an item of the audit sample")
		}
		if p.State != "presented" {
			// A repeat of the same decision by the same person gets the same answer (IDM-3).
			d := mmapAny(p.Rec["decision"])
			// A postponed proposal keeps the nonce of its card until a later digest presents it.
			if d["decided_by"] == press.principal && d["decision"] == req.Decision && (p.State != "recommended" || req.Decision == "defer") {
				out.State = p.State
				return nil
			}
			if p.State == "expired" {
				return refuse("expired", "the card has expired")
			}
			return refuse("already_decided", "the proposal is decided")
		}
		b := p.body()
		if !hasCap(press.tv, press.groups, "proposal.decide:"+b.Category) {
			return deny("forbidden", "the person has no proposal.decide for this category")
		}
		if p.PresentedAt != nil && now.Sub(*p.PresentedAt) > durationOr(press.tv.Spec.Insights.DecideTTL, defaultDecideTTL) {
			if err := c.moveProposal(ctx, tx, now, p, "expired", ""); err != nil {
				return err
			}
			if err := c.planProposalCardUpdates(ctx, tx, now, p); err != nil {
				return err
			}
			return &committed{p: refuse("expired", "the card has expired")}
		}
		if err := c.requireDetails(ctx, tx, now, p.Tenant, "proposal", p.ID, proposalCard, req.Decision, press.principal); err != nil {
			return err
		}
		prev := mmapAny(p.Rec["decision"])
		defers, _ := toFloat(prev["defer_count"])
		decision := map[string]any{"decision": req.Decision, "reason_code": nil, "decided_by": press.principal, "at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0), "details_opened": views > 0, "defer_count": int(defers)}
		var state, reason string
		switch req.Decision {
		case "reject":
			code := req.ReasonCode
			if code == "" {
				// A channel without the reason card gives no reason; it is "other" (DIG-3).
				code = "other"
			}
			if !c.declineReason(code) {
				return refuse("input_schema_invalid", "the reason is not in decline-reasons.yaml")
			}
			decision["reason_code"] = code
			state = "declined"
		case "defer":
			if int(defers) >= maxDefers {
				return deny("forbidden", "a proposal is postponed at most twice")
			}
			decision["defer_count"] = int(defers) + 1
			state = "recommended"
		case "accept":
			if b.Change.Kind == "none" {
				state = "noted"
				break
			}
			// Only one of conflicting changes runs at a time (PRP-9).
			if holders, err := lockHolders(ctx, tx, p.Tenant, p.ID, lockKeys(b)); err != nil {
				return err
			} else if len(holders) > 0 {
				return deny("forbidden", "another accepted change holds the same field")
			}
			code, err := c.realize(ctx, tx, now, cat, press, p)
			if err != nil {
				return err
			}
			if code != "" {
				return deny(code, "the person cannot start the realization of this proposal")
			}
			state = "realizing"
		}
		p.Rec["decision"] = decision
		if err := c.moveProposal(ctx, tx, now, p, state, reason); err != nil {
			return err
		}
		if err := decisionPoints(ctx, tx, now, p, req.Decision, asString(decision["reason_code"])); err != nil {
			return err
		}
		if state == "realizing" {
			if err := c.parkConflicts(ctx, tx, now, p); err != nil {
				return err
			}
		}
		if state == "recommended" {
			// A postponed proposal goes into a later digest with a new card.
			if _, err := tx.Exec(ctx, `UPDATE proposals SET presented_at = NULL WHERE id = $1`, p.ID); err != nil {
				return err
			}
		}
		ev := map[string]any{
			"subject": map[string]any{"kind": "proposal", "id": p.ID}, "card": proposalCard, "decision": req.Decision,
			"decided_by": press.principal, "channel": w.Target, "chat_kind": ob.Chat.Kind, "via": "decide",
			"delivered_at": fmtTime(press.deliveredAt), "decided_at": fmtTime(now),
			"response_ms": max(now.Sub(press.deliveredAt).Milliseconds(), 0), "details_opened": views > 0,
		}
		if code, ok := decision["reason_code"].(string); ok {
			ev["reason_code"] = code
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: p.Tenant, Kind: "decision", Actor: Actor{Principal: press.principal, Via: "channel"},
			Command: "decide", Target: &Target{Kind: "proposal", ID: p.ID}, OK: true,
			After: map[string]any{"decision": req.Decision, "state": state},
		}); err != nil {
			return err
		}
		if err := c.planProposalCardUpdates(ctx, tx, now, p); err != nil {
			return err
		}
		out.State = state
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (c *Core) declineReason(code string) bool {
	reg, err := c.spec.Registry("spec/model/decline-reasons.yaml")
	if err != nil {
		return false
	}
	e, ok := reg.Entry(code)
	return ok && e["since"] == "v0.1"
}

// realize starts the realization of an accepted proposal (PRP-4, PRP-18, PRP-21): an approval
// on behalf of the person for an action request, with the person's right to request it, or the
// executor and the deadline with the realization card for a manifest or a manual change; and
// the trial record with the values before. A non-empty code refuses the press.
func (c *Core) realize(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, press *cardPress, p *proposalRow) (string, error) {
	tv := press.tv
	b := p.body()
	trial, err := c.trialBaseline(ctx, tx, now, cat, p)
	if errors.Is(err, errNoTrial) {
		// A change whose trial cannot be measured is not started (PRP-21).
		return "forbidden", nil
	}
	if err != nil {
		return "", err
	}
	realization := map[string]any{"realizer": nil, "realize_by": fmtTime(now.Add(durationOr(tv.Spec.Insights.RealizeBy, defaultRealizeBy))),
		"object": nil, "commit": nil, "link": nil, "realized_at": nil, "revert_patch": nil, "batch": nil, "revert_reason": nil}
	switch b.Change.Kind {
	case "action_request":
		ar := b.Change.ActionRequest
		var action string
		var args map[string]any
		switch {
		case ar.Action != "":
			act, ok := cat.Actions[ar.Action]
			if !ok {
				return "forbidden", nil
			}
			action, args = ar.Action, actionDefaults(act, ar.Args)
		case ar.Tool == "service/control":
			svc, _ := ar.Args["service"].(string)
			verb, _ := ar.Args["verb"].(string)
			action, args = serviceAction, map[string]any{"service": svc, "verb": verb}
		default:
			// A monitoring script has no person path in this version: the proposal is only
			// rejected or postponed.
			return "forbidden", nil
		}
		if !hasCap(tv, press.groups, requestCap(action, args)) {
			return "forbidden", nil
		}
		act, _ := c.actionOf(cat, action)
		mark := c.personMark(tv, press.groups)
		schema, err := c.actionArgsSchema(cat, tv.Name, mark, act)
		if err != nil || c.rs.validate(schema, args) != nil {
			// An action whose arguments cannot be checked is not started (APR-4).
			return "forbidden", nil
		}
		argsHash, err := jcs.Hash(args)
		if err != nil {
			return "", err
		}
		risk := act.Risk
		if risk == "" {
			risk = "low"
		}
		// A hold on the action stops "Accept" before any approval exists (HLD-1).
		if h, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action"}, approvalScopes(tv.Name, action, "", "")...); err != nil {
			return "", err
		} else if h != "" {
			return "held", nil
		}
		id, code, err := c.openApproval(ctx, tx, now, tv, approvalRow{
			Tenant: tv.Name, Initiator: press.principal, Action: action, ArgsHash: argsHash, Args: args, Risk: risk,
			RequestedBy: map[string]any{"kind": "proposal", "ref": p.ID}, ExpiresAt: now.Add(actionTTL(act)), Mark: mark,
		})
		if err != nil {
			return "", err
		}
		if code != "pending" {
			return code, nil
		}
		ap, err := loadApproval(ctx, tx, tv.Name, id, true)
		if err != nil {
			return "", err
		}
		if ap.CardsAt == nil {
			if err := c.planCards(ctx, tx, now, tv, ap); err != nil {
				return "", err
			}
		}
		realization["realizer"], realization["object"] = principalObject(press.principal), id
	case "script":
		id, code, err := c.realizeScript(ctx, tx, now, cat, press, p)
		if err != nil || code != "" {
			return code, err
		}
		realization["realizer"], realization["object"] = principalObject(press.principal), id
	case "manifest_change", "human_change":
		realizer, err := c.realizer(ctx, tx, now, tv, press.principal, asString(p.Rec["data_mark"]))
		if err != nil {
			return "", err
		}
		if realizer == "" {
			return "forbidden", nil
		}
		realization["realizer"] = principalObject(realizer)
	default:
		return "forbidden", nil
	}
	p.Rec["realization"], p.Rec["trial"] = realization, trial
	if b.Change.Kind == "manifest_change" || b.Change.Kind == "human_change" {
		// The executor gets the card with the change and the deadline (PRP-18); the card is
		// drawn from the record when it is handed to the channel.
		if err := c.planProposalCard(ctx, tx, now, tv, p, realizationCard, "human:"+asString(realization["realizer"]), nil); err != nil {
			return "", err
		}
	}
	return "", nil
}

// principalObject is the object id of a principal reference human:p_<ULID>.
func principalObject(principal string) string {
	_, id, ok := strings.Cut(principal, ":")
	if !ok {
		return principal
	}
	return id
}

// realizer is the executor of a manifest or a manual change (PRP-18): the person who accepted
// when they are an active member of a group of insights.realizers, otherwise the first such
// member; with no realizers the person who accepted. The executor sees the change in full, so
// only a person whose ceiling covers the proposal's mark qualifies (DIG-2); "" when nobody does.
func (c *Core) realizer(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, accepter, mark string) (string, error) {
	sees := func(person string) (bool, error) {
		info, err := loadPrincipal(ctx, tx, now, tv.Name, person)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return info.State == "active" && c.digestShow(mark, c.personMark(tv, groupClosure(tv, info.Groups))) == "full", nil
	}
	groups := tv.Spec.Insights.Realizers
	if len(groups) == 0 {
		if ok, err := sees(accepter); err != nil || !ok {
			return "", err
		}
		return accepter, nil
	}
	var cands []string
	for _, g := range groups {
		people, err := groupMembers(ctx, tx, now, tv, g)
		if err != nil {
			return "", err
		}
		if slices.Contains(people, accepter) {
			cands = append([]string{accepter}, cands...)
		}
		cands = append(cands, people...)
	}
	for _, p := range cands {
		ok, err := sees(p)
		if err != nil {
			return "", err
		}
		if ok {
			return p, nil
		}
	}
	return "", nil
}

// trialBaseline is the trial record on "Accept" (PRP-21): the metric of benefit, the promised
// change, the subjects, the metric value over the 14 days before and one to five guards from
// the registry tables by the target kind and by the metric, plus the tenant's extra guards,
// each with its value before.
func (c *Core) trialBaseline(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, p *proposalRow) (map[string]any, error) {
	b := p.body()
	if b.Benefit == nil {
		return nil, fmt.Errorf("%w: proposal %s of kind %s has no benefit", errNoTrial, p.ID, b.Change.Kind)
	}
	series := c.tenantSeries(cat, p.Tenant)
	metric := series[b.Benefit.Metric]
	if metric.Outcome == nil {
		return nil, fmt.Errorf("%w: proposal %s: metric %s has no outcome block", errNoTrial, p.ID, b.Benefit.Metric)
	}
	subject := b.Benefit.Subject
	from := now.AddDate(0, 0, -trialDays)
	origin := mmapAny(p.Rec["origin"])
	author := asString(origin["worker"])
	before, err := trialValue(ctx, tx, p.Tenant, metric, subject, from, now, author, p.Role)
	if err != nil {
		return nil, err
	}
	promised := map[string]any{"direction": b.Benefit.Direction}
	if b.Benefit.ExpectedChangePP != nil {
		promised["change_pp"] = *b.Benefit.ExpectedChangePP
	}
	if b.Benefit.ExpectedChangePct != nil {
		promised["change_pct"] = *b.Benefit.ExpectedChangePct
	}
	var guards []any
	subjects := []any{subject}
	seen := map[string]bool{metric.Name: true}
	add := func(name, source string) error {
		g, ok := series[name]
		if seen[name] || !ok || g.Outcome == nil || !g.Outcome.Complete || len(guards) >= maxGuards {
			return nil
		}
		gs := c.guardSubject(cat, p, g, subject)
		if gs == "" {
			// A guard whose subject the change does not name measures nothing of it.
			return nil
		}
		seen[name] = true
		v, err := trialValue(ctx, tx, p.Tenant, g, gs, from, now, author, p.Role)
		if err != nil {
			return err
		}
		if !slices.Contains(subjects, any(gs)) {
			subjects = append(subjects, gs)
		}
		guards = append(guards, map[string]any{"series": name, "subject": gs, "compare": g.Outcome.Compare, "before": v["value"],
			"before_samples": v["samples"], "source": source, "broken_at": nil})
		return nil
	}
	for _, name := range c.targetGuards(cat, p) {
		if err := add(name, "target"); err != nil {
			return nil, err
		}
	}
	for _, name := range c.metricGuards(metric.Name) {
		if err := add(name, "metric"); err != nil {
			return nil, err
		}
	}
	if tv := cat.Tenants[p.Tenant]; tv != nil {
		for _, eg := range tv.Spec.Insights.ExtraGuards {
			if eg.Metric != metric.Name {
				continue
			}
			for _, name := range eg.Guards {
				if err := add(name, "extra_guards"); err != nil {
					return nil, err
				}
			}
		}
	}
	if len(guards) == 0 {
		// A metric without guards of its own is guarded by the task outcome of its subject.
		seen[metric.Name] = false
		if err := add("farm.task.outcome", "metric"); err != nil {
			return nil, err
		}
	}
	if len(guards) == 0 {
		// With no guard on a subject of the change the metric guards itself (trial.guards has
		// at least one entry).
		seen[metric.Name] = false
		if err := add(metric.Name, "metric"); err != nil {
			return nil, err
		}
	}
	if len(guards) == 0 {
		return nil, fmt.Errorf("%w: proposal %s: no guard can be measured", errNoTrial, p.ID)
	}
	out := map[string]any{
		"metric":   map[string]any{"series": metric.Name, "subject": subject, "compare": metric.Outcome.Compare, "better": metric.Outcome.Better},
		"promised": promised, "subjects": subjects, "before": before, "guards": guards,
		"window": map[string]any{"from": "realized", "days": trialDays, "starts_at": nil}, "state": "baseline",
		"self_measured": false, "outcome": nil,
	}
	// From v0.3 every trial also guards the farm's work over the whole tenant, and a tenant
	// cannot remove these guards (PRP-26).
	if claimsV03() {
		var farm []any
		for _, name := range farmGuards {
			g, ok := series[name]
			if !ok || g.Outcome == nil {
				continue
			}
			v, err := trialValue(ctx, tx, p.Tenant, g, "tenant:"+p.Tenant, from, now, author, p.Role)
			if err != nil {
				return nil, err
			}
			farm = append(farm, map[string]any{"series": name, "subject": "tenant:" + p.Tenant, "compare": g.Outcome.Compare, "before": v["value"],
				"before_samples": v["samples"], "source": "farm", "broken_at": nil})
		}
		out["farm_guards"] = farm
	}
	return out, nil
}

// farmGuards are the two guards of the farm's work every trial carries from v0.3, over the
// whole tenant: the share of successful tasks and the 95th percentile of the queue wait (PRP-26).
var farmGuards = []string{"farm.task.outcome", "farm.queue.wait"}

// guardSubject is the subject a guard series is measured on (PRP-21): the benefit subject when
// the guard is of its catalog kind, otherwise the object of that kind the change targets, or the
// task type of a targeted schedule; "" when the change names no subject of that kind.
func (c *Core) guardSubject(cat *Catalog, p *proposalRow, g seriesDef, subject string) string {
	if k, _, _ := strings.Cut(subject, ":"); k == g.CatalogKind {
		return subject
	}
	kinds := map[string]string{"Schedule": "schedule", "TaskType": "task_type", "Role": "role"}
	var cands []string
	if mc := p.body().Change.ManifestChange; mc != nil {
		for _, op := range mc.Ops {
			if kinds[op.Target.Kind] == g.CatalogKind {
				cands = append(cands, g.CatalogKind+":"+op.Target.Name)
			}
			if op.Target.Kind == "Schedule" && g.CatalogKind == "task_type" {
				if sc, ok := cat.Tenants[p.Tenant].Schedules[op.Target.Name]; ok && sc.Task != nil {
					cands = append(cands, "task_type:"+sc.Task.Type)
				}
			}
		}
	}
	if k, id, _ := strings.Cut(subject, ":"); k == "schedule" && g.CatalogKind == "task_type" {
		if sc, ok := cat.Tenants[p.Tenant].Schedules[id]; ok && sc.Task != nil {
			cands = append(cands, "task_type:"+sc.Task.Type)
		}
	}
	for _, s := range cands {
		if c.subjectOfTenant(cat, p.Tenant, g.CatalogKind, s) {
			return s
		}
	}
	return ""
}

// targetGuards are the guards of the registry table by the kind of the change target.
func (c *Core) targetGuards(cat *Catalog, p *proposalRow) []string {
	b := p.body()
	target := b.Change.Kind
	if target == "manifest_change" && b.Change.ManifestChange != nil && len(b.Change.ManifestChange.Ops) > 0 {
		op := b.Change.ManifestChange.Ops[0]
		switch {
		case op.Target.Kind == "Schedule":
			target = "schedule"
			if sc, ok := cat.Tenants[p.Tenant].Schedules[op.Target.Name]; ok && sc.Task != nil && cat.TaskTypes[sc.Task.Type].MakesBackup {
				target = "backup_schedule_window"
			}
		case strings.HasPrefix(op.Field, "spec.dispatch."):
			target = "dispatch_rule"
		default:
			target = "tenant_limits"
		}
	}
	raw, err := spec.ReadYAMLFile(filepath.Join(c.spec.Root, "spec/model/core-series.yaml"))
	if err != nil {
		return nil
	}
	list, _ := mmapAny(raw)["target_guards"].([]any)
	for _, e := range list {
		m, _ := e.(map[string]any)
		if m["target"] == target {
			return strList(m["guards"])
		}
	}
	return nil
}

// metricGuards are the guards of the registry table by the metric.
func (c *Core) metricGuards(metric string) []string {
	reg, err := c.spec.Registry("spec/model/core-series.yaml")
	if err != nil {
		return nil
	}
	e, ok := reg.Entry(metric)
	if !ok {
		return nil
	}
	return strList(e["guards_by_metric"])
}

// trialValue computes a metric by its compare rollup over the points of a period: over all
// points, without the author's worker and without its role (PRP-22). window.share is the share
// of points equal to the good value, a window rollup its function over all points, an hour or
// day rollup the mean of its hourly or daily values.
func trialValue(ctx context.Context, tx pgx.Tx, tenant string, s seriesDef, subject string, from, to time.Time, worker, role string) (map[string]any, error) {
	if s.Outcome == nil {
		none := func() map[string]any { return map[string]any{"value": nil, "samples": 0} }
		out := none()
		out["without_author"], out["without_role"] = none(), none()
		return out, nil
	}
	one := func(column, value string) (map[string]any, error) {
		args := []any{tenant, s.Name, subject, from, to}
		// The attempts of a takeover on the seat model do not enter the trial of the role
		// (MGR-48, PRP-22): their lease, task and event points are left out.
		const notTakeover = notTakeoverPoints
		where := `tenant = $1 AND series = $2 AND subject = $3 AND at >= $4 AND at < $5` + notTakeover
		if subject == "tenant:"+tenant {
			// A farm guard reads the series over every subject of the tenant (PRP-26).
			where = `tenant = $1 AND series = $2 AND $3 <> '' AND at >= $4 AND at < $5` + notTakeover
		}
		if column != "" {
			args = append(args, value)
			where += fmt.Sprintf(` AND %s IS DISTINCT FROM $%d`, column, len(args))
		}
		var q string
		period, fn, _ := strings.Cut(s.Outcome.Compare, ".")
		agg := `avg(num)`
		switch fn {
		case "max":
			agg = `max(num)`
		case "p95":
			// The nearest rank, as the observe loop computes it (OBV-4).
			agg = `percentile_disc(0.95) WITHIN GROUP (ORDER BY num)`
		}
		switch {
		case fn == "share":
			args = append(args, s.Outcome.Good)
			q = fmt.Sprintf(`SELECT avg(CASE WHEN enum = $%d THEN 1.0 ELSE 0.0 END), count(*) FROM obs_points WHERE %s`, len(args), where)
		case period == "hour" || period == "day":
			// A rollup per hour or day is compared as the mean of its values over the window.
			q = `SELECT avg(m), coalesce(sum(n), 0)::int FROM (SELECT ` + agg + ` AS m, count(num) AS n FROM obs_points WHERE ` + where +
				` GROUP BY date_trunc('` + period + `', at)) d`
		default:
			q = `SELECT ` + agg + `, count(num) FROM obs_points WHERE ` + where
		}
		var v *float64
		var n int
		if err := tx.QueryRow(ctx, q, args...).Scan(&v, &n); err != nil {
			return nil, err
		}
		out := map[string]any{"value": nil, "samples": n}
		if v != nil {
			out["value"] = *v
		}
		return out, nil
	}
	all, err := one("", "")
	if err != nil {
		return nil, err
	}
	wa, err := one("worker", worker)
	if err != nil {
		return nil, err
	}
	wr, err := one("role", role)
	if err != nil {
		return nil, err
	}
	all["without_author"], all["without_role"] = wa, wr
	return all, nil
}

// presentedStep expires a card nobody decided within decide_ttl (PRP-6).
func (c *Core) presentedStep(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	at := p.StateAt
	if p.PresentedAt != nil {
		at = *p.PresentedAt
	}
	if now.Sub(at) <= durationOr(tv.Spec.Insights.DecideTTL, defaultDecideTTL) {
		return nil
	}
	if err := c.moveProposal(ctx, tx, now, p, "expired", ""); err != nil {
		return err
	}
	return c.planProposalCardUpdates(ctx, tx, now, p)
}

// realizingStep follows the realization (PRP-18, PRP-19): the approval of "Accept" executed is
// realized with the link object and starts the trial window; an approval that ended otherwise
// fails as object_failed, and its own expiry ends it; a change done by hand that is not linked by
// realize_by fails as timeout with the trial canceled.
// At half of the period the realizer of a manifest or manual change gets a reminder.
func (c *Core) realizingStep(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	r := mmapAny(p.Rec["realization"])
	by, err := time.Parse(time.RFC3339, asString(r["realize_by"]))
	if err != nil {
		return fmt.Errorf("proposal %s: realize_by %v", p.ID, r["realize_by"])
	}
	fail := func(reason string) error {
		if t := mmapAny(p.Rec["trial"]); t != nil {
			t["state"], t["outcome"] = "canceled", "canceled"
		}
		if err := c.moveProposal(ctx, tx, now, p, "realization_failed", reason); err != nil {
			return err
		}
		return c.planProposalCardUpdates(ctx, tx, now, p)
	}
	if obj := asString(r["object"]); strings.HasPrefix(obj, "scr_") {
		// A script change is realized by its run (PRP-3).
		switch st, err := scriptRealized(ctx, tx, obj); {
		case err != nil:
			return err
		case st == "failed":
			return fail("object_failed")
		case st == "realized":
			r["link"], r["realized_at"] = "object", fmtTime(now)
			if t := mmapAny(p.Rec["trial"]); t != nil {
				t["state"] = "running"
				mmapAny(t["window"])["starts_at"] = fmtTime(now)
			}
			if err := c.moveProposal(ctx, tx, now, p, "realized", ""); err != nil {
				return err
			}
			return c.planProposalCardUpdates(ctx, tx, now, p)
		}
		return nil
	} else if obj != "" {
		var state string
		var at time.Time
		err := tx.QueryRow(ctx, `SELECT state, coalesce(decided_at, created_at) FROM approvals WHERE id = $1`, obj).Scan(&state, &at)
		if errors.Is(err, pgx.ErrNoRows) {
			return fail("object_failed")
		}
		if err != nil {
			return err
		}
		switch state {
		case "executed":
			r["link"], r["realized_at"] = "object", fmtTime(now)
			if t := mmapAny(p.Rec["trial"]); t != nil {
				t["state"] = "running"
				mmapAny(t["window"])["starts_at"] = fmtTime(now)
			}
			if err := c.moveProposal(ctx, tx, now, p, "realized", ""); err != nil {
				return err
			}
			return c.planProposalCardUpdates(ctx, tx, now, p)
		case "rejected", "expired", "canceled", "exec_failed", "exec_unknown":
			return fail("object_failed")
		default:
			// The approval's own expiry ends a request nobody approved; one approved or
			// executing is not timed out under it.
			return nil
		}
	}
	if now.After(by) {
		return fail("timeout")
	}
	// The reminder at half of the period goes once to the realizer of a change done by hand.
	if r["object"] == nil && r["reminded_at"] == nil {
		decided, _ := time.Parse(time.RFC3339, asString(mmapAny(p.Rec["decision"])["at"]))
		if !decided.IsZero() && now.After(decided.Add(by.Sub(decided)/2)) {
			r["reminded_at"] = fmtTime(now)
			if err := c.saveProposal(ctx, tx, now, p); err != nil {
				return err
			}
			return c.planProposalCard(ctx, tx, now, tv, p, realizationCard, "human:"+asString(r["realizer"]), map[string]any{"reminder": true})
		}
	}
	return nil
}
