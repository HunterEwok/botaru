// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Campaign proposals: P1 campaign.propose and the admission of a plan in one transaction (ORC-1, ORC-5, ORC-6, ORC-8, ORC-14).

// P1CampaignPropose is P1 campaign.propose: a live attempt of a role that proposes campaigns
// proposes one with its lease token, as P3 propose_campaign does (ORC-1); the idempotency_key
// of the request names the proposal within the attempt's task, so a repeat with the same key
// and plan is the same campaign and another plan is idempotency_conflict. The v0.2 rollout
// fields waves and rate_per_min (ORC-15) are deferred and refused.
func (c *Core) P1CampaignPropose(ctx context.Context, w *Worker, header string, args map[string]any) (map[string]any, error) {
	if r, ok := args["rollout"].(map[string]any); ok {
		for _, f := range []string{"waves", "rate_per_min"} {
			if _, has := r[f]; has {
				return nil, refuse("version_unsupported", "rollout.%s (ORC-15) is not served by this version", f)
			}
		}
	}
	key, _ := args["idempotency_key"].(string)
	body := map[string]any{}
	for k, v := range args {
		if k != "idempotency_key" {
			body[k] = v
		}
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out campaignOutcome
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		l, _, err := c.heldLease(ctx, tx, w, header, "campaign.propose", nil)
		if err != nil {
			return err
		}
		if l.Kind != "attempt" || l.State != "active" || !l.Until.After(now) {
			return refuse("fenced", "a campaign is proposed under a live attempt lease")
		}
		g := &toolsGrant{Tenant: l.Tenant, Task: l.WorkID, Worker: w.ID, Role: w.Role, LeaseNo: l.LeaseNo}
		if err := c.loadGrantTask(ctx, tx, g); err != nil {
			return err
		}
		if key == "" {
			out, err = c.proposeCampaign(ctx, tx, now, cat, g, true, body)
		} else {
			out, err = c.proposeCampaignKeyed(ctx, tx, now, cat, g, true, body, key)
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	// A refusal is problem details in binding v1 (spec/bindings/http/README.md, "Errors").
	if out.State == "rejected" {
		return nil, refuse(out.Reason, "the proposal is refused")
	}
	return map[string]any{"campaign": out.Campaign, "state": out.State}, nil
}

// proposeCampaign is P3 propose_campaign: it turns the request into a plan from the attempt's
// tree and admits it (ORC-1).
func (c *Core) proposeCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (campaignOutcome, error) {
	return c.proposeCampaignKeyed(ctx, tx, now, cat, g, offered, args, "")
}

// proposeCampaignKeyed is proposeCampaign with the idempotency key of P1 campaign.propose; P3
// carries none (IDM-1, IDM-2).
func (c *Core) proposeCampaignKeyed(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any, key string) (campaignOutcome, error) {
	deny := func(reason string) (campaignOutcome, error) {
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: g.Tenant, Kind: "authorize", Actor: Actor{Principal: workerPrincipal(g.Worker), Via: "bot", Role: g.Role, Task: g.Task},
			Command: "campaign.propose", Target: &Target{Kind: "campaign", ID: "-"}, OK: false, Reason: reason,
		}); err != nil {
			return campaignOutcome{}, err
		}
		return campaignOutcome{State: "rejected", Reason: reason}, nil
	}
	if !offered || roleProposes(cat, g) == "none" {
		return deny("forbidden")
	}
	if err := c.spec.Validate("campaign-propose-request", args); err != nil || key == "" && args["idempotency_key"] != nil {
		return deny("input_schema_invalid")
	}
	tmpl, _ := args["template"].(map[string]any)
	p := campaignPlan{Tenant: g.Tenant, Initiator: g.Initiator, ParentTask: g.Task, Role: g.Role, Template: tmpl, Source: "bot",
		EnqueuedBy: map[string]any{"role": g.Role, "worker": g.Worker, "attempt": max(g.LeaseNo, 1)}}
	p.Kind, _ = tmpl["kind"].(string)
	switch p.Kind {
	case "task":
		p.Type, _ = tmpl["type"].(string)
		if !slices.Contains(campaignTypes(cat, g), p.Type) {
			return deny("type_not_allowed")
		}
	case "action":
		p.Action, _ = tmpl["action"].(string)
		if !slices.Contains(campaignActions(cat, g), p.Action) {
			return deny("forbidden")
		}
		p.Type = "action." + p.Action
	}
	tree, err := loadTree(ctx, tx, g.Task)
	if err != nil {
		return campaignOutcome{}, err
	}
	var rec []byte
	if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, g.Task).Scan(&rec); err != nil {
		return campaignOutcome{}, err
	}
	var tr struct {
		UntrustedSource string `json:"untrusted_source"`
	}
	_ = json.Unmarshal(rec, &tr)
	p.Untrusted, p.UntrustedSource, p.Mark, p.MaxTier = tree.UntrustedSeen, tr.UntrustedSource, tree.DataMark, tree.MaxTier
	p.Root, p.Depth, p.Conversation, p.Trace, p.Origin, p.CoInitiators = tree.Root, max(tree.Depth, tree.DepthFloor), tree.Conversation, tree.Trace, tree.Origin, tree.CoInitiators
	p.WakeTask = tree.WakeTask
	items, _ := args["items"].(map[string]any)
	if sel, ok := items["selector"].(string); ok {
		p.Selector = true
		list, ok := expandSelector(cat, g.Tenant, sel)
		if !ok || len(list) == 0 {
			return deny("input_schema_invalid")
		}
		allowed := c.campaignResources(cat, g.Tenant, p.Mark)
		for _, r := range list {
			if !slices.Contains(allowed, r) {
				return deny("input_schema_invalid")
			}
		}
		p.Resources = list
	} else {
		allowed := c.campaignResources(cat, g.Tenant, p.Mark)
		for _, r := range items["resources"].([]any) {
			s := r.(string)
			if !slices.Contains(allowed, s) {
				return deny("input_schema_invalid")
			}
			p.Resources = append(p.Resources, s)
		}
		own, _ := items["params"].([]any)
		if a, ok := items["args"].([]any); ok {
			own = a
		}
		if own != nil && len(own) != len(p.Resources) {
			return deny("input_schema_invalid")
		}
		for _, o := range own {
			m, _ := o.(map[string]any)
			p.ItemArgs = append(p.ItemArgs, m)
		}
	}
	if raw, ok := args["rollout"].(map[string]any); ok {
		// The rollout waves and the release rate (ORC-15) are deferred: refused, never ignored.
		if raw["waves"] != nil || raw["rate_per_min"] != nil {
			return deny("version_unsupported")
		}
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &p.Rollout); err != nil {
			return deny("input_schema_invalid")
		}
		if _, ok := raw["max_failures"]; !ok {
			p.Rollout.MaxFailures = nil
		}
	}
	if th, ok := args["then"].(map[string]any); ok {
		t, _ := th["type"].(string)
		role := cat.Roles[g.Role]
		if role.Campaigns == nil || !slices.Contains(role.Campaigns.Then, t) {
			return deny("type_not_allowed")
		}
		p.Then = &campaignThen{Type: t}
	}
	if to, ok := args["to"].(map[string]any); ok {
		grp, _ := to["group"].(string)
		if !slices.Contains(cat.Roles[g.Role].MayTargetGroups, grp) {
			return deny("forbidden")
		}
		p.ToGroup = grp
	}
	// One attempt may propose several campaigns; a retried call with the same plan is the same
	// campaign (IDM-2).
	p.Key = "system:propose:" + g.Task + ":"
	if key != "" {
		// The P1 key of the proposer, under the prefix of its task; it is not completed with
		// the plan hash, so another plan under it is a conflict (ORC-1).
		p.Key = "system:propose:" + g.Task + ":p1:" + key + "."
	}
	out, err := c.admitCampaign(ctx, tx, now, cat, &p)
	if err != nil {
		return campaignOutcome{}, err
	}
	if out.State == "rejected" {
		return deny(out.Reason)
	}
	return out, nil
}

