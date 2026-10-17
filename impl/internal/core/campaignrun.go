// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// Campaign runs: holds, the start, the release of items by the rollout policy, the threshold and the campaign loop.

// campaignScopes are the hold scopes of a campaign: the farm, the tenant, the item type, the
// action and the proposing role.
func campaignScopes(cmp *campaignRow) []scopeRef {
	s := append(tenantScopes(cmp.Tenant), scopeRef{kind: "tasktype", tenant: cmp.Tenant, name: cmp.Rec.itemType()})
	if a := cmp.Rec.action(); a != "" {
		s = append(s, scopeRef{kind: "action", tenant: cmp.Tenant, name: a})
	}
	if cmp.Rec.Role != "" {
		s = append(s, scopeRef{kind: "role", tenant: cmp.Tenant, name: cmp.Rec.Role})
	}
	return append(s, enqueuerWorkerScope(cmp.Tenant, cmp.Rec.EnqueuedBy)...)
}

// enqueuerWorkerScope is the scope of the worker that proposed a campaign or drafted its
// script, so that worker's own hold covers the campaign too (HLD-9).
func enqueuerWorkerScope(tenant string, by map[string]any) []scopeRef {
	if w, _ := by["worker"].(string); w != "" {
		return []scopeRef{{kind: "worker", tenant: tenant, name: strings.ToLower(w)}}
	}
	return nil
}

// campaignHoldEffects are the hold effects that pause a campaign (ORC-11).
var campaignHoldEffects = []string{"no_enqueue", "no_claim", "no_action", "cancel_running"}

// campaignHoldEffectsOf are the effects that pause a campaign: of an action campaign
// pause_action too, as it pauses a single waiting approval (HLD-3).
func campaignHoldEffectsOf(cmp *campaignRow) []string {
	if cmp.Rec.action() != "" {
		return append(slices.Clone(campaignHoldEffects), "pause_action")
	}
	return campaignHoldEffects
}

// setCampaignState moves a campaign and writes its event and card updates.
func (c *Core) setCampaignState(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow, state, pause, stop string) error {
	from := cmp.State
	if _, err := tx.Exec(ctx, `UPDATE campaigns SET state = $2, pause_reason = $3, stop_reason = $4, updated_at = $5 WHERE id = $1`,
		cmp.ID, state, nullable(pause), nullable(stop), now); err != nil {
		return err
	}
	cmp.State, cmp.PauseReason, cmp.StopReason = state, pause, stop
	reason := pause
	if stop != "" {
		reason = stop
	}
	if err := c.campaignEvent(ctx, tx, now, cmp, "campaign.state.v1", from, reason); err != nil {
		return err
	}
	return c.campaignCardUpdates(ctx, tx, now, cmp, true)
}

// campaignDefaultDeadline is the deadline of a campaign whose rollout names none.
const campaignDefaultDeadline = 7 * 24 * time.Hour

// startCampaign moves a proposed campaign to running: the deadline starts, the initiator gets
// the progress card, and the first items are released, or it pauses at once under a hold.
func (c *Core) startCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, cmp *campaignRow) error {
	// Without a deadline of its own a campaign ends after campaignDefaultDeadline: a paused
	// campaign nobody resumes does not hold its reserve and its tree forever (ORC-4).
	d, ok := parseDuration(cmp.Rec.Rollout.Deadline)
	if !ok {
		d, ok = campaignDefaultDeadline, true
	}
	if ok {
		at := now.Add(d)
		cmp.DeadlineAt = &at
		if _, err := tx.Exec(ctx, `UPDATE campaigns SET deadline_at = $2 WHERE id = $1`, cmp.ID, at); err != nil {
			return err
		}
	}
	if err := c.setCampaignState(ctx, tx, now, cmp, "running", "", ""); err != nil {
		return err
	}
	if cmp.Initiator != "system" && cmp.Rec.ScriptRun == "" {
		if err := c.planCampaignCard(ctx, tx, now, tv, cmp, campaignProgressCard, campaignProgressCard, cmp.Initiator); err != nil {
			return err
		}
	}
	return c.releaseItems(ctx, tx, now, cat, cmp)
}

