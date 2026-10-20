// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// SCR-1: a script is data under the closed schema botaru.script/v1: a step kind outside the
// closed list, a run_if or on_failure outside theirs, a field of code or a loop, and then
// inside fanout do not pass; a draft has no more steps than the tenant ceiling.
func TestBackfillScriptClosedSchema(t *testing.T) {
	t.Parallel()
	b := newScriptRunBench(t)
	draft := func(steps ...map[string]any) map[string]any {
		list := make([]any, len(steps))
		for i, s := range steps {
			list[i] = s
		}
		return map[string]any{"script": map[string]any{"script": "v1", "steps": list}}
	}
	with := func(base map[string]any, k string, v any) map[string]any {
		m := deepCopy(base)
		m[k] = v
		return m
	}
	if err := b.c.spec.Validate("script-propose-request", draft(checkStep, fixStep)); err != nil {
		t.Fatalf("a valid draft: %v", err)
	}
	for name, d := range map[string]map[string]any{
		"kind outside the list": draft(with(checkStep, "kind", "loop")),
		"run_if never":          draft(checkStep, with(fixStep, "run_if", "never")),
		"on_failure retry":      draft(with(checkStep, "on_failure", "retry")),
		"code in a step":        draft(with(checkStep, "code", "rm -rf /")),
		"a nested script":       draft(with(checkStep, "script", map[string]any{"script": "v1", "steps": []any{checkStep}})),
		"then inside fanout": draft(map[string]any{"id": "all", "kind": "fanout", "action": "fix_web", "items": map[string]any{"list": []any{"web1"}},
			"then": map[string]any{"kind": "task"}}),
	} {
		if err := b.c.spec.Validate("script-propose-request", d); err == nil {
			t.Errorf("SCR-1: %s passed the schema", name)
		}
	}
	for _, runIf := range []string{"succeeded", "always", "failed"} {
		if err := b.c.spec.Validate("script-propose-request", draft(checkStep, with(fixStep, "run_if", runIf))); err != nil {
			t.Errorf("SCR-1: run_if %s: %v", runIf, err)
		}
	}
	// More steps than the tenant ceiling of 10.
	_, h := b.attemptTools(t, "1", false)
	var many []map[string]any
	for i := range 11 {
		many = append(many, with(checkStep, "id", fmt.Sprintf("check%d", i)))
	}
	if r := proposeScriptCall(t, b.bench, h, many...); r.State != "rejected" || r.reason() != "script_too_large" {
		t.Fatalf("SCR-1: eleven steps: %+v", r)
	}
}

// SCR-4: in a draft the goal of an agent step is a template of the type or the assignment;
// a free goal text, or the goals of a git script, is refused.
func TestBackfillScriptNoFreeGoal(t *testing.T) {
	t.Parallel()
	b := newScriptRunBench(t)
	_, h := b.attemptTools(t, "1", false)
	free := deepCopy(checkStep)
	free["goal"] = "Restart everything you can find."
	if r := proposeScriptCall(t, b.bench, h, free); r.State != "rejected" || r.reason() != "script_invalid" && r.reason() != "script_free_goal" {
		t.Fatalf("SCR-4: a free goal string: %+v", r)
	}
	assignment := deepCopy(checkStep)
	assignment["goal"] = map[string]any{"assignment": true}
	if r := proposeScriptCall(t, b.bench, h, assignment); r.State != "proposed" {
		t.Fatalf("SCR-4: the assignment as a goal: %+v", r)
	}
	// goals is a field of a git script; a draft with it is refused.
	if r := proposeScriptWith(t, b.bench, h, map[string]any{"script": "v1", "steps": []any{checkStep}, "goals": map[string]any{"mine": "Free text."}}); r.State != "rejected" {
		t.Fatalf("SCR-4: a draft with goals: %+v", r)
	}
}

// proposeScriptWith calls propose_script with a whole script object.
func proposeScriptWith(t *testing.T, b *bench, h string, script map[string]any) scriptOutcome {
	t.Helper()
	r, err := b.c.P3ToolsCall(context.Background(), h, mcp.CallParams{Name: proposeScriptName, Arguments: map[string]any{"script": script}})
	if err != nil {
		t.Fatal(err)
	}
	var out scriptOutcome
	if err := json.Unmarshal([]byte(callText(r)), &out); err != nil {
		t.Fatalf("result %q: %v", callText(r), err)
	}
	return out
}

// TRN-4: a draft from an attempt that saw untrusted text has at most one action step;
// otherwise script_untrusted_limit. A draft within the limit carries the flag and its source
// into the run. (The fanout and selector limits need an action with a campaign, which this
// bench's fix_web lacks; they are not exercised here.)
func TestBackfillScriptUntrustedLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "attachment"}' WHERE id = $1`, att.WorkID); err != nil {
		t.Fatal(err)
	}
	second := deepCopy(fixStep)
	second["id"] = "fix2"
	if r := proposeScriptCall(t, b.bench, h, checkStep, fixStep, second); r.State != "rejected" || r.reason() != "script_untrusted_limit" {
		t.Fatalf("TRN-4: two action steps after untrusted text: %+v", r)
	}
	r := proposeScriptCall(t, b.bench, h, checkStep, fixStep)
	if r.State != "proposed" {
		t.Fatalf("TRN-4: one action step after untrusted text: %+v", r)
	}
	run, _ := b.run(t, r.RunRef)
	if !run.Rec.Untrusted.Value || run.Rec.Untrusted.source() == "" {
		t.Fatalf("TRN-4: the run lost the flag or its source: %+v", run.Rec.Untrusted)
	}
}

// SCR-9: the flag of a step is the run flag or that of the earlier steps whose fields it reads;
// an action step whose condition read a flagged check shows the source on its approval.
func TestBackfillScriptStepFlag(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newScriptRunBench(t)
	att, h := b.attemptTools(t, "1", false)
	r := proposeScriptCall(t, b.bench, h, checkStep, fixStep)
	b.translated(t, att, map[string]any{"outcome": "drafted", "run_ref": r.RunRef})
	s := b.drain(t)
	card := cardOf(t, s, scriptConfirmCard, "direct:1001")
	run, _ := b.run(t, r.RunRef)
	if run.Rec.Untrusted.Value {
		t.Fatal("the run starts flagged")
	}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1001", "direct:1001", card.external),
		DecideRequest{SubjectKind: "script", ID: run.ID, Nonce: run.Nonce, Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.checker, "attempt")
	// The check read untrusted text.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true, "untrusted_source": "attachment"}' WHERE id = $1`, item.WorkID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Complete(ctx, b.checker, header(t, item), CompleteRequest{Result: map[string]any{"answer": "down", "healthy": false}}); err != nil {
		t.Fatal(err)
	}
	var approval string
	var checkFlag, fixFlag bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(approval, ''), coalesce((untrusted->>'value')::bool, false) FROM script_steps WHERE run = $1 AND id = 'fix'`, run.ID).Scan(&approval, &fixFlag); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce((untrusted->>'value')::bool, false) FROM script_steps WHERE run = $1 AND id = 'check'`, run.ID).Scan(&checkFlag); err != nil {
		t.Fatal(err)
	}
	if !checkFlag || !fixFlag || approval == "" {
		t.Fatalf("SCR-9: the flags check %v fix %v, approval %q", checkFlag, fixFlag, approval)
	}
	a := b.approval(t, approval)
	if !a.UntrustedSeen || a.UntrustedSource == "" {
		t.Fatalf("SCR-9: the approval does not show the source: %+v", a)
	}
}