// campaignDefaults fills the policy the proposal left out from the action's ceilings.
func campaignDefaults(r *campaignRollout, act *manifest.ActionCampaign) {
	if act != nil {
		r.Canary = max(r.Canary, act.CanaryMin)
		if r.AfterCanary == "" {
			r.AfterCanary = act.AfterCanary
		}
		if r.MaxInFlight == 0 {
			r.MaxInFlight = act.MaxInFlight
		}
		if r.MaxFailures == nil && act.MaxFailuresMax != nil {
			r.MaxFailures = *act.MaxFailuresMax
		}
	}
	if r.AfterCanary == "" {
		r.AfterCanary = "manual"
	}
	if r.MaxInFlight == 0 {
		r.MaxInFlight = 1
	}
	if r.MaxFailures == nil {
		r.MaxFailures = 0
	}
}

// actionTargetParam is the parameter of an action's executor that names its machine, or "".
func actionTargetParam(cat *Catalog, act manifest.Action) string {
	if pl, ok := cat.Plugins[act.Executor.Plugin]; ok {
		if t, ok := pl.Tool(act.Executor.Tool); ok && t.Target != "any" && t.TargetFrom != "" {
			return t.TargetFrom
		}
	}
	return ""
}

// itemArgs builds the arguments of one item: the template's, the item's own over them, and the
// resource in the parameter the executor targets (an action) or in params.resource (a task).
func itemArgs(cat *Catalog, p *campaignPlan, i int) map[string]any {
	base, _ := p.Template["args"].(map[string]any)
	if p.Kind == "task" {
		base, _ = p.Template["params"].(map[string]any)
	}
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	if i < len(p.ItemArgs) {
		for k, v := range p.ItemArgs[i] {
			out[k] = v
		}
	}
	res := p.Resources[i]
	if p.Kind == "action" {
		act := cat.Actions[p.Action]
		if tf := actionTargetParam(cat, act); tf != "" {
			if _, set := out[tf]; !set {
				out[tf] = res
			}
		}
		return actionDefaults(act, out)
	}
	props, _ := cat.TaskTypes[p.Type].ParamsSchema["properties"].(map[string]any)
	if _, ok := props["resource"]; ok {
		if _, set := out["resource"]; !set {
			out["resource"] = res
		}
	}
	return out
}

