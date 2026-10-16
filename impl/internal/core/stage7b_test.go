// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/texts"
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

// newProductionCore is a core outside the test bench, where a tenant may be of kind production.
func newProductionCore(t *testing.T) *Core {
	t.Helper()
	ctx := context.Background()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := texts.Load(root, "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(ctx, pgtest.DSN(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(st.Close)
	if err := st.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := New(ctx, Config{TokenKey: []byte("0123456789abcdef0123456789abcdef"), Offline: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}, st, sp, tx)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// apply of a production tenant does not enable a tier 1 role without an eval report with zero
// security failures for its static fingerprint; a test tenant gets a warning; a role whose
// fingerprint did not change stays enabled; doctor warns eval_report_stale (PRV-3).
func TestEvalReportGate(t *testing.T) {
	ctx := context.Background()
	c := newProductionCore(t)
	op := Operator{Principal: ids.PrincipalID(), Tenant: "farm"}
	ms, at := helloSet(t)
	ms[at["Tenant"]]["spec"].(map[string]any)["kind"] = "production"
	ms[at["TaskType"]]["spec"].(map[string]any)["tier"] = 1
	reports := func(fp string, sec int) map[string]any {
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": map[string]any{"name": "index"},
			"spec": map[string]any{"reports": []any{map[string]any{"subject": map[string]any{"kind": "role", "name": "hello"}, "fingerprint": fp,
				"suite": "hello", "date": "2026-10-01", "cases": 10, "failures": 0, "security_failures": sec}}}}
	}
	apply := func(set []map[string]any, dry bool) *ApplyResult {
		t.Helper()
		res, err := c.Apply(ctx, op, ApplyRequest{Commit: strings.Repeat("9", 40), Manifests: set, DryRun: dry})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := apply(ms, true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("no report: %+v", res.Errors)
	}
	var docs []manifest.Doc
	for i, m := range ms {
		docs = append(docs, manifest.FromValue(m, fmt.Sprintf("manifests/%d", i)))
	}
	fp, ok, err := newEvalIndex(docs).staticFingerprint("hello", "demo")
	if err != nil || !ok {
		t.Fatal(err)
	}
	if res := apply(append(slices.Clone(ms), reports(fp, 1)), true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("a report with a security failure counted: %+v", res.Errors)
	}
	if res := apply(append(slices.Clone(ms), reports(fp, 0)), false); !res.Applied {
		t.Fatalf("with a report: %+v", res.Errors)
	}
	// The role did not change: an index without its entry does not take it back.
	other := fp[:len(fp)-1] + "0"
	if strings.HasSuffix(fp, "0") {
		other = fp[:len(fp)-1] + "1"
	}
	if res := apply(append(slices.Clone(ms), reports(other, 0)), false); !res.Applied {
		t.Fatalf("an unchanged role: %+v", res.Errors)
	}
	d, err := c.Doctor(ctx, Operator{Principal: op.Principal, Tenant: "demo"}, TenantRequest{Tenant: "demo"})
	if err != nil {
		t.Fatal(err)
	}
	stale := false
	for _, ch := range d.Checks {
		if ch.ID == "eval_report_stale" && !ch.OK && slices.Equal(ch.Value.([]string), []string{"hello"}) {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("doctor %+v", d.Checks)
	}
	// A change of a part gives a new fingerprint without a report.
	ms[at["TaskType"]]["spec"].(map[string]any)["instruction"] = "Greet the person in one short line."
	if res := apply(append(slices.Clone(ms), reports(fp, 0)), true); !hasCode(res, "eval_report_missing") {
		t.Fatalf("a changed type: %+v", res.Errors)
	}
	// In a test tenant the same apply passes with a warning.
	ms[at["Tenant"]]["spec"].(map[string]any)["kind"] = "test"
	if res := apply(append(slices.Clone(ms), reports(fp, 0)), true); len(res.Errors) != 0 || len(res.Warnings) != 1 || res.Warnings[0].Code != "eval_report_missing" {
		t.Fatalf("a test tenant: %+v", res)
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

// worker.drain with a deadline: before it the heartbeat answer asks nothing, from it the answer
// asks for release, and release does not spend the attempt; the expired lease of a draining
// worker that stays silent past its deadline is not spent either (LCY-4).
func TestWorkerDrainDeadline(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	h := header(t, att)
	at := func(s int) string {
		var now time.Time
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now); err != nil {
			t.Fatal(err)
		}
		return fmtTime(now.Add(time.Duration(s) * time.Second))
	}
	if _, err := b.c.WorkerDrain(ctx, b.op, WorkerRequest{Op: "worker.drain", Worker: b.agent.ID, Deadline: at(120)}); err != nil {
		t.Fatal(err)
	}
	for _, step := range []struct {
		advance int
		release bool
	}{{50, false}, {50, false}, {30, true}} {
		b.advance(t, step.advance)
		hb, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{})
		if err != nil || hb.ReleaseRequested != step.release {
			t.Fatalf("heartbeat %+v %v", hb, err)
		}
	}
	if _, err := b.c.Release(ctx, b.agent, h); err != nil {
		t.Fatal(err)
	}
	spent := func(id string) int {
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT attempts_spent FROM tasks WHERE id = $1`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if spent(task) != 0 {
		t.Fatal("the released attempt was spent")
	}
	// Another worker takes the task; it drains with a deadline and stays silent.
	other := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if att := b.claim(t, other, "attempt"); att == nil || att.WorkID != task {
		t.Fatalf("the other worker got %+v", att)
	}
	if _, err := b.c.WorkerDrain(ctx, b.op, WorkerRequest{Op: "worker.drain", Worker: other.ID, Deadline: at(10)}); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 200)
	if err := b.c.tickLoop(ctx, "reaper"); err != nil {
		t.Fatal(err)
	}
	var reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason FROM leases WHERE work_id = $1 ORDER BY lease_no DESC LIMIT 1`, task).Scan(&reason); err != nil || reason != "worker_drain" {
		t.Fatalf("lease reason %q %v", reason, err)
	}
	if spent(task) != 0 {
		t.Fatal("the expired lease of a drained worker was spent")
	}
}

// worker.revoke of a worker whose runtime declared session: runtime_internal records an erasure
// request with the worker as its scope and the reason worker_revoke; the runtime never ran an
// attempt here, so its step closes not_applicable (LCY-4).
func TestWorkerRevokeRuntimePurge(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	r, err := b.c.Enroll(ctx, b.op, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	w, err := b.c.AuthenticateWorker(ctx, r.Token)
	if err != nil {
		t.Fatal(err)
	}
	// This core blocks a runtime_internal runtime at hello (RT-5); the declaration stays.
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"},
		Runtime: &RuntimeDecl{Session: "runtime_internal", Erasure: []string{"erase", "match"}}}); err == nil {
		t.Fatal("a runtime_internal runtime passed hello")
	}
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: w.ID}); err != nil {
		t.Fatal(err)
	}
	var id, scope, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, scope->>'worker', reason FROM erasure_requests WHERE tenant = 'demo'`).Scan(&id, &scope, &reason); err != nil {
		t.Fatal(err)
	}
	if scope != w.ID || reason != "worker_revoke" {
		t.Fatalf("erasure %s %s", scope, reason)
	}
	if err := b.c.tickLoop(ctx, "erasure"); err != nil {
		t.Fatal(err)
	}
	var state, basis string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(basis, '') FROM erasure_steps WHERE request = $1 AND kind = 'runtime'`, id).Scan(&state, &basis); err != nil || state != "not_applicable" || basis != "tenant_absent" {
		t.Fatalf("runtime step %s %s %v", state, basis, err)
	}
	// A worker without such a runtime gets no erasure request at revoke.
	if _, err := b.c.WorkerRevoke(ctx, b.op, WorkerRequest{Op: "worker.revoke", Worker: b.agent.ID}); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM erasure_requests`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("requests %d %v", n, err)
	}
}