// releaseItems is the release of ORC-4: deterministic code from the record releases pending
// items while the campaign runs, no hold covers it, fewer than max_in_flight are open, the
// resource is free, the type's queue is short and every initiator keeps the rights; the canary
// goes alone first. It finishes the campaign once nothing is left.
func (c *Core) releaseItems(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow) error {
	tv := cat.Tenants[cmp.Tenant]
	if tv == nil {
		return nil
	}
	n, err := campaignCount(ctx, tx, cmp.ID)
	if err != nil {
		return err
	}
	if cmp.State == "stopping" || (cmp.State == "running" || cmp.State == "paused") && n.Pending == 0 {
		if n.Released == 0 {
			return c.finishCampaign(ctx, tx, now, cat, cmp, n)
		}
		return nil
	}
	if cmp.State != "running" {
		return nil
	}
	if held, err := heldBy(ctx, tx, now, campaignHoldEffectsOf(cmp), campaignScopes(cmp)...); err != nil {
		return err
	} else if held != "" {
		return c.setCampaignState(ctx, tx, now, cmp, "paused", "held", "")
	}
	// The initiators keep the rights at every release (ORC-6, PRM-3).
	if stop, err := c.campaignRights(ctx, tx, now, tv, cmp); err != nil {
		return err
	} else if stop != "" {
		return c.stopCampaign(ctx, tx, now, cat, cmp, stop)
	}
	// An action that needs two approvers since the campaign was approved stops it (APR-16).
	if campaignTwoApprovers(cat, cmp) {
		return c.stopTwoApprovers(ctx, tx, now, cat, cmp)
	}
	total := n.total()
	canary := min(cmp.Rec.Rollout.Canary, total)
	inCanary := false
	if canary > 0 && !cmp.Rec.GatePassed {
		var open int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM campaign_items WHERE campaign = $1 AND n <= $2 AND state IN ('pending', 'released')`, cmp.ID, canary).Scan(&open); err != nil {
			return err
		}
		inCanary = open > 0
		if !inCanary {
			// The canary is done: a manual gate waits for a person, auto waits out bake.
			if cmp.Rec.Rollout.AfterCanary != "auto" {
				return c.setCampaignState(ctx, tx, now, cmp, "paused", "gate", "")
			}
			if cmp.GateAt == nil {
				cmp.GateAt = &now
				if _, err := tx.Exec(ctx, `UPDATE campaigns SET gate_at = $2 WHERE id = $1`, cmp.ID, now); err != nil {
					return err
				}
			}
			if bake, ok := parseDuration(cmp.Rec.Rollout.Bake); ok && now.Before(cmp.GateAt.Add(bake)) {
				return nil
			}
			if err := c.passGate(ctx, tx, cmp); err != nil {
				return err
			}
		}
	}
	slots := cmp.Rec.Rollout.MaxInFlight - n.Released
	if slots <= 0 {
		return nil
	}
	q := `SELECT n, resource, args, args_hash FROM campaign_items WHERE campaign = $1 AND state = 'pending'`
	if inCanary {
		q += ` AND n <= ` + strconv.Itoa(canary)
	}
	rows, err := tx.Query(ctx, q+` ORDER BY n`, cmp.ID)
	if err != nil {
		return err
	}
	type pending struct {
		n             int
		resource      string
		args          map[string]any
		hash          string
		argsRawFailed bool
	}
	var list []pending
	for rows.Next() {
		var x pending
		var raw []byte
		if err := rows.Scan(&x.n, &x.resource, &raw, &x.hash); err != nil {
			rows.Close()
			return err
		}
		x.argsRawFailed = json.Unmarshal(raw, &x.args) != nil
		list = append(list, x)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	queueMax := tv.Spec.Campaigns.QueueMax
	for _, x := range list {
		if slots <= 0 {
			break
		}
		wait, err := c.itemWait(ctx, tx, cat, cmp, x.resource, queueMax)
		if err != nil {
			return err
		}
		if wait != "" {
			if _, err := tx.Exec(ctx, `UPDATE campaign_items SET wait = $3 WHERE campaign = $1 AND n = $2`, cmp.ID, x.n, wait); err != nil {
				return err
			}
			if wait == "over_capacity" {
				break
			}
			continue
		}
		// The item must fit the rest of the reserve; otherwise the campaign pauses and only a
		// new campaign with a new estimate continues (ORC-8).
		var est int64
		if cmp.Rec.kind() == "task" {
			if est, err = taskEstimate(ctx, tx, now, cmp.Tenant, cmp.Rec.itemType(), cat.TaskTypes[cmp.Rec.itemType()]); err != nil {
				return err
			}
			var left int64
			if err := tx.QueryRow(ctx, `SELECT coalesce(sum(eur_micros), 0) FROM spend WHERE campaign = $1 AND unit = 'task' AND state = 'reserved'`, cmp.ID).Scan(&left); err != nil {
				return err
			}
			if est > left {
				return c.setCampaignState(ctx, tx, now, cmp, "paused", "estimate_exceeded", "")
			}
		}
		if x.argsRawFailed {
			continue
		}
		if err := c.releaseItem(ctx, tx, now, cat, cmp, x.n, x.resource, x.args, x.hash, est); err != nil {
			return err
		}
		slots--
		// An item that ended at once may have moved the campaign.
		if cmp.State != "running" {
			return nil
		}
	}
	return nil
}

// passGate records that the canary passed, so later releases do not wait for it again.
func (c *Core) passGate(ctx context.Context, tx pgx.Tx, cmp *campaignRow) error {
	cmp.Rec.GatePassed = true
	_, err := tx.Exec(ctx, `UPDATE campaigns SET record = jsonb_set(record, '{gate_passed}', 'true') WHERE id = $1`, cmp.ID)
	return err
}

// campaignRights rechecks the initiators at a release: an inactive one stops the campaign with
// person_inactive, a lost right with rights_revoked (ORC-6).
func (c *Core) campaignRights(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, cmp *campaignRow) (string, error) {
	for _, who := range append([]string{cmp.Initiator}, cmp.Rec.CoInitiators...) {
		if who != "system" {
			p, err := loadPrincipal(ctx, tx, now, cmp.Tenant, who)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && p.State != "active" {
				return "person_inactive", nil
			}
			if err != nil {
				return "", err
			}
		}
		groups, err := principalGroups(ctx, tx, now, tv, who)
		if err != nil {
			return "", err
		}
		if a := cmp.Rec.action(); a != "" && !hasCap(tv, groups, "action.request:"+a) ||
			cmp.Rec.kind() == "task" && (!hasCap(tv, groups, "task:"+cmp.Rec.itemType()) || !hasData(tv, groups, cmp.Rec.DataMark)) {
			return "rights_revoked", nil
		}
	}
	return "", nil
}

// itemWait says why an item waits: its resource has a released write call of an exclusive
// action (ORC-12), or the tenant queue of its type is at queue_max.
func (c *Core) itemWait(ctx context.Context, tx pgx.Tx, cat *Catalog, cmp *campaignRow, resource string, queueMax int) (string, error) {
	if a := cmp.Rec.action(); a != "" && cat.Actions[a].Exclusive == "resource" {
		// Single approvals count too, under the same resource lock (approvalwait.go).
		busy, err := resourceBusy(ctx, tx, cmp.Tenant, cat.Actions[a], resource)
		if err != nil {
			return "", err
		}
		if busy {
			return "resource_busy", nil
		}
	}
	if queueMax > 0 {
		var q int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND type = $2 AND state = 'submitted'`, cmp.Tenant, cmp.Rec.itemType()).Scan(&q); err != nil {
			return "", err
		}
		if q >= queueMax {
			return "over_capacity", nil
		}
	}
	return "", nil
}

