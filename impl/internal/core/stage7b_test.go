// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// A file above max_file_bytes and a file that takes a task past max_task_files_bytes are
// refused with file_too_large and leave no record; the storage ceiling still gives over_capacity
// (ART-4, CAP-1).
func TestFileSizeLimits(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			switch m["kind"] {
			case "Tenant":
				m["spec"].(map[string]any)["limits"] = map[string]any{"max_file_bytes": 20, "max_task_files_bytes": 30, "max_storage_bytes": 45}
			case "Group":
				m["spec"].(map[string]any)["data"] = []any{"summary", "internal"}
			}
		}
		return ms
	})
	alice := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	count := func() int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM files`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/plain"}, make([]byte, 21)); err == nil || !strings.Contains(err.Error(), "file_too_large") {
		t.Fatalf("a file above max_file_bytes: %v", err)
	}
	if count() != 0 {
		t.Fatal("a refused file left a record")
	}
	b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := header(t, att)
	job := *b.agent
	job.Kind = "job"
	if _, err := b.c.FilePut(ctx, &job, h, nil, FilePutRequest{MediaType: "text/plain"}, make([]byte, 20)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.FilePut(ctx, &job, h, nil, FilePutRequest{MediaType: "text/plain"}, make([]byte, 11)); err == nil || !strings.Contains(err.Error(), "file_too_large") {
		t.Fatalf("a file past max_task_files_bytes: %v", err)
	}
	if _, err := b.c.FilePut(ctx, &job, h, nil, FilePutRequest{MediaType: "text/plain"}, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/plain"}, make([]byte, 16)); err == nil || !strings.Contains(err.Error(), "over_capacity") {
		t.Fatalf("a write past max_storage_bytes: %v", err)
	}
	if count() != 2 {
		t.Fatalf("files %d", count())
	}
}

// People and sinks get only result files of the task whose sniffed type the type lists; an
// HTML file named .png is not handed out, a CSV is text/plain unless its name refines it, and
// the lint refuses an active type and a PDF of an agent type (ART-5).
func TestResultMediaTypes(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" {
				m["spec"].(map[string]any)["result_media_types"] = []any{"image/png", "text/csv"}
			}
		}
		return append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Sink", "metadata": map[string]any{"name": "reports", "tenant": "demo"},
			"spec": map[string]any{"driver": "disk", "base": "/srv/reports", "ceiling": "internal"}})
	})
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := header(t, att)
	job := *b.agent
	job.Kind = "job"
	put := func(name string, content []byte) string {
		r, err := b.c.FilePut(ctx, &job, h, nil, FilePutRequest{Filename: name, MediaType: "image/png"}, content)
		if err != nil {
			t.Fatal(err)
		}
		return r.FileRef
	}
	html := put("report.png", []byte("<!DOCTYPE html><html><script>alert(1)</script></html>"))
	png := put("plot.png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	csvPlain := put("data", []byte("station,depth\nst-03,12\n"))
	csv := put("data.csv", []byte("station,depth\nst-03,12\n"))
	var mt string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT media_type FROM files WHERE file_ref = $1`, csv).Scan(&mt); err != nil || mt != "text/csv" {
		t.Fatalf("refined type %q %v", mt, err)
	}
	// A delivery of the task's result to a sink names the four files.
	sw := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "reports"})
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO deliveries (id, tenant, task, kind, addr, target_kind, target_id, exec_kind, exec_name, state, body, data_mark, dataorigin, created_at, updated_at)
		VALUES ('dlv_files', 'demo', $1, 'task_result', 'sink:reports', 'sink', 'reports', 'sink', 'reports', 'planned', $2, 'internal', 'model', botaru_now(), botaru_now())`,
		task, map[string]any{"files": []any{html, png, csvPlain, csv}}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, sw, "delivery")
	if item == nil {
		t.Fatal("no delivery")
	}
	dh := header(t, item)
	for ref, ok := range map[string]bool{html: false, png: true, csvPlain: false, csv: true} {
		_, err := b.c.FileGet(ctx, sw, dh, FileGetRequest{FileRef: ref})
		if ok != (err == nil) || !ok && !strings.Contains(err.Error(), "media_type_not_allowed") {
			t.Fatalf("file %s: %v", ref, err)
		}
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, task).Scan(&state); err != nil || state != "working" {
		t.Fatalf("the task changed: %q %v", state, err)
	}

	// The lint: active types and a PDF of an agent type.
	ms, at := helloSet(t)
	ms[at["TaskType"]]["spec"].(map[string]any)["result_media_types"] = []any{"text/html", "image/svg+xml", "application/xhtml+xml", "application/pdf", "text/csv"}
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat("8", 40), Manifests: ms, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range res.Errors {
		if e.Code == "media_type_not_allowed" {
			n++
		}
	}
	if n != 4 {
		t.Fatalf("lint %+v", res.Errors)
	}
	ms[at["TaskType"]]["spec"].(map[string]any)["worker_kind"] = "job"
	ms[at["TaskType"]]["spec"].(map[string]any)["result_media_types"] = []any{"application/pdf"}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat("8", 40), Manifests: ms, DryRun: true}); err != nil || hasCode(res, "media_type_not_allowed") {
		t.Fatalf("a job type with a PDF: %+v %v", res, err)
	}
}

func hasCode(res *ApplyResult, code string) bool {
	for _, e := range res.Errors {
		if e.Code == code {
			return true
		}
	}
	return false
}

// A read tool with cache_s answers the same call from the cache after the full check, with the
// flags of the first call, untrusted_seen and the flag cached on the call, audit and usage; a
// no_tool hold and an erasure request of the tenant purge the cache (PRM-12).
func TestToolOutputCache(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "ToolPlugin" && m["metadata"].(map[string]any)["name"] == "checks" {
				m["spec"].(map[string]any)["tools"].([]any)[0].(map[string]any)["cache_s"] = 60
			}
		}
		return ms
	})
	th := b.toolhost(t, "checks")
	args := map[string]any{"check": "www-health"}
	out := map[string]any{"content": []any{}, "structuredContent": map[string]any{"check": "www-health", "ok": 3, "total": 3}}
	viaHost := func(h string) {
		t.Helper()
		done := callAsync(b, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
		item := b.claimTool(t, th)
		if _, err := b.c.Complete(ctx, th, header(t, item), CompleteRequest{Output: out}); err != nil {
			t.Fatal(err)
		}
		if res := <-done; res.err != nil || !strings.Contains(callText(res.r), `"ok":3`) {
			t.Fatalf("call %+v %v", res.r, res.err)
		}
	}
	cached := func(h string) bool {
		t.Helper()
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
		if err != nil {
			t.Fatal(err)
		}
		var c bool
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT cached FROM toolcalls ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&c); err != nil {
			t.Fatal(err)
		}
		if c && !strings.Contains(callText(r), `"ok":3`) {
			t.Fatalf("cached answer %+v", r)
		}
		return c
	}
	att, h := b.attemptTools(t, "1", false)
	viaHost(h)
	// The first call's flags are kept with the entry.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tool_cache SET personal_seen = '["masking"]'`); err != nil {
		t.Fatal(err)
	}
	if !cached(h) {
		t.Fatal("the second call did not come from the cache")
	}
	var rec string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record::text FROM tasks WHERE id = $1`, att.WorkID).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec, `"untrusted_seen": true`) || !strings.Contains(rec, `"masking"`) {
		t.Fatalf("task record %s", rec)
	}
	var audits, lines int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->'after'->>'cached' = 'true'`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM spend WHERE cached`).Scan(&lines); err != nil {
		t.Fatal(err)
	}
	if audits != 1 || lines != 1 {
		t.Fatalf("cached audit %d, usage %d", audits, lines)
	}
	// A no_tool hold on the plugin purges the entry; after its release the call reaches the
	// toolhost again.
	hold, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Op: "hold.set", Scope: HoldScope{Kind: "plugin", Tenant: "demo", Name: "checks"}, Effects: []string{"no_tool"}, Reason: "maintenance"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{Op: "hold.release", ID: hold.ID}); err != nil {
		t.Fatal(err)
	}
	viaHost(h)
	if !cached(h) {
		t.Fatal("the refilled entry was not used")
	}
	if _, err := b.c.ErasureRequest(ctx, b.op, ErasureRequestInput{Scope: ErasureScope{Tenant: "demo", Subject: b.alice}, Reason: "subject_request"}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tool_cache`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("cache after the erasure request: %d %v", n, err)
	}
}