// admitCampaign checks a plan and records the campaign in one transaction (ORC-1, ORC-2,
// ORC-5, ORC-6, ORC-8, ORC-14). A refusal records nothing and takes no reserve.
func (c *Core) admitCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, p *campaignPlan) (campaignOutcome, error) {
	tv := cat.Tenants[p.Tenant]
	if tv == nil {
		return campaignOutcome{State: "rejected", Reason: "not_ready"}, nil
	}
	no := func(reason string) (campaignOutcome, error) {
		return campaignOutcome{State: "rejected", Reason: reason}, nil
	}
	var act manifest.Action
	var actCampaign *manifest.ActionCampaign
	if p.Kind == "action" {
		a, ok := cat.Actions[p.Action]
		// The campaign approval is one press; an action that needs two approvers is never
		// run under it (APR-16).
		if !ok || a.Campaign == nil || !a.Campaign.Allowed || a.Approvals >= 2 {
			return no("campaign_policy_exceeded")
		}
		act, actCampaign = a, a.Campaign
	} else if _, ok := cat.TaskTypes[p.Type]; !ok || strings.HasPrefix(p.Type, "action.") {
		return no("type_not_allowed")
	}
	campaignDefaults(&p.Rollout, actCampaign)
	n := len(p.Resources)
	// ORC-5: no more items, in flight or failures than the ceilings, no smaller canary.
	limit := defaultCampaignItems
	if m := tv.Spec.Campaigns.MaxItems; m > 0 {
		limit = m
	}
	limit = min(limit, farmMaxCampaignItems)
	if actCampaign != nil && actCampaign.MaxItems > 0 {
		limit = min(limit, actCampaign.MaxItems)
	}
	r := p.Rollout
	switch {
	case n == 0 || n > limit:
		return no("campaign_policy_exceeded")
	case actCampaign != nil && actCampaign.MaxInFlight > 0 && r.MaxInFlight > actCampaign.MaxInFlight:
		return no("campaign_policy_exceeded")
	case actCampaign != nil && r.Canary < actCampaign.CanaryMin:
		return no("campaign_policy_exceeded")
	case r.AfterCanary == "auto" && (actCampaign == nil && p.Kind == "action" || actCampaign != nil && actCampaign.AfterCanary != "auto"):
		return no("campaign_policy_exceeded")
	case actCampaign != nil && actCampaign.MaxFailuresMax != nil && r.threshold(n) > *actCampaign.MaxFailuresMax:
		return no("campaign_policy_exceeded")
	case r.threshold(n) > campaignThresholdCeiling:
		return no("campaign_policy_exceeded")
	}
	if r.Deadline != "" {
		if _, ok := parseDuration(r.Deadline); !ok {
			return no("input_schema_invalid")
		}
	}
	if r.Bake != "" {
		if _, ok := parseDuration(r.Bake); !ok {
			return no("input_schema_invalid")
		}
	}
	// ORC-14: under distrust or for system, a short explicit list only; a longer read
	// campaign needs a person.
	forceCard := p.ForceCard
	if p.Untrusted || p.Initiator == "system" {
		maxU := defaultUntrustedItems
		if m := tv.Spec.Campaigns.MaxItemsUntrusted; m != nil {
			maxU = *m
		}
		switch {
		case p.Selector:
			return no("campaign_untrusted_limit")
		case p.Kind == "action" && n > maxU:
			return no("campaign_untrusted_limit")
		case p.Kind == "task" && n > untrustedReadItems:
			forceCard = true
		}
	}
	// The arguments of each item pass the closed schema of the type or the action.
	args := make([]map[string]any, n)
	hashes := make([]string, n)
	var argsSchema map[string]any
	if p.Kind == "action" {
		s, err := c.actionArgsSchema(cat, p.Tenant, p.Mark, act)
		if err != nil {
			return campaignOutcome{}, err
		}
		argsSchema = s
	}
	for i := range n {
		args[i] = itemArgs(cat, p, i)
		// An item acts on its own machine and nothing else: the card shows the machines and the
		// template, so an action item has no arguments of its own beside its target, and the
		// target is the item's resource (ORC-2, ORC-7, ORC-12).
		if p.Kind == "action" {
			tf := actionTargetParam(cat, act)
			if tf != "" && args[i][tf] != p.Resources[i] {
				return no("input_schema_invalid")
			}
			if i < len(p.ItemArgs) {
				for k := range p.ItemArgs[i] {
					if k != tf {
						return no("input_schema_invalid")
					}
				}
			}
		} else if r, set := args[i]["resource"]; set && r != p.Resources[i] {
			return no("input_schema_invalid")
		}
		if p.Kind == "action" {
			if c.rs.validate(argsSchema, args[i]) != nil {
				return no("input_schema_invalid")
			}
		} else if c.validateParams(cat.TaskTypes[p.Type], map[string]any{"params": args[i]}) != nil {
			return no("input_schema_invalid")
		}
		h, err := jcs.Hash(args[i])
		if err != nil {
			return campaignOutcome{}, err
		}
		hashes[i] = h
	}
	// ORC-6: every item on behalf of every initiator.
	mark := p.Mark
	if p.Kind == "task" {
		mark = c.higherMark(cat.TaskTypes[p.Type].DataClass, p.Mark)
	}
	for _, who := range append([]string{p.Initiator}, p.CoInitiators...) {
		groups, err := principalGroups(ctx, tx, now, tv, who)
		if err != nil {
			return campaignOutcome{}, err
		}
		if who != "system" {
			pi, err := loadPrincipal(ctx, tx, now, p.Tenant, who)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && pi.State != "active" {
				return no("person_inactive")
			}
			if err != nil {
				return campaignOutcome{}, err
			}
		}
		if p.Kind == "action" && !hasCap(tv, groups, "action.request:"+p.Action) ||
			p.Kind == "task" && (!hasCap(tv, groups, "task:"+p.Type) || !hasData(tv, groups, mark)) {
			return no("no_group_cap")
		}
	}
	if p.Then != nil {
		if _, ok := cat.TaskTypes[p.Then.Type]; !ok {
			return no("type_not_allowed")
		}
	}
	// A hold in the scope of an action campaign refuses it (ORC-11).
	if p.Kind == "action" {
		var parentType string
		if p.ParentTask != "" {
			_ = tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, p.ParentTask).Scan(&parentType)
		}
		if held, err := heldBy(ctx, tx, now, []string{"no_action", "pause_action", "no_enqueue"}, append(approvalScopes(p.Tenant, p.Action, p.Role, parentType), enqueuerWorkerScope(p.Tenant, p.EnqueuedBy)...)...); err != nil {
			return campaignOutcome{}, err
		} else if held != "" {
			return no("held")
		}
	}
	plan := map[string]any{"template": p.Template, "items": p.Resources, "args": hashes, "rollout": p.Rollout, "then": p.Then}
	planHash, err := jcs.Hash(plan)
	if err != nil {
		return campaignOutcome{}, err
	}
	// A repeat of the proposal with the same key is the same campaign; another plan under the
	// key is a conflict (ORC-1, IDM-1). A key ending in ":" is completed with the plan hash.
	if strings.HasSuffix(p.Key, ":") {
		p.Key += planHash
	}
	if p.Key != "" {
		var id, hash, state string
		err := tx.QueryRow(ctx, `SELECT id, plan_hash, state FROM campaigns WHERE tenant = $1 AND idempotency_key = $2`, p.Tenant, p.Key).Scan(&id, &hash, &state)
		if err == nil {
			if hash != planHash {
				return no("idempotency_conflict")
			}
			return campaignOutcome{Campaign: id, State: state}, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return campaignOutcome{}, err
		}
	}
	// ORC-8: the whole tree size and the whole estimate are reserved now.
	treeLimit := defaultMaxTreeTasks
	if m := tv.Spec.Campaigns.MaxTreeTasks; m > 0 {
		treeLimit = m
	}
	treeLimit = min(treeLimit, farmMaxTreeTasks)
	extra := n
	if p.Then != nil {
		extra++
	}
	if p.ScriptRun == "" {
		if _, err := tx.Exec(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, p.Root); err != nil {
			return campaignOutcome{}, err
		}
		used, err := treeSize(ctx, tx, p.Tenant, p.Root)
		if err != nil {
			return campaignOutcome{}, err
		}
		if used+extra > treeLimit {
			return no("tree_too_large")
		}
	}
	var itemMicros, thenMicros int64
	line := "work"
	if p.Kind == "task" {
		tt := cat.TaskTypes[p.Type]
		if itemMicros, err = taskEstimate(ctx, tx, now, p.Tenant, p.Type, tt); err != nil {
			return campaignOutcome{}, err
		}
		line = c.budgetLine(tt.Purpose)
	}
	if p.Then != nil {
		if thenMicros, err = taskEstimate(ctx, tx, now, p.Tenant, p.Then.Type, cat.TaskTypes[p.Then.Type]); err != nil {
			return campaignOutcome{}, err
		}
	}
	total := itemMicros*int64(n) + thenMicros
	card := p.Kind == "action" || forceCard
	if p.ScriptRun != "" {
		card = p.Kind == "action"
	}
	confirmItems := defaultConfirmReadItems
	if v := tv.Spec.Campaigns.ConfirmReadOver.Items; v > 0 {
		confirmItems = v
	}
	confirmEUR := defaultConfirmReadEUR
	if v := tv.Spec.Campaigns.ConfirmReadOver.EUR; v != nil {
		confirmEUR = *v
	}
	if !card && p.ScriptRun == "" && (n > confirmItems || total > money.FromEUR(confirmEUR)) {
		card = true
	}
	overDay := false
	groups, err := principalGroups(ctx, tx, now, tv, p.Initiator)
	if err != nil {
		return campaignOutcome{}, err
	}
	sysGroups := groupClosure(tv, []string{"system-ops"})
	var crossed []crossing
	if p.ScriptRun != "" {
		// The run reserve pays the step: its share moves to the campaign below (SCR-8).
	} else if !card {
		if pb := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: p.Initiator, groups: groups, extra: total, line: line, crossed: &crossed}); pb != nil {
			// Only the person's day short: a card instead of a refusal (ORC-8, D-53).
			if p.Initiator == "system" || pb.Reason != "budget_exhausted" ||
				c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", groups: sysGroups, extra: total, line: line}) != nil {
				return no("budget_exhausted")
			}
			card, overDay, crossed = true, true, nil
		}
	} else if pb := c.checkBudget(ctx, tx, now, tv, budgetAsk{subject: "system", groups: sysGroups, extra: total, line: line, crossed: &crossed}); pb != nil {
		return no("budget_exhausted")
	}
	id := ids.New(ids.Campaign)
	var src *string
	if p.Untrusted {
		s := p.UntrustedSource
		if s == "" {
			s = "parent"
		}
		src = &s
	}
	rec := campaignRec{
		Template: p.Template, Rollout: p.Rollout, Then: p.Then, ToGroup: p.ToGroup, Refs: p.Refs, MaxTier: p.MaxTier, DataMark: mark,
		Untrusted: campaignUntrusted{Value: p.Untrusted, Source: src}, Reserve: campaignReserve{TreeTasks: extra, EUR: float64(money.CentsUp(total)) / 100},
		EnqueuedBy: p.EnqueuedBy, Root: p.Root, Depth: p.Depth, Conversation: p.Conversation, Trace: p.Trace, Origin: p.Origin,
		CoInitiators: p.CoInitiators, WakeTask: p.WakeTask, ItemMicros: itemMicros, Line: line, PaidByCard: card || p.ScriptRun != "", OverDayLimit: overDay, Role: p.Role,
		ScriptRun: p.ScriptRun, ScriptStep: p.ScriptStep,
	}
	if p.Source == "human" {
		rec.EnqueuedBy = nil
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return campaignOutcome{}, err
	}
	var expires any
	if card {
		ttl := defaultCampaignCardTTL
		if p.Kind == "action" {
			ttl = actionTTL(act)
		}
		expires = now.Add(ttl)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO campaigns (id, tenant, parent_task, initiator, state, plan_hash, record, idempotency_key, nonce, card, expires_at, created_at, updated_at)
		VALUES ($1, $2, $3, $4, 'proposed', $5, $6, $7, $8, $9, $10, $11, $11)`,
		id, p.Tenant, nullable(p.ParentTask), p.Initiator, planHash, raw, nullable(p.Key), ids.Secret(16), card, expires, now); err != nil {
		return campaignOutcome{}, err
	}
	for i := range n {
		a, _ := json.Marshal(args[i])
		if _, err := tx.Exec(ctx, `INSERT INTO campaign_items (campaign, n, resource, args, args_hash, state) VALUES ($1, $2, $3, $4, $5, 'pending')`,
			id, i+1, p.Resources[i], a, hashes[i]); err != nil {
			return campaignOutcome{}, err
		}
	}
	if total > 0 {
		subject := p.Initiator
		if card || p.ScriptRun != "" {
			subject = "system"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, root, subject, line, purpose, unit, state, eur_micros, time, campaign)
			VALUES ($1, $2, $3, $4, $5, $6, 'task', 'reserved', $7, $8, $3)`, ids.New(ids.Spend), p.Tenant, id, subject, line, line, total, now); err != nil {
			return campaignOutcome{}, err
		}
	}
	if err := c.emitCrossed(ctx, tx, now, p.Tenant, crossed); err != nil {
		return campaignOutcome{}, err
	}
	if p.ScriptRun != "" {
		if err := shrinkRunReserve(ctx, tx, p.ScriptRun, total, n); err != nil {
			return campaignOutcome{}, err
		}
	}
	cmp, err := loadCampaign(ctx, tx, p.Tenant, id, true)
	if err != nil {
		return campaignOutcome{}, err
	}
	if err := c.campaignEvent(ctx, tx, now, cmp, "campaign.proposed.v1", "", ""); err != nil {
		return campaignOutcome{}, err
	}
	actor := Actor{Principal: p.Initiator, Via: "core"}
	if p.Source == "bot" {
		w, _ := p.EnqueuedBy["worker"].(string)
		actor = Actor{Principal: workerPrincipal(w), Via: "bot", Role: p.Role, Task: p.ParentTask}
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: p.Tenant, Kind: "authorize", Actor: actor, Command: "campaign.propose", Target: &Target{Kind: "campaign", ID: id}, OK: true,
		After: map[string]any{"items": n, "kind": p.Kind, "card": card, "plan_hash": planHash},
	}); err != nil {
		return campaignOutcome{}, err
	}
	if card {
		if err := c.planCampaignCards(ctx, tx, now, cat, tv, cmp); err != nil {
			return campaignOutcome{}, err
		}
		return campaignOutcome{Campaign: id, State: "proposed"}, notifyWork(ctx, tx)
	}
	if err := c.startCampaign(ctx, tx, now, cat, tv, cmp); err != nil {
		return campaignOutcome{}, err
	}
	return campaignOutcome{Campaign: id, State: cmp.State}, nil
}

// treeSize is the tasks of a root with the pending items and unstarted continuations of its
// live campaigns: what the tree already has and what it has reserved (ORC-8).
func treeSize(ctx context.Context, q store.Querier, tenant, root string) (int, error) {
	var n int
	err := q.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE tenant = $1 AND (id = $2 OR record->>'root' = $2))
		+ (SELECT count(*) FROM campaign_items i JOIN campaigns c ON c.id = i.campaign
			WHERE c.tenant = $1 AND c.record->>'root' = $2 AND c.state = ANY($3) AND i.state = 'pending')
		+ (SELECT count(*) FROM campaigns c WHERE c.tenant = $1 AND c.record->>'root' = $2 AND c.state = ANY($3) AND c.record->'then' IS NOT NULL
			AND jsonb_typeof(c.record->'then') = 'object')
		+ (SELECT coalesce(sum(tree_left), 0) FROM script_runs WHERE tenant = $1 AND record->>'root' = $2 AND state IN ('running', 'paused'))`, tenant, root, campaignLive).Scan(&n)
	return n, err
}
