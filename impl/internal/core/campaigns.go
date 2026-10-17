// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Campaigns (spec/lifecycles/campaign.md, docs/coordination.md). Only a live attempt of a manager
// proposes one, through P3 propose_campaign (ORC-1); the core freezes the plan, checks it against
// the ceilings, the rights of every initiator, the distrust ceiling, the tree size and the whole
// reserve in one transaction (ORC-5, ORC-6, ORC-8, ORC-14), and from then on only core code moves
// it: it releases the items, each at most once, by the rollout policy, and stops on the
// threshold (ORC-3, ORC-4). A campaign with an action, or a read campaign above the tenant's
// threshold, starts only after one person approves its card (ORC-2).
// This file holds the constants and the records of campaigns with their stored rows; the
// tools, proposal, runs, cards and decisions are in the files campaign*.go.

const (
	proposeCampaignName = "propose_campaign"
	campaignStatusName  = "campaign_status"
	campaignResultsName = "campaign_results"
	// Item ceilings: the farm, the tenant default, distrust (ORC-5, ORC-14).
	farmMaxCampaignItems     = 500
	defaultCampaignItems     = 100
	defaultUntrustedItems    = 3
	untrustedReadItems       = 10
	defaultConfirmReadItems  = 50
	defaultConfirmReadEUR    = 2.0
	campaignProgressEvery    = 30 * time.Second
	campaignResultsPage      = 20
	campaignApprovalCard     = "campaign_approval"
	campaignProgressCard     = "campaign_progress"
	campaignSummaryKind      = "campaign_summary"
	campaignListKind         = "campaign_list"
	defaultCampaignCardTTL   = time.Hour
	maxCampaignResultsChars  = 2000
	campaignItemFirstOnCard  = 5
	campaignThresholdCeiling = 500
)

// campaignLive are the states in which a campaign still holds its reserve.
var campaignLive = []string{"proposed", "running", "paused", "stopping"}

// campaignRollout is the rollout policy as the core fixed it: every field the proposal left out
// has its value from the action's ceilings (ORC-5).
type campaignRollout struct {
	Canary      int    `json:"canary"`
	AfterCanary string `json:"after_canary"`
	Bake        string `json:"bake,omitempty"`
	MaxInFlight int    `json:"max_in_flight"`
	MaxFailures any    `json:"max_failures"`
	OnUnknown   string `json:"on_unknown,omitempty"`
	Deadline    string `json:"deadline,omitempty"`
}

// threshold is the number of failed items the campaign tolerates: a number, or a share of n.
func (r campaignRollout) threshold(n int) int {
	switch v := r.MaxFailures.(type) {
	case float64:
		return int(v)
	case int:
		return v
	case string:
		p, err := strconv.Atoi(strings.TrimSuffix(v, "%"))
		if err == nil {
			return n * p / 100
		}
	}
	return 0
}

// code writes the rollout order in words for a card (ORC-2).
func (r campaignRollout) code(n int) string {
	s := fmt.Sprintf("canary %d, then %s, %d at once, stop after %d failed", min(r.Canary, n), r.AfterCanary, r.MaxInFlight, r.threshold(n))
	if r.Deadline != "" {
		s += ", deadline " + r.Deadline
	}
	return s
}

type campaignThen struct {
	Type string `json:"type"`
}

type campaignUntrusted struct {
	Value  bool    `json:"value"`
	Source *string `json:"source"`
}

type campaignReserve struct {
	TreeTasks int     `json:"tree_tasks"`
	EUR       float64 `json:"eur"`
}

// campaignRec is the frozen record of a campaign: what campaign.schema.json names, and the parts
// of the proposing tree the core needs to release items.
type campaignRec struct {
	Template   map[string]any    `json:"template"`
	Rollout    campaignRollout   `json:"rollout"`
	Then       *campaignThen     `json:"then"`
	ToGroup    string            `json:"to_group,omitempty"`
	Refs       string            `json:"refs,omitempty"`
	MaxTier    int               `json:"max_tier"`
	DataMark   string            `json:"data_mark"`
	Untrusted  campaignUntrusted `json:"untrusted_seen"`
	Reserve    campaignReserve   `json:"reserve"`
	EnqueuedBy map[string]any    `json:"enqueued_by,omitempty"`
	// The tree of the proposing task: items are its children and the continuation follows them.
	Root         string   `json:"root"`
	Depth        int      `json:"depth"`
	Conversation string   `json:"conversation,omitempty"`
	Trace        string   `json:"trace,omitempty"`
	Origin       *Origin  `json:"origin,omitempty"`
	CoInitiators []string `json:"co_initiators,omitempty"`
	// WakeTask is the wake-up whose subtree the campaign is in (MSG-11).
	WakeTask string `json:"wake_task,omitempty"`
	// ItemMicros is the estimate of one item at the proposal, Line the budget line; a card pays
	// the campaign from the tenant's month instead of the person's day (ORC-8, BUD-12).
	ItemMicros   int64  `json:"item_micros"`
	Line         string `json:"line"`
	PaidByCard   bool   `json:"paid_by_card,omitempty"`
	OverDayLimit bool   `json:"over_day_limit,omitempty"`
	GatePassed   bool   `json:"gate_passed,omitempty"`
	Role         string `json:"role,omitempty"`
	ScriptRun    string `json:"script_run,omitempty"`
	ScriptStep   string `json:"script_step,omitempty"`
	// ExecutorFrom is the resume of a no_executor pause: a released item gets its start_by
	// window again from then (ORC-4).
	ExecutorFrom *time.Time `json:"executor_from,omitempty"`
	// RejectedBy is the reason of a P2 cancel that rejected the proposed campaign, so a repeat
	// answers the same (IDM-3).
	RejectedBy string `json:"rejected_by,omitempty"`
}