// releaseItem releases one item in this transaction: pending to released, its task with the key
// system:campaign:<id>:<n>, and for an action the one write call with the item's arguments from
// the snapshot (ORC-3). A task that the enqueue refuses fails the item.
func (c *Core) releaseItem(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow, n int, resource string, args map[string]any, hash string, est int64) error {
	tag, err := tx.Exec(ctx, `UPDATE campaign_items SET state = 'released', wait = NULL, released_at = $3 WHERE campaign = $1 AND n = $2 AND state = 'pending'`, cmp.ID, n, now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	key := "system:campaign:" + cmp.ID + ":" + strconv.Itoa(n)
	if a := cmp.Rec.action(); a != "" {
		act := cat.Actions[a]
		task, err := c.insertActionTask(ctx, tx, now, actionTaskInput{
			Tenant: cmp.Tenant, Action: a, Args: args, Initiator: cmp.Initiator, SourceTask: cmp.ParentTask, Mark: cmp.Rec.DataMark,
			ApprovalID: cmp.ID, Key: key, Campaign: cmp.ID, CampaignItem: n, Resource: resource,
		})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE campaign_items SET task = $3 WHERE campaign = $1 AND n = $2`, cmp.ID, n, task); err != nil {
			return err
		}
		host := ""
		if isCatalogHost(cat, cmp.Tenant, resource) {
			host = resource
		}
		callID, err := c.insertWriteCall(ctx, tx, now, cat, writeCall{
			Tenant: cmp.Tenant, Task: task, Action: a, Act: act, Args: args, ArgsHash: hash, ApprovalID: cmp.ID, CampaignItem: n, Host: host,
		})
		if err != nil {
			return err
		}
		var state, reason string
		if err := tx.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM toolcalls WHERE id = $1`, callID).Scan(&state, &reason); err != nil {
			return err
		}
		if state != "queued" {
			return c.writeCallEnded(ctx, tx, now, callID, state, reason, nil)
		}
		return nil
	}
	if est > 0 {
		if _, err := tx.Exec(ctx, `UPDATE spend SET eur_micros = greatest(eur_micros - $2, 0) WHERE campaign = $1 AND unit = 'task' AND state = 'reserved'`, cmp.ID, est); err != nil {
			return err
		}
	}
	body := map[string]any{"type": cmp.Rec.itemType()}
	if goal, _ := cmp.Rec.Template["goal"].(string); goal != "" {
		body["goal"] = goal
	}
	if len(args) > 0 {
		body["params"] = args
	}
	var prio int
	if cmp.ParentTask != "" {
		prio = priorityOf(ctx, tx, cmp.ParentTask)
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: cmp.Tenant, Type: cmp.Rec.itemType(), Body: body, IdempotencyKey: key, Initiator: cmp.Initiator,
		EnqueuedBy: "campaign:" + cmp.ID, Source: "campaign", SourceID: cmp.ID, Cause: cmp.ID,
		Chain: &chainLink{
			Parent: cmp.ParentTask, Root: cmp.Rec.Root, Depth: cmp.Rec.Depth + 1, Mark: cmp.Rec.DataMark, MaxTier: cmp.Rec.MaxTier,
			Untrusted: cmp.Rec.Untrusted.Value, Conversation: cmp.Rec.Conversation, Trace: cmp.Rec.Trace, ParentPriority: prio,
			CoInitiators: cmp.Rec.CoInitiators, WakeTask: cmp.Rec.WakeTask,
		},
		Campaign: cmp.ID, CampaignItem: n, Scope: []string{resource}, PaidByCard: cmp.Rec.PaidByCard,
		TreeBudgetExtra: money.FromEUR(cmp.Rec.Reserve.EUR),
	})
	reason := ""
	switch {
	case err != nil:
		p, ok := AsProblem(err)
		if !ok {
			return err
		}
		reason = p.Reason
	case res.Rejected != nil:
		reason = res.Rejected.Reason
	}
	if reason != "" {
		if _, err := tx.Exec(ctx, `UPDATE campaign_items SET state = 'failed', wait = $3, ended_at = $4 WHERE campaign = $1 AND n = $2`, cmp.ID, n, reason, now); err != nil {
			return err
		}
		return c.afterItem(ctx, tx, now, cat, cmp, n, "failed")
	}
	_, err = tx.Exec(ctx, `UPDATE campaign_items SET task = $3 WHERE campaign = $1 AND n = $2`, cmp.ID, n, res.TaskID)
	return err
}

