// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// The claim payload carries full_result_schema beside the unchanged result_schema: enum_from
// becomes the catalog values of the attempt and x-botaru-format the pattern and length of the
// format, so the runtime checks a result as complete does (OUT-1).
func TestFullResultSchemaPayload(t *testing.T) {
	t.Parallel()
	b := newBench(t)
	ms, at := helloSet(t)
	tt := ms[at["TaskType"]]["spec"].(map[string]any)
	declared := map[string]any{
		"type": "object", "additionalProperties": false, "required": []any{"answer"},
		"properties": map[string]any{
			"answer":   map[string]any{"type": "string", "maxLength": 500},
			"services": map[string]any{"type": "array", "maxItems": 5, "items": map[string]any{"enum_from": "catalog.services"}},
			"host":     map[string]any{"enum_from": "catalog.hosts"},
			"facts": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
				"subnet_401": map[string]any{"type": "string", "x-botaru-format": "ipv4_subnet24"},
				"top_urls":   map[string]any{"type": "array", "maxItems": 3, "items": map[string]any{"type": "string", "x-botaru-format": "url_path"}},
				"day":        map[string]any{"type": "string", "x-botaru-format": "date"},
			}},
		},
	}
	tt["result_schema"] = declared
	ms = append(ms, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
		"spec": map[string]any{"hosts": []any{
			map[string]any{"id": "web1", "services": []any{map[string]any{"name": "nginx", "unit": "nginx.service", "control": []any{"restart"}, "via": "host"}}},
			map[string]any{"id": "db1"},
		}},
	})
	if res, err := b.c.Apply(context.Background(), b.op, ApplyRequest{Commit: "7777777777777777777777777777777777777777", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	b.hello(t, "51")
	att := b.claim(t, b.agent, "attempt")
	// The projection stays the declared schema.
	var want map[string]any
	raw, _ := json.Marshal(declared)
	_ = json.Unmarshal(raw, &want)
	if got := att.Payload["result_schema"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("result_schema changed: %v", got)
	}
	full, _ := att.Payload["full_result_schema"].(map[string]any)
	if full == nil {
		t.Fatalf("no full_result_schema: %v", att.Payload)
	}
	props := full["properties"].(map[string]any)
	if e := props["services"].(map[string]any)["items"].(map[string]any)["enum"]; !reflect.DeepEqual(e, []any{"nginx"}) {
		t.Errorf("services enum %v", e)
	}
	if e, _ := props["host"].(map[string]any)["enum"].([]any); !slices.Contains(e, any("web1")) || !slices.Contains(e, any("db1")) {
		t.Errorf("host enum %v", e)
	}
	facts := props["facts"].(map[string]any)["properties"].(map[string]any)
	for _, f := range []map[string]any{facts["subnet_401"].(map[string]any), facts["top_urls"].(map[string]any)["items"].(map[string]any), facts["day"].(map[string]any)} {
		if _, ok := f["pattern"].(string); !ok || f["maxLength"] == nil {
			t.Errorf("format leaf without pattern or maxLength: %v", f)
		}
	}
	if strings.Contains(string(mustJSON(full)), "enum_from") {
		t.Error("an enum_from is left in the full schema")
	}
	good := map[string]any{"answer": "a", "services": []any{"nginx"}, "host": "db1",
		"facts": map[string]any{"subnet_401": "203.0.113.0/24", "top_urls": []any{"/api/v1/report"}, "day": "2026-10-20"}}
	if err := b.c.rs.validate(full, good); err != nil {
		t.Errorf("a valid result was refused: %v", err)
	}
	for _, bad := range []map[string]any{
		{"answer": "a", "services": []any{"postgres"}},
		{"answer": "a", "facts": map[string]any{"subnet_401": "203.0.113.7"}},
		{"answer": "a", "facts": map[string]any{"top_urls": []any{"no slash"}}},
		{"answer": "a", "facts": map[string]any{"day": "20.10.2026"}},
	} {
		if b.c.rs.validate(full, bad) == nil {
			t.Errorf("an invalid result passed: %v", bad)
		}
	}
}

// observations_query names the subjects the series may have in the tenant as a closed list and
// says in its description which kind each series takes; an analysis gets the subjects of its
// role's work in insight_context (OBV-3).
func TestObservationsQuerySubjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newInsightBench(t, map[string]any{"tools": []any{}})
	_, att := b.analysisAttempt(t, "1")
	h, _ := att.Payload["handles"].(map[string]any)["tools"].(string)
	l, err := b.c.P3ToolsList(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	var tool map[string]any
	for _, x := range l.Tools {
		if m := x.(map[string]any); m["name"] == obsToolName {
			tool = m
		}
	}
	if tool == nil {
		t.Fatal("no observations_query")
	}
	desc, _ := tool["description"].(string)
	if !strings.Contains(desc, "farm.task.attempts takes task_type:hello.greet.v1") {
		t.Errorf("description %q", desc)
	}
	subject := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)["subject"].(map[string]any)
	enum, _ := subject["enum"].([]any)
	if !slices.Contains(enum, any("task_type:hello.greet.v1")) || !slices.Contains(enum, any("task_type:hello.insight.v1")) {
		t.Errorf("subject enum %v", enum)
	}
	ic, _ := att.Payload["insight_context"].(map[string]any)
	if types, _ := ic["role_task_types"].([]any); !slices.Equal(types, []any{"task_type:hello.greet.v1"}) {
		t.Errorf("insight_context %v", ic)
	}
	if _, ok := ic["proposable_targets"]; ok {
		t.Error("proposable_targets without the evolution")
	}
	// A subject of the list reads; the core still checks it as before.
	r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: obsToolName, Arguments: map[string]any{"series": "farm.task.attempts", "subject": "task_type:hello.greet.v1", "window": "1d", "rollup": "window.mean"}})
	if err != nil || r.IsError {
		t.Fatalf("query: %+v %v", r, err)
	}
}

