// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// INJ-8: the farm downloads no URL from a person's message: the link stays text in the goal,
// nothing fetches it while the task runs, and an attached file comes as a file_ref whose
// content is untrusted, whoever uploaded it.
func TestBackfillLinkStaysText(t *testing.T) {
	ctx := context.Background()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer srv.Close()
	b := newFileBench(t)
	alice := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	up, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/plain"}, []byte("notes"))
	if err != nil {
		t.Fatal(err)
	}
	link := srv.URL + "/page"
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "1", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/hello read " + link, Attachments: []string{up.FileRef}}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	r, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || r.Result != "task" {
		t.Fatalf("submit: %+v %v", r, err)
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	body := att.Payload["task"].(map[string]any)["body"].(map[string]any)
	if body["goal"] != "read "+link {
		t.Fatalf("INJ-8: the link is not kept as text: %v", body["goal"])
	}
	raw, _ := json.Marshal(att.Payload)
	if !strings.Contains(string(raw), up.FileRef) {
		t.Fatalf("INJ-8: the attachment does not come as its file_ref: %s", raw)
	}
	var untrusted bool
	var source string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce((record->>'untrusted_seen')::bool, false), coalesce(record->>'untrusted_source', '') FROM tasks WHERE id = $1`, r.Task).Scan(&untrusted, &source); err != nil {
		t.Fatal(err)
	}
	if !untrusted || source != "attachment" {
		t.Fatalf("INJ-8: a person's own file is not untrusted: %v %q", untrusted, source)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "ok"}}); err != nil {
		t.Fatal(err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("INJ-8: the farm fetched the link %d times", n)
	}
}

// INJ-10: the model changes no configuration: no built-in tool of the registry writes a
// manifest, a role, a permission, a skill, a persona, a charter, a limit or a budget, a call of
// such a name is refused, and an attempt leaves the manifests as git set them.
func TestBackfillModelChangesNoConfig(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	reg, err := sp.Registry("spec/model/builtin-tools.yaml")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range reg.Entries {
		id := asString(e["id"])
		for _, w := range []string{"apply", "manifest", "role", "permission", "skill_write", "persona", "charter", "budget", "limit", "catalog", "group", "seat"} {
			if strings.Contains(id, w) {
				t.Errorf("INJ-10: the built-in tool %s changes configuration", id)
			}
		}
	}
	ctx := context.Background()
	b := newToolBench(t)
	before := storedHash(t, b, "Role", "", "hello")
	if before == "" {
		t.Fatal("no stored role hello")
	}
	_, h := b.attemptTools(t, "1", true)
	for _, name := range []string{"apply", "manifest_patch", "role_update", "skill_write", "grant", "budget_set"} {
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: name, Arguments: map[string]any{}})
		if err == nil && !r.IsError {
			t.Errorf("INJ-10: the call %s passed: %s", name, callText(r))
		}
	}
	if storedHash(t, b, "Role", "", "hello") != before {
		t.Fatal("INJ-10: an attempt changed the role")
	}
	// Proposals change a manifest only by a patch a person commits: the core has no write of
	// manifests outside apply (impl/internal/core/apply.go).
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "apply.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(src), "INSERT INTO manifests") || strings.Contains(string(src), "UPDATE manifests") {
			t.Errorf("INJ-10: %s writes manifests outside apply", f)
		}
	}
}

// INJ-16: script names reach the model only from the catalog: a script the monitoring system
// lists but the catalog does not is not in the enum, a catalog script the list lacks is not
// offered, and a name the adapter sends in the list reaches neither the tool nor the events;
// a role gets scripts by name or ["*"], never true.
func TestBackfillScriptNamesFromCatalog(t *testing.T) {
	ctx := context.Background()
	b := newScriptBench(t)
	b.scripts(t, "hash-0003", map[string]any{"scriptid": "17", "input": "none"},
		map[string]any{"scriptid": "99", "input": "none"})
	_, h := b.attemptTools(t, "1", true)
	l, err := b.c.P3ToolsList(ctx, h)
	if err != nil {
		t.Fatal(err)
	}
	var schema string
	for _, x := range l.Tools {
		if m := x.(map[string]any); m["name"] == scriptToolName {
			raw, _ := json.Marshal(m)
			schema = string(raw)
		}
	}
	if !strings.Contains(schema, "app-api-restart") || strings.Contains(schema, "cache-clear") || strings.Contains(schema, "free-input") || strings.Contains(schema, `"99"`) {
		t.Fatalf("INJ-16: the script tool %s", schema)
	}
	// A list that carries a name of the monitoring system: the name enters nothing.
	if _, err := b.c.SubmitActivity(ctx, b.mon, func() Activity {
		a := Activity{Kind: "signal", Channel: "zabbix", UpdateID: "hash-0004", Chat: Chat{Kind: "direct", ExternalID: "monitor"}, Event: &SignalEvent{Type: "ext.monitor.scripts.v1",
			Data: map[string]any{"resource": "web1", "hash": "hash-0004", "scripts": []any{map[string]any{"scriptid": "17", "input": "none", "name": "restart nginx now"}}}}}
		a.Sender.Identity, a.Sender.Verified = "hook:monitor", true
		return a
	}()); err != nil {
		t.Logf("the list with a name is refused: %v", err)
	}
	_, h2 := b.attemptTools(t, "2", true)
	l, err = b.c.P3ToolsList(ctx, h2)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(l)
	var inEvents int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE data::text LIKE '%restart nginx now%'`).Scan(&inEvents); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "restart nginx now") || inEvents != 0 {
		t.Fatalf("INJ-16: a name from the monitoring system reached the model (%v) or the events (%d)", strings.Contains(string(raw), "restart nginx now"), inEvents)
	}
	for _, v := range []any{true, []any{"*"}, []any{"app-api-restart"}} {
		err := b.c.spec.Validate("manifest:role", map[string]any{"worker_kind": "agent", "claims": []any{"hello.greet.v1"}, "models": map[string]any{"base": "base-std"}, "monitor_scripts": v})
		if (v == true) != (err != nil) {
			t.Errorf("INJ-16: monitor_scripts %v: %v", v, err)
		}
	}
}