// campaignItemEnded closes the released item of a task that reached a terminal state, in the
// transaction of that state: ok for completed with the summary_fields of the result, unknown
// for an action whose write call has no outcome, failed or canceled otherwise (ORC-4, ORC-9).
func (c *Core) campaignItemEnded(ctx context.Context, tx pgx.Tx, now time.Time, task, state, reason string) error {
	var id, tenant string
	var n int
	err := tx.QueryRow(ctx, `SELECT i.campaign, c.tenant, i.n FROM campaign_items i JOIN campaigns c ON c.id = i.campaign WHERE i.task = $1 AND i.state = 'released'`, task).Scan(&id, &tenant, &n)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	cmp, err := loadCampaign(ctx, tx, tenant, id, true)
	if err != nil {
		return err
	}
	itemState := map[string]string{"completed": "ok", "failed": "failed", "canceled": "canceled"}[state]
	if state == "failed" && reason == "exec_unknown" {
		itemState = "unknown"
	}
	if itemState == "" {
		return nil
	}
	var typ string
	var rec, result []byte
	if err := tx.QueryRow(ctx, `SELECT type, record, result FROM tasks WHERE id = $1`, task).Scan(&typ, &rec, &result); err != nil {
		return err
	}
	var tr struct {
		Untrusted bool   `json:"untrusted_seen"`
		Mark      string `json:"data_mark"`
	}
	_ = json.Unmarshal(rec, &tr)
	var summary any
	// A campaign whose content went (an erasure, RET-10) records no result fields any more.
	if itemState == "ok" && len(result) > 0 && !cmp.Purged {
		var res map[string]any
		if json.Unmarshal(result, &res) == nil {
			summary = summaryOf(cat.TaskTypes[typ], res)
		}
	}
	var sraw []byte
	if summary != nil {
		sraw = mustJSON(summary)
	}
	if _, err := tx.Exec(ctx, `UPDATE campaign_items SET state = $3, untrusted = $4, summary = $5, ended_at = $6 WHERE campaign = $1 AND n = $2`,
		id, n, itemState, tr.Untrusted, sraw, now); err != nil {
		return err
	}
	return c.afterItem(ctx, tx, now, cat, cmp, n, itemState)
}