// With the evolution on and one dispatcher rule, the analysis sees the guarded fields of that
// rule with their current values, the ones checkTenantOp takes (PRP-12); with two rules, none.
func TestInsightContextDispatch(t *testing.T) {
	rule := map[string]any{"match": map[string]any{"type": "ops.investigate.v1"}, "timeouts": map[string]any{"attempt": "5m"},
		"retry": map[string]any{"max_attempts": 1}}
	cat := &Catalog{Tenants: map[string]*TenantView{"demo": {Name: "demo", Raw: map[string]any{"spec": map[string]any{
		"limits": map[string]any{"max_queued": 50}, "dispatch": map[string]any{"rules": []any{rule}}}}}}}
	pt, _ := insightContext(cat, "demo", "ops-reader", true)["proposable_targets"].(map[string]any)
	d, _ := pt["dispatch"].(map[string]any)
	fields, _ := d["fields"].(map[string]any)
	f, _ := fields["spec.dispatch.rules[].timeouts.attempt"].(map[string]any)
	if f["current"] != "5m" || len(fields) != 1 {
		t.Fatalf("dispatch targets %v", d)
	}
	if tg, _ := d["target"].(map[string]any); tg["kind"] != "Tenant" || tg["name"] != "demo" {
		t.Errorf("target %v", d["target"])
	}
	rule["retry"] = map[string]any{"max_attempts": 3}
	pt, _ = insightContext(cat, "demo", "ops-reader", true)["proposable_targets"].(map[string]any)
	d, _ = pt["dispatch"].(map[string]any)
	if fields, _ := d["fields"].(map[string]any); len(fields) != 2 {
		t.Errorf("an attempt limit of 3 is not offered: %v", d)
	}
	cat.Tenants["demo"].Raw["spec"].(map[string]any)["dispatch"] = map[string]any{"rules": []any{rule, rule}}
	pt, _ = insightContext(cat, "demo", "ops-reader", true)["proposable_targets"].(map[string]any)
	if _, ok := pt["dispatch"]; ok {
		t.Error("dispatch targets with two rules")
	}
}