func (r campaignRec) kind() string {
	k, _ := r.Template["kind"].(string)
	return k
}

func (r campaignRec) action() string {
	a, _ := r.Template["action"].(string)
	return a
}

// itemType is the task type of an item: the template type, or action.<a>.
func (r campaignRec) itemType() string {
	if r.kind() == "action" {
		return "action." + r.action()
	}
	t, _ := r.Template["type"].(string)
	return t
}

// campaignRow is a campaign as the core reads it.
type campaignRow struct {
	ID, Tenant, ParentTask, Initiator, State, PauseReason, StopReason, PlanHash, Nonce, DecidedBy string
	Card, Purged                                                                                  bool
	Rec                                                                                           campaignRec
	ExpiresAt, DeadlineAt, GateAt, ProgressAt                                                     *time.Time
	CreatedAt                                                                                     time.Time
}

const campaignColumns = `id, tenant, coalesce(parent_task, ''), initiator, state, coalesce(pause_reason, ''), coalesce(stop_reason, ''), plan_hash,
	nonce, coalesce(decided_by, ''), card, record, expires_at, deadline_at, gate_at, progress_at, created_at, purged_at IS NOT NULL`

func scanCampaign(row pgx.Row) (*campaignRow, error) {
	var c campaignRow
	var rec []byte
	if err := row.Scan(&c.ID, &c.Tenant, &c.ParentTask, &c.Initiator, &c.State, &c.PauseReason, &c.StopReason, &c.PlanHash,
		&c.Nonce, &c.DecidedBy, &c.Card, &rec, &c.ExpiresAt, &c.DeadlineAt, &c.GateAt, &c.ProgressAt, &c.CreatedAt, &c.Purged); err != nil {
		return nil, err
	}
	return &c, json.Unmarshal(rec, &c.Rec)
}

func loadCampaign(ctx context.Context, q store.Querier, tenant, id string, lock bool) (*campaignRow, error) {
	sql := `SELECT ` + campaignColumns + ` FROM campaigns WHERE id = $1 AND tenant = $2`
	if lock {
		sql += ` FOR UPDATE`
	}
	return scanCampaign(q.QueryRow(ctx, sql, id, tenant))
}

// campaignCounters are the item counts by state; they always equal the items (ORC-4).
type campaignCounters struct {
	Pending  int `json:"pending"`
	Released int `json:"released"`
	OK       int `json:"ok"`
	Failed   int `json:"failed"`
	Unknown  int `json:"unknown"`
	Skipped  int `json:"skipped"`
	Canceled int `json:"canceled"`
}

func (n campaignCounters) total() int {
	return n.Pending + n.Released + n.OK + n.Failed + n.Unknown + n.Skipped + n.Canceled
}

func (n campaignCounters) asMap() map[string]any {
	return map[string]any{"pending": n.Pending, "released": n.Released, "ok": n.OK, "failed": n.Failed, "unknown": n.Unknown, "skipped": n.Skipped, "canceled": n.Canceled}
}

func campaignCount(ctx context.Context, q store.Querier, id string) (campaignCounters, error) {
	var n campaignCounters
	err := q.QueryRow(ctx, `SELECT count(*) FILTER (WHERE state = 'pending'), count(*) FILTER (WHERE state = 'released'), count(*) FILTER (WHERE state = 'ok'),
		count(*) FILTER (WHERE state = 'failed'), count(*) FILTER (WHERE state = 'unknown'), count(*) FILTER (WHERE state = 'skipped'),
		count(*) FILTER (WHERE state = 'canceled') FROM campaign_items WHERE campaign = $1`, id).
		Scan(&n.Pending, &n.Released, &n.OK, &n.Failed, &n.Unknown, &n.Skipped, &n.Canceled)
	return n, err
}

