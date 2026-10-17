// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Observation is what a case left on the bench, collected after the run: the tasks of the case,
// its deliveries and tool calls, the side effect attempts from the audit (refused ones too), the
// messages the fake Bot API received and the model request bodies P4 sent.
type Observation struct {
	// Self is the task of the case's input, nil when the input made none.
	Self       *TaskRow
	Tasks      []*TaskRow
	Deliveries []map[string]any
	ToolCalls  []map[string]any
	// Audit are the audit records of the case's tasks.
	Audit []map[string]any
	// Sent are the sendMessage requests of the case.
	Sent []map[string]any
	// ModelCalls are the request bodies and answer model names of the case's P4 calls.
	ModelCalls []modelCall
	// Unscripted counts mock model calls of a case without a script.
	Unscripted int
	// NotRun is set when a setup.then step made no task: the case is not tested.
	NotRun string
	// Refusal is the first refusal of the input's sender by the core, when it made no task.
	Refusal *Refusal
	// Answers are the generator answers of the case.
	Answers map[string]any
	// DataIPs are the full addresses of the generated data (INJ-4).
	DataIPs []string
}

// Refusal is a task.enqueue the core denied: the reason and the task type.
type Refusal struct{ Reason, Type string }

// TaskRow is one task of a case.
type TaskRow struct {
	ID, Type, State, Reason, Initiator string
	Result                             map[string]any
	Record                             map[string]any
	Created                            time.Time
}

// state is the terminal state of the case: the state of its task, or rejected when the input
// made no task (the core refused it in code, PRM-2).
func (o *Observation) state() string {
	if o.Self == nil {
		return "rejected"
	}
	return o.Self.State
}

// taskIDs are the ids of the case's tasks.
func (o *Observation) taskIDs() []string {
	out := make([]string, len(o.Tasks))
	for i, t := range o.Tasks {
		out[i] = t.ID
	}
	return out
}

