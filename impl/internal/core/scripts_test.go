// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// scriptRunBench is the action bench where hello is a translator with the vocabulary
// hello.child.v1 and fix_web, and the worker role checker takes hello.child.v1.
type scriptRunBench struct {
	*actionBench
	checker *Worker
}

func newScriptRunBench(t *testing.T) *scriptRunBench {
	t.Helper()
	return newScriptRunBenchWith(t, nil)
}

// newScriptRunBenchWith is the script bench with extra manifests.
func newScriptRunBenchWith(t *testing.T, extra []map[string]any) *scriptRunBench {
	t.Helper()
	b := newActionBench(t, func(ms []map[string]any) []map[string]any {
		campaignHosts(ms)
		var child map[string]any
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			name, _ := m["metadata"].(map[string]any)["name"].(string)
			switch {
			case m["kind"] == "TaskType" && name == "hello.greet.v1":
				raw, _ := json.Marshal(m)
				_ = json.Unmarshal(raw, &child)
				s["purpose"], s["proposable"] = "translate", false
				s["result_schema"] = map[string]any{"type": "object", "additionalProperties": false, "required": []any{"outcome"}, "properties": map[string]any{
					"outcome": map[string]any{"enum": []any{"drafted", "unsupported", "ambiguous"}},
					"run_ref": map[string]any{"type": "string", "maxLength": 64},
					"missing": map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"enum": []any{"resource_missing", "action_unclear"}}},
				}}
			case m["kind"] == "Role" && name == "hello":
				s["profile"] = "translator"
				delete(s, "tools")
				delete(s, "actions_may_request")
				s["scripts"] = map[string]any{"propose": "draft", "step_types": []any{"hello.child.v1"}, "actions": []any{"fix_web"}}
			case m["kind"] == "Group" && name == "hello":
				s["caps"] = append(s["caps"].([]any), "task:hello.child.v1")
			case m["kind"] == "Action" && name == "fix_web":
				s["script"] = map[string]any{"allowed": true}
			}
		}
		cs := child["spec"].(map[string]any)
		child["metadata"].(map[string]any)["name"] = "hello.child.v1"
		cs["title"] = "Machine check"
		cs["summary_for_managers"] = "Checks one machine"
		cs["goal_templates"] = []any{map[string]any{"name": "check", "goal": "Check the machine."}}
		cs["params_schema"] = map[string]any{"type": "object", "additionalProperties": false, "required": []any{"resource"},
			"properties": map[string]any{"resource": map[string]any{"type": "string", "enum_from": "catalog.hosts"}}}
		rs := cs["result_schema"].(map[string]any)
		rs["properties"].(map[string]any)["healthy"] = map[string]any{"type": "boolean", "x-botaru-read-by": "code"}
		cs["tree_budget_eur"] = 0.1
		checker := map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "checker"},
			"spec": map[string]any{"profile": "worker", "worker_kind": "agent", "claims": []any{"hello.child.v1"}, "models": map[string]any{"base": "base-std"}, "memory": "none"},
		}
		runners := map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "runners", "tenant": "demo"},
			"spec": map[string]any{"caps": []any{"script.run"}, "data": []any{"summary"}},
		}
		return append(append(ms, child, checker, runners), extra...)
	})
	// script.run is privileged: the membership goes in directly, as the code flow would leave it.
	if _, err := b.c.st.Pool.Exec(context.Background(), `INSERT INTO memberships (tenant, principal, grp, state, created_at) VALUES ('demo', $1, 'runners', 'active', now())`, b.alice); err != nil {
		t.Fatal(err)
	}
	return &scriptRunBench{actionBench: b, checker: b.enroll(t, EnrollRequest{Tenant: "demo", Role: "checker", Kind: "agent"})}
}

type scriptOutcome struct {
	RunRef   string `json:"run_ref"`
	State    string `json:"state"`
	Problems []struct {
		Step   int    `json:"step"`
		Reason string `json:"reason"`
	} `json:"problems"`
}

func proposeScriptCall(t *testing.T, b *bench, h string, steps ...map[string]any) scriptOutcome {
	t.Helper()
	list := make([]any, len(steps))
	for i, s := range steps {
		list[i] = s
	}
	r, err := b.c.P3ToolsCall(context.Background(), h, mcp.CallParams{Name: proposeScriptName, Arguments: map[string]any{"script": map[string]any{"script": "v1", "steps": list}}})
	if err != nil {
		t.Fatal(err)
	}
	var out scriptOutcome
	if err := json.Unmarshal([]byte(callText(r)), &out); err != nil {
		t.Fatalf("result %q: %v", callText(r), err)
	}
	return out
}