// Record is the campaign as spec/model/campaign.schema.json describes it, without free text.
func (c *campaignRow) Record(ctx context.Context, q store.Querier) (map[string]any, error) {
	n, err := campaignCount(ctx, q, c.ID)
	if err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT n, resource, args_hash FROM campaign_items WHERE campaign = $1 ORDER BY n`, c.ID)
	if err != nil {
		return nil, err
	}
	items := []any{}
	for rows.Next() {
		var i int
		var res, hash string
		if err := rows.Scan(&i, &res, &hash); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, map[string]any{"n": i, "resource": res, "args_hash": hash})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reserve := map[string]any{"tree_tasks": c.Rec.Reserve.TreeTasks, "eur": c.Rec.Reserve.EUR}
	rollout := map[string]any{"canary": c.Rec.Rollout.Canary, "after_canary": c.Rec.Rollout.AfterCanary, "max_in_flight": c.Rec.Rollout.MaxInFlight,
		"max_failures": c.Rec.Rollout.MaxFailures}
	for k, v := range map[string]string{"bake": c.Rec.Rollout.Bake, "on_unknown": c.Rec.Rollout.OnUnknown, "deadline": c.Rec.Rollout.Deadline} {
		if v != "" {
			rollout[k] = v
		}
	}
	r := map[string]any{
		"id": c.ID, "tenant": c.Tenant, "initiator": map[string]any{"principal": c.Initiator}, "template": c.Rec.Template, "items": items,
		"plan_hash": c.PlanHash, "rollout": rollout, "max_tier": c.Rec.MaxTier, "data_mark": c.Rec.DataMark,
		"untrusted_seen": map[string]any{"value": c.Rec.Untrusted.Value, "source": c.Rec.Untrusted.Source}, "reserve": reserve,
		"state": c.State, "counters": n.asMap(), "then": nil,
	}
	if c.Rec.Then != nil {
		r["then"] = map[string]any{"type": c.Rec.Then.Type}
	}
	if c.ParentTask != "" {
		r["parent_task"] = c.ParentTask
	}
	if len(c.Rec.EnqueuedBy) > 0 {
		r["enqueued_by"] = c.Rec.EnqueuedBy
	}
	if c.Rec.ToGroup != "" {
		r["to_group"] = c.Rec.ToGroup
	}
	if c.Rec.Refs != "" {
		r["refs"] = c.Rec.Refs
	}
	if c.Card {
		r["approval_id"] = c.ID
	}
	if c.PauseReason != "" {
		r["pause_reason"] = c.PauseReason
	}
	if c.StopReason != "" {
		r["stop_reason"] = c.StopReason
	}
	return r, nil
}

// campaignEvent writes campaign.<event>.v1 with metadata only: the states, the reason and the
// counters (AUD-2).
func (c *Core) campaignEvent(ctx context.Context, tx pgx.Tx, now time.Time, cmp *campaignRow, event, from, reason string) error {
	n, err := campaignCount(ctx, tx, cmp.ID)
	if err != nil {
		return err
	}
	data := map[string]any{"event": event, "campaign": cmp.ID, "to": cmp.State, "counters": n.asMap()}
	if from != "" {
		data["from"] = from
	} else {
		data["from"] = nil
	}
	if reason != "" {
		data["reason"] = reason
	}
	return c.appendEvent(ctx, tx, now, cmp.Tenant, Event{Type: event, Subject: cmp.ID, Data: data, Causation: cmp.ParentTask})
}

// campaignPlan is a proposal the core has turned into items: who asks, from which tree, the
// template, the resources with the arguments of each item, the policy and the continuation.
type campaignPlan struct {
	Tenant, Initiator, ParentTask, Role string
	Template                            map[string]any
	Kind, Type, Action                  string
	Resources                           []string
	ItemArgs                            []map[string]any
	Selector                            bool
	Rollout                             campaignRollout
	Then                                *campaignThen
	ToGroup, Refs                       string
	Untrusted                           bool
	UntrustedSource                     string
	Mark                                string
	MaxTier                             int
	EnqueuedBy                          map[string]any
	Root                                string
	Depth                               int
	Conversation, Trace                 string
	Origin                              *Origin
	CoInitiators                        []string
	WakeTask                            string
	Key                                 string
	// ForceCard makes the core ask a person even for a read campaign: a retry.
	ForceCard bool
	// Source is how the card names who asked: bot or human.
	Source string
	// ScriptRun and ScriptStep make the campaign a fanout step of a script run: its list was
	// frozen and its tree and estimate reserved at the run approval, it has no card of its own
	// unless it has an action, and no progress card or summary (SCR-6, SCR-8, ORC-10).
	ScriptRun, ScriptStep string
}

// campaignOutcome is the answer of a proposal: the campaign and its state, or a refusal code.
type campaignOutcome struct {
	Campaign string `json:"campaign,omitempty"`
	State    string `json:"state"`
	Reason   string `json:"reason,omitempty"`
}

func (o campaignOutcome) call() *mcp.CallResult {
	b, _ := json.Marshal(o)
	var sc map[string]any
	_ = json.Unmarshal(b, &sc)
	return &mcp.CallResult{IsError: o.State == "rejected", StructuredContent: sc, Content: []mcp.Content{{Type: "text", Text: string(b)}}}
}
