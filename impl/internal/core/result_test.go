// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// applyResultCheckSet applies the hello-bot set with a result schema that has an enum_from
// list and a formatted field, and a catalog whose list has one value at the task's mark
// (summary) and one only above it (internal).
func applyResultCheckSet(t *testing.T, b *bench) {
	t.Helper()
	ms, at := helloSet(t)
	tt := ms[at["TaskType"]]["spec"].(map[string]any)
	tt["result_schema"] = map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"answer"},
		"properties": map[string]any{
			"answer": map[string]any{"type": "string", "maxLength": 500},
			"run":    map[string]any{"enum_from": "catalog.lab_runs"},
			"month":  map[string]any{"type": "string", "x-botaru-format": "year_month"},
		},
	}
	tt["retry"] = map[string]any{"max_attempts": 3, "backoff": map[string]any{"initial": "1s", "max": "1s"}}
	ms = append(ms, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
		"spec": map[string]any{"resources": []any{
			map[string]any{"id": "runs-a", "title": "Runs", "list": "lab_runs", "format": "id", "values": []any{"r-01"}, "data_class": "summary"},
			map[string]any{"id": "runs-b", "title": "Internal runs", "list": "lab_runs", "format": "id", "values": []any{"r-09"}, "data_class": "internal"},
		}},
	})
	res, err := b.c.Apply(context.Background(), b.op, ApplyRequest{Commit: "6666666666666666666666666666666666666666", Manifests: ms})
	if err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
}

func (b *bench) hello(t *testing.T, update string) string {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello hi"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil || res.Result != "task" {
		t.Fatalf("submit_activity: %+v %v", res, err)
	}
	return res.Task
}

// advance moves the test bench clock forward.
func (b *bench) advance(t *testing.T, seconds int) {
	t.Helper()
	if _, err := b.c.st.Pool.Exec(context.Background(), `UPDATE farm_meta SET clock_offset_us = clock_offset_us + $1`, int64(seconds)*1000000); err != nil {
		t.Fatal(err)
	}
}

// The full result check at complete (OUT-1, D-98, blackbox:model/result-full-schema): a value
// outside the catalog list, a value the list has only above the task's mark and a value that
// breaks its x-botaru-format each get output_schema_invalid; the attempt is spent with backoff
// once, no result is stored, the identical repeat gets output_schema_invalid again, and another
// complete under the same lease is fenced.
func TestResultFullSchema(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	applyResultCheckSet(t, b)
	task := b.hello(t, "21")
	bad := []map[string]any{
		{"answer": "a", "run": "r-99"},
		{"answer": "a", "run": "r-09"},
		{"answer": "a", "month": "2026-13"},
	}
	for i, result := range bad {
		att := b.claim(t, b.agent, "attempt")
		if att == nil || att.WorkID != task || att.LeaseNo != i+1 {
			t.Fatalf("case %d: attempt %+v", i, att)
		}
		h := header(t, att)
		_, err := b.c.Complete(ctx, b.agent, h, CompleteRequest{Result: result})
		if p, _ := AsProblem(err); p == nil || p.Reason != "output_schema_invalid" {
			t.Fatalf("case %d: complete %v", i, err)
		}
		// The identical repeat gets the same answer (IDM-3); another result is fenced.
		_, err = b.c.Complete(ctx, b.agent, h, CompleteRequest{Result: result})
		if p, _ := AsProblem(err); p == nil || p.Reason != "output_schema_invalid" {
			t.Fatalf("case %d: repeated complete: %v", i, err)
		}
		_, err = b.c.Complete(ctx, b.agent, h, CompleteRequest{Result: map[string]any{"answer": "a", "run": "r-01"}})
		if p, _ := AsProblem(err); p == nil || p.Reason != "fenced" {
			t.Fatalf("case %d: second complete under the same lease: %v", i, err)
		}
		var state string
		var reason *string
		var spent int
		var stored []byte
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, reason, attempts_spent, result FROM tasks WHERE id = $1`, task).Scan(&state, &reason, &spent, &stored); err != nil {
			t.Fatal(err)
		}
		if spent != i+1 || stored != nil {
			t.Fatalf("case %d: spent %d, result %s", i, spent, stored)
		}
		if i == len(bad)-1 {
			// The third spent attempt is the last one of the type (max_attempts 3).
			if state != "failed" || reason == nil || *reason != "attempts_exhausted" {
				t.Fatalf("case %d: task %s %v", i, state, reason)
			}
			break
		}
		if state != "submitted" {
			t.Fatalf("case %d: task %s", i, state)
		}
		if w := b.claim(t, b.agent, "attempt"); w != nil {
			t.Fatalf("case %d: the retry did not wait for its backoff", i)
		}
		b.advance(t, 2)
	}
	// A result within the list at the task's mark and in its format passes.
	task = b.hello(t, "22")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != task {
		t.Fatalf("attempt %+v", att)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "a", "run": "r-01", "month": "2026-09"}}); err != nil {
		t.Fatal(err)
	}
}

// A lease taken before the upgrade has no attempt record (leases.attempt is NULL): the core
// resolves the enum_from values at complete, filtered by the task's mark, instead of refusing
// every value.
func TestResultCheckWithoutAttemptRecord(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	applyResultCheckSet(t, b)
	task := b.hello(t, "31")
	dropRecord := func(att *WorkItem) {
		t.Helper()
		if att == nil || att.WorkID != task {
			t.Fatalf("attempt %+v", att)
		}
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE leases SET attempt = NULL, fingerprint = NULL WHERE work_id = $1`, task); err != nil {
			t.Fatal(err)
		}
	}
	att := b.claim(t, b.agent, "attempt")
	dropRecord(att)
	_, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "a", "run": "r-09"}})
	if p, _ := AsProblem(err); p == nil || p.Reason != "output_schema_invalid" {
		t.Fatalf("a value above the task's mark: %v", err)
	}
	b.advance(t, 2)
	att = b.claim(t, b.agent, "attempt")
	dropRecord(att)
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "a", "run": "r-01"}}); err != nil {
		t.Fatalf("a value of the list: %v", err)
	}
}