// At call time an agent role does not read a dataset with model_exposure none, and reads one
// with aggregates only through a tool of numbers and enum values (PRV-4). The lint refuses the
// none case at apply, so the catalog in memory is changed after it to reach the call check.
func TestModelExposureCall(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Catalog" {
				for _, r := range m["spec"].(map[string]any)["resources"].([]any) {
					if rm := r.(map[string]any); rm["id"] == "web-checks" {
						rm["kind"] = "dataset" // aggregates by default
					}
				}
			}
		}
		return ms
	})
	_, h := b.attemptTools(t, "1", false)
	call := func() string {
		r, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "checks_latest", Arguments: map[string]any{"check": "www-health"}})
		if err != nil {
			t.Fatal(err)
		}
		return callText(r)
	}
	// Without a toolhost an allowed call ends with plugin_unavailable, after the checks.
	if got := call(); got != "error: plugin_unavailable" {
		t.Fatalf("aggregates through a trusted tool: %q", got)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := tenantCatalog(cat, "demo")
	for _, r := range spec["resources"].([]any) {
		if rm := r.(map[string]any); rm["id"] == "web-checks" {
			rm["model_exposure"] = "none"
		}
	}
	if got := call(); got != noPermission {
		t.Fatalf("none: %q", got)
	}
}
