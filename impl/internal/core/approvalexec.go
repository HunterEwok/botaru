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
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The execution of an approved action: its action task and its one write call (APR-5, APR-7, CNT-7).

// executeApproval moves an approved approval to executing: the action.<a> task directly in
// working and its one write call from the snapshot (APR-5, APR-7, CNT-7). Before that the core
// checks again that the initiator is active with action.request:<a> and that the arguments
// still pass the catalog (APR-6, MAN-10).
func (c *Core) executeApproval(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, a *approvalRow) error {
	act, ok := c.actionOf(cat, a.Action)
	if !ok {
		return c.closeApproval(ctx, tx, now, a, "canceled", "ref_invalid")
	}
	// Every initiator but the literal system is a principal row: a person, a service or a
	// system principal loses the release with its state or its right (APR-6, APR-18).
	if reason, err := c.initiatorLost(ctx, tx, now, tv, a); err != nil {
		return err
	} else if reason != "" {
		return c.closeApproval(ctx, tx, now, a, "canceled", reason)
	}
	// A service action from a bot needs the role's permission at the decision too (APR-18),
	// and every service action passes the guards of MON-8 again: the verb is still allowed,
	// one operation per service, the rate limits and the holds.
	if a.Action == serviceAction {
		role, _ := a.RequestedBy["role"].(string)
		name, _ := a.Args["service"].(string)
		verb, _ := a.Args["verb"].(string)
		if role != "" && !roleControls(cat, role, name) {
			return c.closeApproval(ctx, tx, now, a, "canceled", "rights_revoked")
		}
		svc, ok := findControllable(cat, a.Tenant, name)
		if !ok || !slices.Contains(svc.Control, verb) {
			return c.closeApproval(ctx, tx, now, a, "canceled", "ref_invalid")
		}
		task, _ := a.RequestedBy["task"].(string)
		var typ string
		if task != "" {
			_ = tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, task).Scan(&typ)
		}
		// A seat task's approved action runs only within the seat's ops_per_hour at execution,
		// under the same locks, seat first, as its other operations (MGR-14).
		if a.SourceTask != "" {
			var rec []byte
			if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, a.SourceTask).Scan(&rec); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			if _, limit, err := seatToolGate(ctx, tx, now, cat, rec); err != nil {
				return err
			} else if limit != "" {
				return c.closeApproval(ctx, tx, now, a, "exec_failed", limit)
			}
		}
		code, err := c.serviceGuards(ctx, tx, now, serviceCall{Tenant: a.Tenant, Service: svc, Verb: verb, Initiator: a.Initiator, Role: role, Task: task, Type: typ})
		if err != nil {
			return err
		}
		switch code {
		case "":
		case "held":
			return c.closeApproval(ctx, tx, now, a, "canceled", "held")
		default:
			return c.closeApproval(ctx, tx, now, a, "exec_failed", code)
		}
	}
	mark := a.Mark
	if mark == "" && a.SourceTask != "" {
		var rec []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, a.SourceTask).Scan(&rec); err == nil {
			var r struct {
				DataMark string `json:"data_mark"`
			}
			_ = json.Unmarshal(rec, &r)
			mark = r.DataMark
		}
	}
	schema, err := c.actionArgsSchema(cat, a.Tenant, mark, act)
	if err != nil {
		return err
	}
	if c.rs.validate(schema, a.Args) != nil {
		return c.closeApproval(ctx, tx, now, a, "canceled", "ref_invalid")
	}
	// A busy resource of an exclusive action makes the approval wait, not fail (ORC-12).
	if wait, err := c.approvalWaits(ctx, tx, now, cat, a, act); err != nil || wait {
		return err
	}
	taskID, err := c.insertActionTask(ctx, tx, now, actionTaskInput{
		Tenant: a.Tenant, Action: a.Action, Args: a.Args, Initiator: a.Initiator, SourceTask: a.SourceTask, Mark: mark, ApprovalID: a.ID,
	})
	if err != nil {
		return err
	}
	callID, err := c.insertWriteCall(ctx, tx, now, cat, writeCall{
		Tenant: a.Tenant, Task: taskID, Action: a.Action, Act: act, Args: a.Args, ArgsHash: a.ArgsHash, ApprovalID: a.ID,
	})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = 'executing', reason = NULL, action_task = $2, toolcall = $3, updated_at = $4 WHERE id = $1 AND state IN ('pending', 'approved')`,
		a.ID, taskID, callID, now); err != nil {
		return err
	}
	a.State, a.ActionTask, a.Toolcall = "executing", taskID, callID
	if err := c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: "approval.executing", Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "task": taskID}}); err != nil {
		return err
	}
	if err := c.planCardUpdates(ctx, tx, now, a); err != nil {
		return err
	}
	if err := c.scriptApprovalMoved(ctx, tx, now, a); err != nil {
		return err
	}
	// A call that ended at once (no toolhost, a hold) closes the task and the approval now.
	var state, reason string
	if err := tx.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM toolcalls WHERE id = $1`, callID).Scan(&state, &reason); err != nil {
		return err
	}
	if state != "queued" {
		if err := c.writeCallEnded(ctx, tx, now, callID, state, reason, nil); err != nil {
			return err
		}
		a.State = map[string]string{"done": "executed", "exec_unknown": "exec_unknown"}[state]
		if a.State == "" {
			a.State = "exec_failed"
		}
	}
	return nil
}