// The run fingerprint is computed by the core at claim and written into the attempt, the usage
// lines and the task.completed event (PRV-1); the task record carries fairness_key and trace
// (DSP-2, OBS-2), and every event of the task carries the root as correlationid.
func TestRunFingerprint(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "31")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != task {
		t.Fatalf("attempt %+v", att)
	}
	var fp string
	var raw []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT fingerprint, attempt FROM leases WHERE work_id = $1 AND lease_no = 1`, task).Scan(&fp, &raw); err != nil {
		t.Fatal(err)
	}
	var rec attemptRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(fp, "sha256:") || rec.Fingerprint != fp {
		t.Fatalf("fingerprint %q, record %+v", fp, rec)
	}
	for _, k := range []string{"role", "type", "skills", "model", "runtime"} {
		if s, _ := rec.Parts[k].(string); !strings.HasPrefix(s, "sha256:") {
			t.Fatalf("part %s: %v", k, rec.Parts[k])
		}
	}
	for _, k := range []string{"persona", "catalog", "sandbox"} {
		if v, ok := rec.Parts[k]; !ok || v != nil {
			t.Fatalf("part %s should be null: %v", k, v)
		}
	}
	cat, _ := b.c.Catalog(ctx)
	again, parts, err := runFingerprint(cat, b.agent, "hello", "hello.greet.v1", "base", "base-std", nil, nil)
	if err != nil || again != fp || parts["model"] != rec.Parts["model"] {
		t.Fatalf("recomputed %s, %v", again, err)
	}
	if other, _, _ := runFingerprint(cat, b.agent, "hello", "hello.greet.v1", "base", "seat-std", nil, nil); other == fp {
		t.Fatal("another configured profile gave the same fingerprint")
	}
	handle, _ := att.Payload["handles"].(map[string]any)["model"].(string)
	g, err := b.c.ResolveModelHandle(ctx, handle)
	if err != nil || g.Fingerprint != fp {
		t.Fatalf("grant %+v %v", g, err)
	}
	spend, err := b.c.Reserve(ctx, g, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.Charge(ctx, spend, 10, &Tokens{Input: 1, Output: 1}, "req_fp", false); err != nil {
		t.Fatal(err)
	}
	var lineFP string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT fingerprint FROM spend WHERE id = $1`, spend).Scan(&lineFP); err != nil || lineFP != fp {
		t.Fatalf("usage line fingerprint %q %v", lineFP, err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "hi"}}); err != nil {
		t.Fatal(err)
	}
	var evFP, corr string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data->>'fingerprint', correlationid FROM events WHERE subject = $1 AND type = 'task.completed'`, task).Scan(&evFP, &corr); err != nil || evFP != fp || corr != task {
		t.Fatalf("task.completed fingerprint %q correlation %q %v", evFP, corr, err)
	}
	var fk, trace string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'fairness_key', record->>'trace' FROM tasks WHERE id = $1`, task).Scan(&fk, &trace); err != nil {
		t.Fatal(err)
	}
	if fk != "interactive:"+b.alice {
		t.Fatalf("fairness_key %q", fk)
	}
	if len(trace) != 55 || !strings.HasPrefix(trace, "00-") || !strings.HasSuffix(trace, "-01") {
		t.Fatalf("trace %q", trace)
	}
	var missing int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events e WHERE (e.subject = $1 OR e.data->>'task' = $1) AND (e.correlationid IS DISTINCT FROM $1 OR e.causationid IS NULL)`, task).Scan(&missing); err != nil || missing != 0 {
		t.Fatalf("%d events of the task without the root as correlationid or without a cause (%v)", missing, err)
	}
	var cause string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT causationid FROM events WHERE subject = $1 AND type = 'task.submitted'`, task).Scan(&cause); err != nil || cause != "system:activity:telegram:31" {
		t.Fatalf("task.submitted cause %q %v", cause, err)
	}
}