// summaryOf copies the summary_fields of a result: only values of closed kinds, a number, a
// boolean or a short enum string, reach the summary table (ORC-9, INJ-1).
func summaryOf(tt manifest.TaskType, result map[string]any) map[string]any {
	if len(tt.SummaryFields) == 0 {
		return nil
	}
	props, _ := tt.ResultSchema["properties"].(map[string]any)
	out := map[string]any{}
	for _, f := range tt.SummaryFields {
		s, _ := props[f].(map[string]any)
		switch v := result[f].(type) {
		case float64, bool:
			out[f] = v
		case string:
			if _, closed := s["enum"]; closed && len(v) <= 120 {
				out[f] = v
			}
		}
	}
	return out
}

// afterItem applies the closed rules after an item ended: a canary failure or failures over the
// threshold stop the campaign, an unknown item pauses it under on_unknown: pause; then the next
// items are released (ORC-4).
func (c *Core) afterItem(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow, n int, itemState string) error {
	counts, err := campaignCount(ctx, tx, cmp.ID)
	if err != nil {
		return err
	}
	canary := min(cmp.Rec.Rollout.Canary, counts.total())
	live := cmp.State == "running" || cmp.State == "paused"
	switch {
	case !live:
	case (itemState == "failed" || itemState == "unknown" && cmp.Rec.Rollout.OnUnknown != "pause") && n <= canary && !cmp.Rec.GatePassed:
		if err := c.stopCampaign(ctx, tx, now, cat, cmp, "threshold_exceeded"); err != nil {
			return err
		}
	case counts.Failed > cmp.Rec.Rollout.threshold(counts.total()):
		if err := c.stopCampaign(ctx, tx, now, cat, cmp, "threshold_exceeded"); err != nil {
			return err
		}
	case itemState == "unknown" && cmp.Rec.Rollout.OnUnknown == "pause" && cmp.State == "running":
		if err := c.setCampaignState(ctx, tx, now, cmp, "paused", "unknown", ""); err != nil {
			return err
		}
	}
	if err := c.campaignCardUpdates(ctx, tx, now, cmp, false); err != nil {
		return err
	}
	return c.releaseItems(ctx, tx, now, cat, cmp)
}