func (o scriptOutcome) reason() string {
	if len(o.Problems) == 0 {
		return ""
	}
	return o.Problems[0].Reason
}

func (b *bench) run(t *testing.T, id string) (*scriptRun, string) {
	t.Helper()
	ctx := context.Background()
	r, err := loadRun(ctx, b.c.st.Pool, "demo", id, false)
	if err != nil {
		t.Fatal(err)
	}
	steps, err := loadSteps(ctx, b.c.st.Pool, id)
	if err != nil {
		t.Fatal(err)
	}
	out := ""
	for i, s := range steps {
		if i > 0 {
			out += ","
		}
		out += s.State
	}
	return r, out
}

var (
	checkStep = map[string]any{"id": "check", "kind": "task", "type": "hello.child.v1", "goal": map[string]any{"template": "check"}, "params": map[string]any{"resource": "web1"}}
	fixStep   = map[string]any{"id": "fix", "kind": "action", "action": "fix_web", "args": map[string]any{"host": "web1"}, "needs": []any{"check"},
		"when": map[string]any{"ref": "check.healthy", "op": "eq", "value": false}}
)

// Acceptance as a whole (SCR-2, SCR-3, SCR-5, TRN-3): v0.2 kinds, free goals, unknown types and
// conditions over fields that are not closed are refused with the step; only the translator
// sees propose_script.
func TestScriptAcceptance(t *testing.T) {
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	if names := toolNames(t, b.bench, h); !containsString(names, proposeScriptName) {
		t.Fatalf("catalog %v", names)
	}
	if r := proposeScriptCall(t, b.bench, h, map[string]any{"id": "pause", "kind": "wait", "for": "5m"}); r.State != "rejected" || r.reason() != "script_invalid" || r.Problems[0].Step != 1 {
		t.Fatalf("a v0.2 step: %+v", r)
	}
	unknown := map[string]any{"id": "x", "kind": "task", "type": "hello.greet.v1", "goal": map[string]any{"assignment": true}}
	if r := proposeScriptCall(t, b.bench, h, unknown); r.reason() != "script_ref_unknown" {
		t.Fatalf("a translate type: %+v", r)
	}
	open := map[string]any{"id": "fix", "kind": "action", "action": "fix_web", "args": map[string]any{"host": "web1"}, "needs": []any{"check"},
		"when": map[string]any{"ref": "check.answer", "op": "eq", "value": "ok"}}
	if r := proposeScriptCall(t, b.bench, h, checkStep, open); r.reason() != "script_invalid" || r.Problems[0].Step != 2 {
		t.Fatalf("a condition over free text: %+v", r)
	}
	// The fourth call of one attempt is over the ceiling of 3, refusals included (TRN-3).
	if r := proposeScriptCall(t, b.bench, h, checkStep, fixStep); r.reason() != "service_rate_limited" {
		t.Fatalf("a fourth call: %+v", r)
	}
	b.translated(t, att, map[string]any{"outcome": "unsupported", "missing": []any{"action_unclear"}})
	_, h = b.attemptTools(t, "2", false)
	if r := proposeScriptCall(t, b.bench, h, checkStep, fixStep); r.State != "proposed" || r.RunRef == "" {
		t.Fatalf("a valid draft: %+v", r)
	}
}