// collect reads what happened on the bench since the case started: tasks created at or after
// since in tenant demo, with their deliveries, tool calls and audit records. The task of the
// case is the one of the input's sender with the input's idempotency key (its update id).
func (b *Bench) collect(ctx context.Context, since time.Time, sender, key string) (*Observation, error) {
	o := &Observation{}
	names := map[string]string{}
	for n, id := range b.people {
		names[id] = n
	}
	rows, err := b.st.Pool.Query(ctx, `SELECT id, type, state, coalesce(reason, ''), initiator, idempotency_key, coalesce(result, 'null'::jsonb), record, created_at
		FROM tasks WHERE tenant = 'demo' AND created_at >= $1 ORDER BY created_at, id`, since)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &TaskRow{}
		var res, rec []byte
		var idem string
		if err := rows.Scan(&t.ID, &t.Type, &t.State, &t.Reason, &t.Initiator, &idem, &res, &rec, &t.Created); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal(res, &t.Result)
		_ = json.Unmarshal(rec, &t.Record)
		if o.Self == nil && idem == key && t.Initiator == sender {
			o.Self = t
		}
		if n := names[t.Initiator]; n != "" {
			t.Initiator = n
		}
		o.Tasks = append(o.Tasks, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := o.taskIDs()
	chats := map[string]string{}
	for _, c := range b.seed.Chats {
		chats[c.ExternalID] = c.Name
	}
	drows, err := b.st.Pool.Query(ctx, `SELECT coalesce(task, ''), kind, addr, target_id, state, coalesce(body, 'null'::jsonb), coalesce(reason, '')
		FROM deliveries WHERE tenant = 'demo' AND created_at >= $1 ORDER BY seq`, since)
	if err != nil {
		return nil, err
	}
	for drows.Next() {
		var task, kind, addr, target, state, reason string
		var body []byte
		if err := drows.Scan(&task, &kind, &addr, &target, &state, &body, &reason); err != nil {
			drows.Close()
			return nil, err
		}
		var bm map[string]any
		_ = json.Unmarshal(body, &bm)
		// The target of a chat delivery is "direct:<external id>" or "group:<external id>".
		ext := target
		if i := strings.IndexByte(ext, ':'); i >= 0 {
			ext = ext[i+1:]
		}
		chat := chats[ext]
		if chat == "" {
			for e, name := range chats {
				if strings.HasSuffix(addr, ":"+e) {
					chat = name
				}
			}
		}
		lang := str(bm["locale"])
		if lang == "" {
			lang = str(bm["lang"])
		}
		o.Deliveries = append(o.Deliveries, map[string]any{"task": task, "kind": kind, "chat": chat, "state": state, "reason": reason, "lang": lang, "body": bm})
	}
	drows.Close()
	if err := drows.Err(); err != nil {
		return nil, err
	}
	trows, err := b.st.Pool.Query(ctx, `SELECT task, tool, args, tenant, state, coalesce(reason, '') FROM toolcalls WHERE task = ANY($1) ORDER BY created_at, id`, ids)
	if err != nil {
		return nil, err
	}
	for trows.Next() {
		var task, tool, tenant, state, reason string
		var args []byte
		if err := trows.Scan(&task, &tool, &args, &tenant, &state, &reason); err != nil {
			trows.Close()
			return nil, err
		}
		var am map[string]any
		_ = json.Unmarshal(args, &am)
		call := map[string]any{"task": task, "tool": tool, "args": am, "state": state, "reason": reason}
		call["tenant"] = tenant
		o.ToolCalls = append(o.ToolCalls, call)
	}
	trows.Close()
	if err := trows.Err(); err != nil {
		return nil, err
	}
	arows, err := b.st.Pool.Query(ctx, `SELECT record FROM audit WHERE record->'actor'->>'task' = ANY($1) ORDER BY tenant, epoch, seq`, ids)
	if err != nil {
		return nil, err
	}
	for arows.Next() {
		var rec []byte
		if err := arows.Scan(&rec); err != nil {
			arows.Close()
			return nil, err
		}
		var m map[string]any
		_ = json.Unmarshal(rec, &m)
		o.Audit = append(o.Audit, m)
	}
	arows.Close()
	if err := arows.Err(); err != nil {
		return nil, err
	}
	if o.Self == nil {
		var reason, typ string
		err := b.st.Pool.QueryRow(ctx, `SELECT coalesce(record->>'reason', ''), coalesce(record->'after'->>'type', record->'target'->>'id', '') FROM audit
			WHERE tenant = 'demo' AND record->>'command' = 'task.enqueue' AND record->>'ok' = 'false' AND record->'actor'->>'principal' = $1
			AND (record->>'at')::timestamptz >= $2 ORDER BY epoch, seq LIMIT 1`, sender, since).Scan(&reason, &typ)
		if err == nil {
			o.Refusal = &Refusal{Reason: reason, Type: typ}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
	}
	return o, nil
}

// pending says whether a case still has work: a task that is not terminal or a delivery of
// tenant demo that is not done. The exports of a monitoring adapter do not count: a stand with
// the monitor profile (the simulator) exports them on its own pace.
func (b *Bench) pending(ctx context.Context, since time.Time) (bool, error) {
	var n int
	err := b.st.Pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM tasks WHERE tenant = 'demo' AND created_at >= $1 AND state IN ('submitted', 'working'))
		+ (SELECT count(*) FROM deliveries WHERE tenant = 'demo' AND created_at >= $1 AND state IN ('planned', 'sending') AND exec_kind <> 'monitor')`, since).Scan(&n)
	return n > 0, err
}

// storeNow is the store clock, which the task records use.
func (b *Bench) storeNow(ctx context.Context) (time.Time, error) {
	var t time.Time
	err := b.st.Pool.QueryRow(ctx, `SELECT botaru_now()`).Scan(&t)
	return t, err
}

// counters counts the side effect attempts of a case from the audit and the tool call records,
// including those the core refused (evals/README.md, "Side effect counters"). targets holds the
// target of each attempt where a counter has an allowed list.
func (o *Observation) counters() (map[string]int, map[string][]string) {
	n := map[string]int{}
	targets := map[string][]string{}
	for _, a := range o.Audit {
		cmd := str(a["command"])
		via := str(asMap(a["actor"])["via"])
		switch cmd {
		case "action.request":
			n["request_action"]++
			targets["request_action"] = append(targets["request_action"], str(asMap(a["target"])["id"]))
		case "task.enqueue_child":
			n["enqueue_child"]++
		case "msg.send":
			n["messages_sent.any"]++
		case "memory.propose":
			n["memory_proposals"]++
		case "control.apply":
			n["control_ops"]++
		case "grant.request":
			n["grant_requests"]++
		case "campaign.propose":
			n["campaign_proposals"]++
		case "hold.set":
			if via == "bot" {
				n["holds"]++
			}
		case "decide":
			if via == "bot" {
				n["decides"]++
			}
		}
	}
	for _, t := range o.ToolCalls {
		switch tool := str(t["tool"]); tool {
		case "service/control", "monitor/script_run":
			n["service_control"]++
			args := asMap(t["args"])
			target := str(args["service"])
			if target == "" {
				target = str(args["script"])
			}
			targets["service_control"] = append(targets["service_control"], target)
		}
	}
	return n, targets
}

// countedSideEffects are the counters the runner counts; a case that names another one is not
// run, and the default zero of the others is reported as not counted.
var countedSideEffects = []string{"request_action", "service_control", "enqueue_child", "messages_sent", "memory_proposals", "decides",
	"holds", "control_ops", "grant_requests", "campaign_proposals"}

func counted(name string) bool { return slices.Contains(countedSideEffects, name) }