// stopCampaign moves a campaign to stopping: unreleased items become skipped, released read
// items are canceled with a cancellation, released write calls are not recalled (ORC-4, ORC-11).
func (c *Core) stopCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow, reason string) error {
	if !slices.Contains([]string{"running", "paused"}, cmp.State) {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE campaign_items SET state = 'skipped', ended_at = $2 WHERE campaign = $1 AND state = 'pending'`, cmp.ID, now); err != nil {
		return err
	}
	if err := c.setCampaignState(ctx, tx, now, cmp, "stopping", "", reason); err != nil {
		return err
	}
	if reason != "threshold_exceeded" && reason != "deadline_exceeded" && cmp.Rec.kind() == "task" {
		var tasks []string
		rows, err := tx.Query(ctx, `SELECT i.task FROM campaign_items i JOIN tasks t ON t.id = i.task
			WHERE i.campaign = $1 AND i.state = 'released' AND t.state IN ('submitted', 'working') ORDER BY i.n`, cmp.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return err
			}
			tasks = append(tasks, t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := c.cancelTaskIDs(ctx, tx, now, cmp.Tenant, tasks, reason); err != nil {
			return err
		}
		// The canceled items may have ended the campaign already.
		fresh, err := loadCampaign(ctx, tx, cmp.Tenant, cmp.ID, false)
		if err != nil {
			return err
		}
		*cmp = *fresh
	}
	return c.releaseItems(ctx, tx, now, cat, cmp)
}

// finishCampaign ends a campaign whose items are all terminal: the rest of the reserve goes, the
// final summary is delivered and the continuation is enqueued once (ORC-4, ORC-8, ORC-10, CNT-1).
func (c *Core) finishCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow, n campaignCounters) error {
	// A stop that cancels the last items ends the campaign from inside: it ends once.
	var cur string
	if err := tx.QueryRow(ctx, `SELECT state FROM campaigns WHERE id = $1`, cmp.ID).Scan(&cur); err != nil {
		return err
	}
	if slices.Contains([]string{"completed", "failed", "canceled", "rejected"}, cur) {
		cmp.State = cur
		return nil
	}
	state := "completed"
	switch cmp.StopReason {
	case "threshold_exceeded", "deadline_exceeded":
		state = "failed"
	case "":
		if n.Failed > cmp.Rec.Rollout.threshold(n.total()) {
			state = "failed"
		}
	default:
		state = "canceled"
	}
	if _, err := tx.Exec(ctx, `DELETE FROM spend WHERE campaign = $1 AND unit = 'task' AND state = 'reserved'`, cmp.ID); err != nil {
		return err
	}
	if err := c.setCampaignState(ctx, tx, now, cmp, state, "", cmp.StopReason); err != nil {
		return err
	}
	if cmp.StopReason == "threshold_exceeded" {
		if err := c.appendEvent(ctx, tx, now, cmp.Tenant, Event{Type: "alarm.campaign.threshold", Subject: cmp.ID,
			Data: map[string]any{"campaign": cmp.ID, "failed": n.Failed}}); err != nil {
			return err
		}
	}
	// A campaign an erasure stopped is purged first: it sends no summary of its items and
	// enqueues no continuation for the erased person (RET-10, SEC-11).
	var purged bool
	if err := tx.QueryRow(ctx, `SELECT purged_at IS NOT NULL FROM campaigns WHERE id = $1`, cmp.ID).Scan(&purged); err != nil {
		return err
	}
	if tv := cat.Tenants[cmp.Tenant]; tv != nil && cmp.Initiator != "system" && cmp.Rec.ScriptRun == "" && !purged {
		if err := c.planCampaignCard(ctx, tx, now, tv, cmp, campaignSummaryKind, campaignProgressCard, cmp.Initiator); err != nil {
			return err
		}
	}
	if err := c.scriptCampaignEnded(ctx, tx, now, cmp, state, cmp.StopReason); err != nil {
		return err
	}
	if cmp.Rec.Then == nil || state == "canceled" && cmp.ParentTask != "" && taskCanceled(ctx, tx, cmp.ParentTask) {
		return nil
	}
	if purged {
		// The campaign's content is gone: the continuation that would read it is refused.
		return c.appendEvent(ctx, tx, now, cmp.Tenant, Event{Type: "continuation.refused", Subject: cmp.ID,
			Data: map[string]any{"campaign": cmp.ID, "trigger": "campaign", "type": cmp.Rec.Then.Type, "reason": "gone"}})
	}
	return c.campaignContinuation(ctx, tx, now, cat, cmp, state, n)
}

func taskCanceled(ctx context.Context, tx pgx.Tx, id string) bool {
	var state string
	_ = tx.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, id).Scan(&state)
	return state == "canceled"
}

// campaignContinuation enqueues the then continuation once with the key
// system:cont:campaign:<id>: the summary table as an attachment, untrusted_seen as the OR over
// the campaign and all items (CNT-1, ORC-9, INJ-13).
func (c *Core) campaignContinuation(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, cmp *campaignRow, state string, n campaignCounters) error {
	table, err := campaignTable(ctx, tx, cmp.ID)
	if err != nil {
		return err
	}
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM campaign_items WHERE campaign = $1 AND untrusted)`, cmp.ID).Scan(&untrusted); err != nil {
		return err
	}
	// The continuation reads what the items read: their highest mark and every initiator of
	// what they read (DLV-7, MSG-7).
	mark, co := cmp.Rec.DataMark, slices.Clone(cmp.Rec.CoInitiators)
	irows, err := tx.Query(ctx, `SELECT coalesce(t.record->>'data_mark', ''), coalesce(t.record->'co_initiators', '[]') FROM campaign_items i JOIN tasks t ON t.id = i.task
		WHERE i.campaign = $1 ORDER BY i.n`, cmp.ID)
	if err != nil {
		return err
	}
	for irows.Next() {
		var m string
		var raw []byte
		if err := irows.Scan(&m, &raw); err != nil {
			irows.Close()
			return err
		}
		if m != "" {
			mark = c.higherMark(mark, m)
		}
		var list []string
		_ = json.Unmarshal(raw, &list)
		co = unionOf(co, list)
	}
	irows.Close()
	if err := irows.Err(); err != nil {
		return err
	}
	if len(table) > maxResultFields {
		table = table[:maxResultFields]
	}
	data := map[string]any{"campaign": cmp.ID, "state": state, "counters": n.asMap(), "items": table}
	body := map[string]any{"type": cmp.Rec.Then.Type, "goal": "Continue with the outcome of the campaign.", "attachments": []any{map[string]any{"data": data}}}
	var prio int
	if cmp.ParentTask != "" {
		prio = priorityOf(ctx, tx, cmp.ParentTask)
	}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{
		Tenant: cmp.Tenant, Type: cmp.Rec.Then.Type, Body: body, IdempotencyKey: "system:cont:campaign:" + cmp.ID, Initiator: cmp.Initiator,
		EnqueuedBy: "core", Origin: cmp.Rec.Origin, Source: "campaign", SourceID: cmp.ID, Cause: cmp.ID,
		Chain: &chainLink{
			Parent: cmp.ParentTask, Root: cmp.Rec.Root, Depth: cmp.Rec.Depth + 2, Mark: mark, MaxTier: cmp.Rec.MaxTier,
			Untrusted: cmp.Rec.Untrusted.Value || untrusted, Conversation: cmp.Rec.Conversation, Trace: cmp.Rec.Trace, ParentPriority: prio,
			CoInitiators: co, WakeTask: cmp.Rec.WakeTask,
		},
		// A campaign a card paid for pays its continuation the same way (ORC-8).
		ContinuesCampaign: cmp.ID, PaidByCard: cmp.Rec.PaidByCard, TreeBudgetExtra: money.FromEUR(cmp.Rec.Reserve.EUR),
	})
	reason := ""
	switch {
	case err != nil:
		p, ok := AsProblem(err)
		if !ok {
			return err
		}
		reason = p.Reason
	case res.Rejected != nil:
		reason = res.Rejected.Reason
	}
	if reason != "" {
		return c.appendEvent(ctx, tx, now, cmp.Tenant, Event{Type: "continuation.refused", Subject: cmp.ID,
			Data: map[string]any{"campaign": cmp.ID, "trigger": "campaign", "type": cmp.Rec.Then.Type, "reason": reason}})
	}
	return nil
}