// The path of a run (SCR-6, SCR-8, SCR-11): only the initiator presses "Run", the core
// releases the check as a task, a false condition skips the action, and the run completes
// with its summary and gives its reserve back.
func TestScriptRunFlow(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	r := proposeScriptCall(t, b.bench, h, checkStep, fixStep)
	if r.State != "proposed" {
		t.Fatalf("propose %+v", r)
	}
	run, states := b.run(t, r.RunRef)
	rec, err := run.Record(ctx, b.c.st.Pool)
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "script-run", rec)
	if states != "pending,pending" || !run.Rec.NeedsRun {
		t.Fatalf("run %s %+v", states, run.Rec)
	}
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	s := b.drain(t)
	card := cardOf(t, s, scriptConfirmCard, "direct:1001")
	cd := card.body["card"].(map[string]any)
	validOut(t, b.bench, "card", cd)
	if cd["subject_kind"] != "script" || len(cd["buttons"].([]any)) != 2 {
		t.Fatalf("card %v", cd)
	}
	approve := DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}
	_, err = b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1001", card.external), approve)
	wantReason(t, err, "forbidden")
	if res, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external), approve); err != nil || res.State != "running" {
		t.Fatalf("run %+v %v", res, err)
	}
	if _, states = b.run(t, run.ID); states != "released,pending" {
		t.Fatalf("after the start %s", states)
	}
	// A step is paid by the run: out of the person's day limit and with the run's estimate on
	// top of the tree budget (SCR-8).
	var stepRec, anchorRec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT t.record FROM tasks t JOIN script_steps s ON s.task = t.id WHERE s.run = $1 AND s.id = 'check'`, run.ID).Scan(&stepRec); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, run.Rec.Root).Scan(&anchorRec); err != nil {
		t.Fatal(err)
	}
	var sr, ar struct {
		Paid bool  `json:"paid_by_card"`
		Tree int64 `json:"tree_budget_micros"`
	}
	_ = json.Unmarshal(stepRec, &sr)
	_ = json.Unmarshal(anchorRec, &ar)
	if !sr.Paid || ar.Tree <= 0 || sr.Tree != ar.Tree+run.Rec.Estimate.Micros {
		t.Fatalf("step record %+v anchor %+v estimate %d", sr, ar, run.Rec.Estimate.Micros)
	}
	item := b.claim(t, b.checker, "attempt")
	if item == nil {
		t.Fatal("no check attempt")
	}
	if _, err := b.c.Complete(ctx, b.checker, header(t, item), CompleteRequest{Result: map[string]any{"answer": "fine", "healthy": true}}); err != nil {
		t.Fatal(err)
	}
	run, states = b.run(t, run.ID)
	if run.State != "completed" || states != "ok,skipped" {
		t.Fatalf("after the check %s %s", run.State, states)
	}
	var left int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM spend WHERE script = $1`, run.ID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("reserve left %d %v", left, err)
	}
	s = b.drain(t)
	cardOf(t, s, scriptSummaryKind, "direct:1001")
}

// An action step goes to its own approval card naming the script step (SCR-7); "Cancel" on
// the run card cancels the run and the waiting approval (SCR-11).
func TestScriptActionAndCancel(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	r := proposeScriptCall(t, b.bench, h, checkStep, fixStep)
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	s := b.drain(t)
	card := cardOf(t, s, scriptConfirmCard, "direct:1001")
	run, _ := b.run(t, r.RunRef)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.checker, "attempt")
	if _, err := b.c.Complete(ctx, b.checker, header(t, item), CompleteRequest{Result: map[string]any{"answer": "down", "healthy": false}}); err != nil {
		t.Fatal(err)
	}
	if _, states := b.run(t, run.ID); states != "ok,waiting" {
		t.Fatalf("after the check %s", states)
	}
	var approval string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT approval FROM script_steps WHERE run = $1 AND id = 'fix'`, run.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	a := b.approval(t, approval)
	if a.State != "pending" || a.RequestedBy["kind"] != "script_step" || a.RequestedBy["ref"] != run.ID+":fix" {
		t.Fatalf("approval %+v", a)
	}
	s = b.drain(t)
	cardOf(t, s, "approval_card", "direct:1002")
	if res, err := b.c.ScriptControl(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		ScriptControlRequest{ID: run.ID, Command: "cancel", Nonce: run.Nonce}); err != nil || res.State != "canceled" {
		t.Fatalf("cancel %+v %v", res, err)
	}
	run, states := b.run(t, run.ID)
	if run.State != "canceled" || states != "ok,canceled" || b.approval(t, approval).State != "canceled" {
		t.Fatalf("after the cancel %s %s %s", run.State, states, b.approval(t, approval).State)
	}
}

// /cancel on the tree stops a run anchored in it although the anchor has finished: the run and
// the waiting approval are canceled and nothing more is released (SCR-11).
func TestScriptTreeCancel(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	r := proposeScriptCall(t, b.bench, h, checkStep, fixStep)
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	card := cardOf(t, b.drain(t), scriptConfirmCard, "direct:1001")
	run, _ := b.run(t, r.RunRef)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.checker, "attempt")
	if _, err := b.c.Complete(ctx, b.checker, header(t, item), CompleteRequest{Result: map[string]any{"answer": "down", "healthy": false}}); err != nil {
		t.Fatal(err)
	}
	var approval string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT approval FROM script_steps WHERE run = $1 AND id = 'fix'`, run.ID).Scan(&approval); err != nil {
		t.Fatal(err)
	}
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		return b.c.cancelTree(ctx, tx, now, run.Tenant, run.Rec.Root, "initiator")
	}); err != nil {
		t.Fatal(err)
	}
	run, states := b.run(t, run.ID)
	if run.State != "canceled" || states != "ok,canceled" || b.approval(t, approval).State != "canceled" {
		t.Fatalf("after the tree cancel %s %s %s", run.State, states, b.approval(t, approval).State)
	}
}