// actionTaskInput is the action.<a> task of an approval or a grant.
type actionTaskInput struct {
	Tenant, Action, Initiator, SourceTask, Mark string
	Args                                        map[string]any
	ApprovalID, GrantID                         string
	// Origin is the chat of a person's command, which gets the outcome as code text; Key is
	// then the key of the activity, so a repeat of the same update finds the task (DLV-5).
	Origin *Origin
	Key    string
	// Campaign, CampaignItem and Resource make the task an item of an action campaign (ORC-3).
	Campaign     string
	CampaignItem int
	Resource     string
}

// insertActionTask creates the action.<a> task directly in working, without an attempt and a
// lease, with the key system:approval:<id> or system:grant:<id> (CNT-7). It has no deadline of
// its own: the card's deadline and the call's timeout hold it.
func (c *Core) insertActionTask(ctx context.Context, tx pgx.Tx, now time.Time, in actionTaskInput) (string, error) {
	id := ids.New(ids.Task)
	typ := "action." + in.Action
	body := map[string]any{"type": typ, "params": in.Args}
	bodyHash, err := jcs.Hash(body)
	if err != nil {
		return "", err
	}
	root, fairness := id, "action:"+in.Action
	if in.SourceTask != "" {
		var rec []byte
		if err := tx.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, in.SourceTask).Scan(&rec); err == nil {
			var r struct {
				Root     string `json:"root"`
				Fairness string `json:"fairness_key"`
			}
			_ = json.Unmarshal(rec, &r)
			if r.Root != "" {
				root = r.Root
			}
			if r.Fairness != "" {
				fairness = r.Fairness
			}
		}
	}
	record := map[string]any{
		"tenant": in.Tenant, "enqueued_by": "core", "depth": 0, "root": root, "correlationid": root, "max_tier": 3,
		"untrusted_seen": false, "fairness_key": fairness, "budget_line": "work", "purpose": "work",
	}
	key, cause := "system:approval:"+in.ApprovalID, in.ApprovalID
	if in.ApprovalID != "" {
		record["approval_id"] = in.ApprovalID
	} else {
		record["grant_id"] = in.GrantID
		key, cause = "system:grant:"+in.GrantID, in.GrantID
	}
	if in.Key != "" {
		key = in.Key
	}
	if in.Mark != "" {
		record["data_mark"] = in.Mark
	}
	if in.Origin != nil {
		record["origin"] = in.Origin
	}
	if in.Campaign != "" {
		record["campaign"], record["campaign_item"], record["fairness_key"] = in.Campaign, in.CampaignItem, "campaign:"+in.Campaign
		record["scope"] = map[string]any{"resources": []any{in.Resource}}
	}
	rec, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, created_at, updated_at)
		VALUES ($1, $2, $3, 'working', $4, 2, $5, $6, $7, $8, $9, $9)`,
		id, in.Tenant, typ, in.Initiator, key, bodyHash, raw, rec, now); err != nil {
		return "", err
	}
	if err := c.appendEvent(ctx, tx, now, in.Tenant, Event{Type: "task.submitted", Subject: id, Data: map[string]any{"task": id, "type": typ}, Causation: cause}); err != nil {
		return "", err
	}
	return id, nil
}

// writeCall is a write call of an action.
type writeCall struct {
	Tenant, Task, Action string
	Act                  manifest.Action
	Args                 map[string]any
	ArgsHash             string
	ApprovalID, GrantID  string
	// CampaignItem is the item number of a campaign call: the plugin key is <campaign>:<n>
	// (ORC-3). Host is the machine of the item for a tool with target: any.
	CampaignItem int
	Host         string
}

// insertWriteCall queues the one write call of an action task; a call that a hold or the lack
// of a toolhost stops ends at once, before it is handed out (APR-7, DLV-16).
func (c *Core) insertWriteCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, wc writeCall) (string, error) {
	plugin, toolName := wc.Act.Executor.Plugin, wc.Act.Executor.Tool
	right := toolspec.RightName(plugin, toolName)
	pl := cat.Plugins[plugin]
	t, ok := pl.Tool(toolName)
	id := ids.New(ids.Call)
	row := toolCallRow{
		ID: id, Tenant: wc.Tenant, Task: wc.Task, Tool: right, Access: "write", Args: wc.Args, Plugin: plugin, Placement: pl.Placement,
		Untrusted: !ok || !t.TrustedOutput(), State: "queued", ArgsHash: wc.ArgsHash, ApprovalID: wc.ApprovalID, GrantID: wc.GrantID,
		CampaignItem: wc.CampaignItem,
	}
	end := func(state, reason string) (string, error) {
		row.State, row.Reason = state, reason
		if err := c.insertToolCall(ctx, tx, now, row); err != nil {
			return "", err
		}
		return id, nil
	}
	if !ok || !t.On() || t.Access != "write" {
		return end("error", "plugin_unavailable")
	}
	row.DefHash = toolDefHash(pl.Placement, t)
	// The machine of the call comes from the action argument named in target_from, by the
	// catalog lists of the action schema (TEN-3a).
	params := map[string]map[string]any{}
	props, _ := wc.Act.Args["properties"].(map[string]any)
	for k, v := range props {
		if m, ok := v.(map[string]any); ok {
			params[k] = m
		}
	}
	target, resolved, err := c.writeTarget(cat, wc.Tenant, t, params, wc.Args)
	if err != nil {
		return end("error", "input_schema_invalid")
	}
	row.Host, row.Resource, row.Resolved = target.host, target.resource, resolved
	if t.Target == "any" && wc.Host != "" {
		target.host, row.Host = wc.Host, wc.Host
	}
	scopes := append(tenantScopes(wc.Tenant), scopeRef{kind: "plugin", tenant: wc.Tenant, name: plugin},
		scopeRef{kind: "action", tenant: wc.Tenant, name: wc.Action}, scopeRef{kind: "tasktype", tenant: wc.Tenant, name: "action." + wc.Action})
	scopes = append(scopes, targetScopes(wc.Tenant, target)...)
	if held, err := heldBy(ctx, tx, now, []string{"no_tool", "no_action"}, scopes...); err != nil {
		return "", err
	} else if held != "" {
		return end("denied", "held")
	}
	live, err := c.liveToolhost(ctx, tx, now, cat, wc.Tenant, plugin, pl.Placement, target.host)
	if err != nil {
		return "", err
	}
	if !live {
		return end("error", "plugin_unavailable")
	}
	row.StartBy = now.Add(maxToolStartBy)
	if err := c.insertToolCall(ctx, tx, now, row); err != nil {
		return "", err
	}
	return id, notifyWork(ctx, tx)
}

// writeTarget computes the machine of a write call: the argument of target_from may be one
// value or a list of values, and all of them must lie on one machine.
func (c *Core) writeTarget(cat *Catalog, tenant string, t toolspec.Tool, params map[string]map[string]any, args map[string]any) (callTargetRef, map[string]string, error) {
	if t.Target == "any" {
		return callTargetRef{}, map[string]string{}, nil
	}
	p := params[t.TargetFrom]
	if p == nil {
		return callTargetRef{}, nil, errors.New("no target parameter")
	}
	var values []string
	switch v := args[t.TargetFrom].(type) {
	case string:
		values = []string{v}
	case []any:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return callTargetRef{}, nil, errors.New("a target value is not a string")
			}
			values = append(values, s)
		}
		if items, ok := p["items"].(map[string]any); ok {
			p = items
		}
	}
	if len(values) == 0 {
		return callTargetRef{}, nil, errors.New("no target value")
	}
	tool := t
	tool.Params = map[string]map[string]any{t.TargetFrom: p}
	tool.Commands = nil
	var first callTargetRef
	resolved := map[string]string{}
	for i, v := range values {
		ref, res, err := c.callTargetAny(cat, tenant, tool, map[string]any{t.TargetFrom: v})
		if err != nil {
			return callTargetRef{}, nil, err
		}
		if i == 0 {
			first, resolved = ref, res
			continue
		}
		if ref.host != first.host {
			return callTargetRef{}, nil, errors.New("the target values lie on several machines")
		}
		if ref.resource != first.resource {
			first.resource = ""
		}
	}
	return first, resolved, nil
}

// writeCallEnded closes the action task and the approval of a write call that ended (CNT-7,
// APR-7): done completes them, exec_unknown leaves the outcome unknown, anything else is
// exec_failed.
func (c *Core) writeCallEnded(ctx context.Context, tx pgx.Tx, now time.Time, callID, state, reason string, output []byte) error {
	var task, tenant string
	var approval, grant *string
	err := tx.QueryRow(ctx, `SELECT task, tenant, approval_id, grant_id FROM toolcalls WHERE id = $1 AND access = 'write'`, callID).Scan(&task, &tenant, &approval, &grant)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	taskState, taskReason, apState := "completed", "", "executed"
	switch state {
	case "done":
	case "exec_unknown":
		taskState, taskReason, apState = "failed", "exec_unknown", "exec_unknown"
	default:
		taskState, taskReason, apState = "failed", "exec_failed", "exec_failed"
	}
	var result []byte
	if state == "done" {
		var o mcp.CallResult
		_ = json.Unmarshal(output, &o)
		sc := o.StructuredContent
		if sc == nil {
			sc = map[string]any{}
		}
		result = mustJSON(sc)
	}
	// Only an action task lives and ends with its write call (CNT-7); a grant call of a bot's
	// own task ends only the call.
	var rec []byte
	var typ, initiator string
	err = tx.QueryRow(ctx, `UPDATE tasks SET state = $2, reason = $3, result = $4, updated_at = $5 WHERE id = $1 AND state = 'working' AND type LIKE 'action.%'
		RETURNING record, type, initiator`, task, taskState, nullable(taskReason), result, now).Scan(&rec, &typ, &initiator)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if err == nil {
		data := map[string]any{"task": task}
		if taskReason != "" {
			data["reason"] = taskReason
		}
		if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "task." + taskState, Subject: task, Data: data}); err != nil {
			return err
		}
		if err := c.grantOutcomeNotice(ctx, tx, now, tenant, task, typ, initiator, rec, taskState, taskReason); err != nil {
			return err
		}
		if err := c.campaignItemEnded(ctx, tx, now, task, taskState, taskReason); err != nil {
			return err
		}
		if err := c.scriptTaskEnded(ctx, tx, now, task, taskState, taskReason); err != nil {
			return err
		}
	}
	// A campaign item has no approval row of its own: the campaign is its subject (ORC-2).
	if approval == nil || strings.HasPrefix(*approval, ids.Campaign+"_") {
		return nil
	}
	a, err := loadApproval(ctx, tx, tenant, *approval, true)
	if err != nil {
		return err
	}
	if a.State != "executing" {
		return nil
	}
	apReason := ""
	if apState != "executed" {
		apReason = reason
		if apReason == "" {
			apReason = apState
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE approvals SET state = $2, reason = $3, updated_at = $4 WHERE id = $1`, a.ID, apState, nullable(apReason), now); err != nil {
		return err
	}
	a.State, a.Reason = apState, apReason
	if err := c.appendEvent(ctx, tx, now, tenant, Event{Type: "approval." + apState, Subject: a.ID,
		Data: map[string]any{"approval": a.ID, "action": a.Action, "task": task, "reason": apReason}}); err != nil {
		return err
	}
	return c.planCardUpdates(ctx, tx, now, a)
}

// grantOutcomeNotice tells the person who gave /service or /run the outcome of the grant's
// action as code text in the chat of the command (PRM-18).
func (c *Core) grantOutcomeNotice(ctx context.Context, tx pgx.Tx, now time.Time, tenant, task, typ, initiator string, rec []byte, state, reason string) error {
	var r struct {
		Origin *Origin `json:"origin"`
	}
	if json.Unmarshal(rec, &r) != nil || r.Origin == nil {
		return nil
	}
	body := map[string]any{"form": "text_code", "text_key": "cmd.action.done", "params": map[string]any{"action": strings.TrimPrefix(typ, "action.")}}
	if state != "completed" {
		body = map[string]any{"form": "text_code", "text_key": "task.failed", "params": map[string]any{"reason": reason}}
	}
	_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Task: task, Kind: "task_result", Addr: "origin", TargetKind: "chat", TargetID: r.Origin.Chat.ID(),
		ExecKind: "channel", ExecName: r.Origin.Channel, Recipient: initiator, DataOrigin: "code", Body: body,
	})
	return err
}