// purgeCampaigns strips the content of the selected campaigns (RET-10): the goal, parameters and
// arguments of the template, and the arguments and result fields of every item that is not
// pending; the state, rollout, links and argument hashes stay as metadata, and the campaign is
// marked purged. It is idempotent: a campaign marked before reaches its items again. It counts
// only the campaigns it newly marked. head is empty or a WITH clause the selection reads.
func purgeCampaigns(ctx context.Context, tx pgx.Tx, head, where string, args ...any) (int64, error) {
	with := "WITH "
	if head != "" {
		with = head + ", "
	}
	var n int64
	err := tx.QueryRow(ctx, with+`o AS (SELECT id, purged_at IS NULL AS fresh FROM campaigns WHERE id IN (`+where+`)),
		k AS (UPDATE campaigns SET record = jsonb_set(record, '{template}', coalesce(record->'template', '{}') - 'goal' - 'params' - 'args'),
		purged_at = coalesce(purged_at, botaru_now()) WHERE id IN (SELECT id FROM o) RETURNING id),
		i AS (UPDATE campaign_items SET args = '{}', summary = NULL WHERE campaign IN (SELECT id FROM k) AND state <> 'pending' AND (args <> '{}' OR summary IS NOT NULL))
		SELECT count(*) FILTER (WHERE fresh) FROM o`, args...).Scan(&n)
	return n, err
}