// A confirm step waits for its own card with its own nonce (SCR-11); "Continue" releases the
// steps after it.
func TestScriptConfirmStep(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	gate := map[string]any{"id": "ask", "kind": "confirm"}
	check := map[string]any{"id": "check", "kind": "task", "type": "hello.child.v1", "goal": map[string]any{"template": "check"}, "params": map[string]any{"resource": "web1"}, "needs": []any{"ask"}}
	r := proposeScriptCall(t, b.bench, h, gate, check)
	if r.State != "proposed" {
		t.Fatalf("propose %+v", r)
	}
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	card := cardOf(t, b.drain(t), scriptConfirmCard, "direct:1001")
	run, _ := b.run(t, r.RunRef)
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if _, states := b.run(t, run.ID); states != "waiting,pending" {
		t.Fatalf("after the start %s", states)
	}
	gc := cardOf(t, b.drain(t), scriptGateCard, "direct:1001")
	cd := gc.body["card"].(map[string]any)
	validOut(t, b.bench, "card", cd)
	if cd["subject_kind"] != "script_gate" || len(cd["buttons"].([]any)) != 2 {
		t.Fatalf("gate card %v", cd)
	}
	var id, nonce string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT gate, nonce FROM script_steps WHERE run = $1 AND id = 'ask'`, run.ID).Scan(&id, &nonce); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", gc.external),
		DecideRequest{SubjectKind: "script_gate", ID: id, Nonce: run.Nonce, Decision: "approve"}); err == nil {
		t.Fatal("the run nonce opened the gate")
	}
	// A tombstone of a run has no script to read the approvers from (RET-10).
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE script_runs SET record = record || '{"purged": true}' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	_, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", gc.external),
		DecideRequest{SubjectKind: "script_gate", ID: id, Nonce: nonce, Decision: "approve"})
	wantReason(t, err, "already_terminal")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE script_runs SET record = record - 'purged' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", gc.external),
		DecideRequest{SubjectKind: "script_gate", ID: id, Nonce: nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	if _, states := b.run(t, run.ID); states != "ok,released" {
		t.Fatalf("after the gate %s", states)
	}
}

// translated completes a translator attempt with its closed result.
func (b *bench) translated(t *testing.T, att *WorkItem, result map[string]any) {
	t.Helper()
	if _, err := b.c.Complete(context.Background(), b.agent, header(t, att), CompleteRequest{Result: result}); err != nil {
		t.Fatal(err)
	}
}

// The translator (TRN-2, TRN-3, TRN-5): a forwarded assignment starts no translator task; a
// result is only the closed translation-result whose run is the task's own; the answer to the
// person is a code text with the codes and the vocabulary names.
func TestTranslator(t *testing.T) {
	ctx := context.Background()
	b := newScriptRunBench(t)
	fwd := Activity{Kind: "message", Channel: "telegram", UpdateID: "f1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello restart everything", Forwarded: true}
	fwd.Sender.Identity, fwd.Sender.Verified = "telegram:1001", true
	if res, err := b.c.SubmitActivity(ctx, b.channel, fwd); err != nil || res.Result == "task" {
		t.Fatalf("a forwarded assignment: %+v %v", res, err)
	}
	att, _ := b.attemptTools(t, "1", false)
	_, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"outcome": "drafted", "run_ref": "scr_0000000000000000000000000"}})
	wantReason(t, err, "output_schema_invalid")
	att, _ = b.attemptTools(t, "2", false)
	b.translated(t, att, map[string]any{"outcome": "unsupported", "missing": []any{"resource_missing"}})
	answer := cardOf(t, b.drain(t), "task_result", "direct:1001")
	text, _ := answer.body["text"].(string)
	for _, want := range []string{"I cannot make a script", "not one I know", "Machine check", "Restart the web site"} {
		if !strings.Contains(text, want) {
			t.Fatalf("answer %q has no %q", text, want)
		}
	}
	var origin string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT dataorigin FROM deliveries WHERE task = $1 AND kind = 'task_result'`, att.WorkID).Scan(&origin); err != nil || origin != "code" {
		t.Fatalf("data origin %q %v", origin, err)
	}
}