// fairnessKey covers every source of DSP-2; a task the core creates itself gets
// system:<purpose>, and a child or continuation keeps its source's key.
func TestFairnessKey(t *testing.T) {
	cases := []struct {
		in   enqueueInput
		want string
	}{
		{enqueueInput{Source: "interactive", Initiator: "human:p_1"}, "interactive:human:p_1"},
		{enqueueInput{Source: "schedule", SourceID: "daily"}, "schedule:daily"},
		{enqueueInput{Source: "route", SourceID: "r1"}, "route:r1"},
		{enqueueInput{Source: "campaign", SourceID: "c1"}, "campaign:c1"},
		{enqueueInput{Source: "script", SourceID: "run1"}, "script:run1"},
		{enqueueInput{FairnessKey: "root:t_1", Source: "interactive", Initiator: "x"}, "root:t_1"},
		{enqueueInput{}, "system:review"},
	}
	for _, c := range cases {
		tt := manifest.TaskType{Purpose: "review"}
		if got := fairnessKey(c.in, tt); got != c.want {
			t.Errorf("%+v: %q, want %q", c.in, got, c.want)
		}
	}
}

// The result is checked against the result_schema of the type at claim: an apply that
// narrows the type while the attempt runs does not refuse the attempt's valid result.
func TestResultSchemaFixedAtClaim(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	applyResultCheckSet(t, b)
	task := b.hello(t, "41")
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != task {
		t.Fatalf("attempt %+v", att)
	}
	ms, at := helloSet(t)
	ms[at["TaskType"]]["spec"].(map[string]any)["result_schema"] = map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"answer"},
		"properties": map[string]any{"answer": map[string]any{"type": "string", "maxLength": 1}},
	}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "5555555555555555555555555555555555555555", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "longer than one", "run": "r-01"}}); err != nil {
		t.Fatalf("a result valid for the type at claim: %v", err)
	}
}

// The result schema a runtime may send for constrained decoding is only a hint: a result that
// the projection of the attempt accepts, where x-botaru-format is no assertion, is still
// checked against the full schema at complete and refused (OUT-2, OUT-1).
func TestResultProjectionHint(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	applyResultCheckSet(t, b)
	b.hello(t, "41")
	att := b.claim(t, b.agent, "attempt")
	proj, _ := att.Payload["result_schema"].(map[string]any)
	if proj == nil {
		t.Fatalf("no projection: %v", att.Payload)
	}
	result := map[string]any{"answer": "a", "month": "2026-13"}
	if err := b.c.rs.validate(proj, result); err != nil {
		t.Fatalf("the projection refused the result: %v", err)
	}
	_, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: result})
	if p, _ := AsProblem(err); p == nil || p.Reason != "output_schema_invalid" {
		t.Fatalf("complete: %v", err)
	}
}