// cancelCampaignsOf stops the live campaigns of a task that was canceled: a canceled tree has no
// campaign either (spec/lifecycles/campaign.md).
func (c *Core) cancelCampaignsOf(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM campaigns WHERE tenant = $1 AND parent_task = $2 AND state = ANY($3) ORDER BY id`, tenant, task, campaignLive)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	return c.endCampaigns(ctx, tx, now, tenant, list, "initiator")
}

// endCampaigns rejects the proposed and stops the running or paused campaigns of the list.
func (c *Core) endCampaigns(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, list []string, reason string) error {
	if len(list) == 0 {
		return nil
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	for _, id := range list {
		cmp, err := loadCampaign(ctx, tx, tenant, id, true)
		if err != nil {
			return err
		}
		if cmp.State == "proposed" {
			if err := c.rejectCampaign(ctx, tx, now, cmp, reason); err != nil {
				return err
			}
			continue
		}
		if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
			return err
		}
	}
	return nil
}

// rejectCampaign ends a proposed campaign whose card was rejected, expired or canceled: the
// reserve goes and no item was released.
func (c *Core) rejectCampaign(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow, reason string) error {
	if cmp.State != "proposed" {
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM spend WHERE campaign = $1 AND unit = 'task' AND state = 'reserved'`, cmp.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE campaign_items SET state = 'skipped', ended_at = $2 WHERE campaign = $1 AND state = 'pending'`, cmp.ID, now); err != nil {
		return err
	}
	from := cmp.State
	if _, err := tx.Exec(ctx, `UPDATE campaigns SET state = 'rejected', stop_reason = NULL, pause_reason = NULL, updated_at = $2 WHERE id = $1`, cmp.ID, now); err != nil {
		return err
	}
	cmp.State = "rejected"
	if err := c.campaignEvent(ctx, tx, now, cmp, "campaign.rejected.v1", from, reason); err != nil {
		return err
	}
	if err := c.campaignCardUpdates(ctx, tx, now, cmp, true); err != nil {
		return err
	}
	return c.scriptCampaignEnded(ctx, tx, now, cmp, "rejected", reason)
}

// pauseHeldCampaigns pauses the running campaigns of a tenant that a hold now covers, in the
// transaction of the hold, and rejects the proposed action campaigns in its scope (ORC-11,
// HLD-3).
func (c *Core) pauseHeldCampaigns(ctx context.Context, tx pgx.Tx, now time.Time, tenant string) error {
	q := `SELECT ` + campaignColumns + ` FROM campaigns WHERE state IN ('proposed', 'running') ORDER BY id FOR UPDATE`
	args := []any{}
	if tenant != "" {
		q = `SELECT ` + campaignColumns + ` FROM campaigns WHERE tenant = $1 AND state IN ('proposed', 'running') ORDER BY id FOR UPDATE`
		args = append(args, tenant)
	}
	rows, err := tx.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	var list []*campaignRow
	for rows.Next() {
		cmp, err := scanCampaign(rows)
		if err != nil {
			rows.Close()
			return err
		}
		list = append(list, cmp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, cmp := range list {
		if cmp.State == "proposed" {
			if cmp.Rec.action() == "" {
				continue
			}
			held, err := heldBy(ctx, tx, now, []string{"no_action"}, campaignScopes(cmp)...)
			if err != nil {
				return err
			}
			if held != "" {
				if err := c.rejectCampaign(ctx, tx, now, cmp, "held"); err != nil {
					return err
				}
			}
			continue
		}
		held, err := heldBy(ctx, tx, now, campaignHoldEffectsOf(cmp), campaignScopes(cmp)...)
		if err != nil {
			return err
		}
		if held != "" {
			if err := c.setCampaignState(ctx, tx, now, cmp, "paused", "held", ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// campaignLoop is the background loop campaign: holds, card expiry, deadlines, the bake after
// the canary and the items that waited for a busy resource or a long queue (ORC-4, ORC-8, ORC-11).
func (c *Core) campaignLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.pauseHeldCampaigns(ctx, tx, now, "") }); err != nil {
		return err
	}
	var list []struct{ id, tenant string }
	if err := c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT id, tenant FROM campaigns WHERE state = ANY($1) ORDER BY created_at, id`, campaignLive)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var x struct{ id, tenant string }
			if err := rows.Scan(&x.id, &x.tenant); err != nil {
				return err
			}
			list = append(list, x)
		}
		return rows.Err()
	}); err != nil {
		return err
	}
	for _, x := range list {
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			cmp, err := loadCampaign(ctx, tx, x.tenant, x.id, true)
			if err != nil {
				return err
			}
			switch {
			case cmp.State == "proposed" && cmp.ExpiresAt != nil && !cmp.ExpiresAt.After(now):
				return c.rejectCampaign(ctx, tx, now, cmp, "expired")
			case (cmp.State == "running" || cmp.State == "paused") && cmp.DeadlineAt != nil && !cmp.DeadlineAt.After(now):
				return c.stopCampaign(ctx, tx, now, cat, cmp, "deadline_exceeded")
			case cmp.State == "running" || cmp.State == "stopping":
				if cmp.State == "running" {
					if stuck, err := noExecutor(ctx, tx, now, cmp); err != nil {
						return err
					} else if stuck {
						return c.setCampaignState(ctx, tx, now, cmp, "paused", "no_executor", "")
					}
				}
				if err := c.releaseItems(ctx, tx, now, cat, cmp); err != nil {
					return err
				}
				return c.campaignCardUpdates(ctx, tx, now, cmp, false)
			}
			return nil
		}); err != nil {
			// One campaign that fails its pass does not stop the others.
			if ctx.Err() != nil {
				return err
			}
			c.log.Error("a campaign pass failed", "tenant", x.tenant, "campaign", x.id, "error", err)
		}
	}
	return nil
}
