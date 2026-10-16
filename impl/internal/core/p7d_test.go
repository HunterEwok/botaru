// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// lintFound runs the set lint over YAML manifests against the stored YAML manifests prior and
// returns "code path" of each finding.
func lintFound(t *testing.T, c *Core, docs, prior string) []string {
	t.Helper()
	parse := func(s string) []manifest.Doc {
		if s == "" {
			return nil
		}
		ds, err := manifest.ParseYAML(strings.NewReader(s), "test")
		if err != nil {
			t.Fatal(err)
		}
		return ds
	}
	ds := parse(docs)
	index := make([]int, len(ds))
	for i := range ds {
		index[i] = i
		if ds[i].Kind == "Role" || ds[i].Kind == "ModelProfile" {
			ds[i].Tenant = ""
		}
	}
	errs, err := c.lintManifests(ds, index, nil, parse(prior))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range errs {
		out = append(out, e.Code+" "+e.Path)
	}
	return out
}

const p7dControl = "controller: {name: Example Org, contact: privacy@example.org, agreement_ref: contracts/demo}"

func p7dTenant(kind, privacy string, extra ...string) string {
	return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: demo}\nspec: {kind: " + kind + ", farm_manager: {enabled: false}, budgets: {month_eur: 60}" +
		strings.Join(extra, "") + ", privacy: {" + privacy + "}}\n---\n"
}

// SEC-8: apply refuses a production tenant with an empty field of the privacy block (an
// explicit none is allowed for dpia_ref), and any tenant whose providers_allowed names a
// provider that is not a subprocessor.
func TestPrivacyBlockRequired(t *testing.T) {
	c := lintCore(t)
	full := "purposes: [{purpose: operations}], " + p7dControl + ", recipients: [messenger, monitoring], notice_ref: privacy/demo, notice_version: 1, dpia_ref: none, ropa_ref: ropa/demo"
	if got := lintFound(t, c, p7dTenant("production", full), ""); len(got) != 0 {
		t.Fatalf("SEC-8: a full block with dpia_ref none: %v", got)
	}
	partial := "purposes: [{purpose: operations}], controller: {name: Example Org}, recipients: [messenger, monitoring], notice_version: 1, dpia_ref: none"
	got := lintFound(t, c, p7dTenant("production", partial), "")
	for _, want := range []string{"privacy_incomplete /manifests/0/spec/privacy/controller/contact", "privacy_incomplete /manifests/0/spec/privacy/notice_ref",
		"privacy_incomplete /manifests/0/spec/privacy/ropa_ref"} {
		if !slices.Contains(got, want) {
			t.Errorf("SEC-8: %s missing from %v", want, got)
		}
	}
	// A tenant without kind is production; a test tenant may leave the block partial.
	if got := lintFound(t, c, strings.Replace(p7dTenant("production", partial), "kind: production, ", "", 1), ""); !slices.Contains(got, "privacy_incomplete /manifests/0/spec/privacy/ropa_ref") {
		t.Errorf("SEC-8: a tenant without kind %v", got)
	}
	if got := lintFound(t, c, p7dTenant("test", partial), ""); len(got) != 0 {
		t.Errorf("SEC-8: a test tenant %v", got)
	}
	// A production tenant with an allowed provider names its subprocessors in full.
	sub := full + ", subprocessors: [{provider: provider-a, agreement_ref: contracts/a}]"
	got = lintFound(t, c, p7dTenant("production", sub, ", providers_allowed: [provider-a]"), "")
	if !slices.Contains(got, "privacy_incomplete /manifests/0/spec/privacy/subprocessors/0/region") || !slices.Contains(got, "privacy_incomplete /manifests/0/spec/privacy/subprocessors/0/retention") {
		t.Errorf("SEC-8: a subprocessor without region and retention %v", got)
	}
	// Every allowed provider of any tenant is a subprocessor.
	got = lintFound(t, c, p7dTenant("test", "recipients: [monitoring], subprocessors: [{provider: provider-a}]", ", providers_allowed: [provider-a, provider-b]"), "")
	if !slices.Equal(got, []string{"subprocessor_undeclared /manifests/0/spec/providers_allowed/1"}) {
		t.Errorf("SEC-8: providers_allowed outside the subprocessors %v", got)
	}
}

// SEC-13: a change of controller, purposes, subprocessors or recipients without a higher
// notice_version fails apply; another field may change without it.
func TestNoticeVersionMustGrow(t *testing.T) {
	c := lintCore(t)
	block := func(recipients string, version int, ropa string) string {
		return p7dTenant("test", "recipients: ["+recipients+"], notice_version: "+string(rune('0'+version))+", ropa_ref: "+ropa)
	}
	stored := block("messenger, monitoring", 2, "ropa/a")
	if got := lintFound(t, c, block("model_provider, messenger, monitoring", 2, "ropa/a"), stored); !slices.Equal(got, []string{"notice_version_not_raised /manifests/0/spec/privacy/notice_version"}) {
		t.Fatalf("SEC-13: recipients changed at the same version %v", got)
	}
	if got := lintFound(t, c, block("model_provider, messenger, monitoring", 1, "ropa/a"), stored); len(got) != 1 {
		t.Fatalf("SEC-13: a lower version %v", got)
	}
	if got := lintFound(t, c, block("model_provider, messenger, monitoring", 3, "ropa/a"), stored); len(got) != 0 {
		t.Fatalf("SEC-13: a higher version %v", got)
	}
	if got := lintFound(t, c, block("messenger, monitoring", 2, "ropa/b"), stored); len(got) != 0 {
		t.Fatalf("SEC-13: a field outside the four %v", got)
	}
	// A new tenant has no notice to raise.
	if got := lintFound(t, c, block("messenger", 1, "ropa/a"), ""); len(got) != 0 {
		t.Fatalf("SEC-13: a new tenant %v", got)
	}
}

// GW-14: a profile whose provider binding does not list the subprocessor region fails apply for
// the tenant; a provider without a binding note (the test providers) has no table.
func TestRegionFromBindingTable(t *testing.T) {
	c := lintCore(t)
	set := func(provider, region string) string {
		return p7dTenant("test", "recipients: [monitoring], subprocessors: [{provider: "+provider+", agreement_ref: contracts/x, region: "+region+", retention: {mode: zero}}]",
			", models_allowed: [fx], providers_allowed: ["+provider+"]") +
			"apiVersion: botaru/v1alpha1\nkind: ModelProfile\nmetadata: {name: fx}\nspec: {provider: " + provider + ", model: m-1, family: family-a, rank: 1, max_output_tokens: 8000, tokens_per_s: 60, " +
			"price: {input: 1.0, output: 4.0, checked: '2026-10-01'}, provider_training: {value: false, checked: '2026-10-01'}, provider_human_review: {value: abuse_only, checked: '2026-10-01'}}\n"
	}
	if got := lintFound(t, c, set("anthropic", "eu-central"), ""); !slices.Contains(got, "region_unsupported /manifests/0/spec/models_allowed/0") {
		t.Fatalf("GW-14: a region outside the table %v", got)
	}
	for _, ok := range [][2]string{{"anthropic", "us"}, {"anthropic", "global"}, {"provider-a", "region-a"}} {
		if got := lintFound(t, c, set(ok[0], ok[1]), ""); slices.ContainsFunc(got, func(s string) bool { return strings.HasPrefix(s, "region_unsupported") }) {
			t.Fatalf("GW-14: %v %v", ok, got)
		}
	}
}

// MEM-2: from v0.2, while the personal assistant is deferred, apply refuses a role with memory:
// person (person_memory_deferred); a core that claims v0.1 keeps it.
func TestPersonMemoryDeferred(t *testing.T) {
	c := lintCore(t)
	role := "apiVersion: botaru/v1alpha1\nkind: Role\nmetadata: {name: assistant}\nspec: {worker_kind: agent, memory: person, models: {base: base-std}}\n"
	has := func() bool {
		return slices.ContainsFunc(lintFound(t, c, role, ""), func(s string) bool { return strings.HasPrefix(s, "person_memory_deferred ") })
	}
	if has() {
		t.Fatal("MEM-2: refused on a core that claims v0.1")
	}
	setClaimsV03(t, true)
	if !has() {
		t.Fatal("MEM-2: memory: person passed on a core that claims v0.3")
	}
}

// SEC-8: an apply that changes the privacy block gives the event tenant.privacy_changed with the
// names of the changed fields and a code notice to each admin (admin:people); an apply that
// keeps the block gives neither.
func TestPrivacyChangeNotice(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	bob := b.withAdmins(t)
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	set := func(version int, recipients []any) []map[string]any {
		ms, err := testbench.HelloBotManifests(root)
		if err != nil {
			t.Fatal(err)
		}
		ms = append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Group",
			"metadata": map[string]any{"name": "admins", "tenant": "demo"},
			"spec":     map[string]any{"caps": []any{"admin:people", "hold.release", "hold.set:plugin"}, "data": []any{"summary"}},
		})
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				p := m["spec"].(map[string]any)["privacy"].(map[string]any)
				p["notice_version"] = version
				if recipients != nil {
					p["recipients"] = recipients
				}
			}
		}
		return ms
	}
	apply := func(commit string, ms []map[string]any) {
		t.Helper()
		res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat(commit, 40), Manifests: ms})
		if err != nil || !res.Applied {
			t.Fatalf("apply %+v %v", res, err)
		}
	}
	notices := func() int {
		return b.count(t, `SELECT count(*) FROM deliveries WHERE tenant = 'demo' AND kind = 'notice' AND recipient = $1 AND body->>'text_key' = 'notice.privacy_changed'`, bob)
	}
	before := countEvents(t, b, "tenant.privacy_changed", "demo")
	apply("2", set(7, []any{"model_provider", "messenger", "monitoring"}))
	if n := countEvents(t, b, "tenant.privacy_changed", "demo"); n != before+1 {
		t.Fatalf("SEC-8: %d events after a change", n-before)
	}
	var fields, version string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data->>'fields', data->>'notice_version' FROM events WHERE type = 'tenant.privacy_changed' ORDER BY time DESC, id DESC LIMIT 1`).Scan(&fields, &version); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fields, "notice_version") || version != "7" {
		t.Fatalf("SEC-8: event fields %s version %s", fields, version)
	}
	if n := notices(); n != 1 {
		t.Fatalf("SEC-8: %d notices to the admin", n)
	}
	var params string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body->'params'->>'fields' FROM deliveries WHERE body->>'text_key' = 'notice.privacy_changed'`).Scan(&params); err != nil || !strings.Contains(params, "notice_version") {
		t.Fatalf("SEC-8: notice params %q %v", params, err)
	}
	// The same block again: nothing new.
	apply("3", set(7, []any{"model_provider", "messenger", "monitoring"}))
	if n := countEvents(t, b, "tenant.privacy_changed", "demo"); n != before+1 || notices() != 1 {
		t.Fatalf("SEC-8: an unchanged block gave an event or a notice: %d events, %d notices", n-before, notices())
	}
	// SEC-13 at apply: recipients change at the same version is refused.
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat("4", 40), Manifests: set(7, []any{"messenger", "monitoring"})})
	if err != nil || res.Applied || !slices.ContainsFunc(res.Errors, func(e ApplyError) bool { return e.Code == "notice_version_not_raised" }) {
		t.Fatalf("SEC-13: %+v %v", res, err)
	}
}

// MEM-9: the core cannot seal a runtime_internal session, so retiring the identity enqueues in
// the same transaction an erasure request with reason bot_retired for its worker, whose one
// step is the E4 purge of that runtime; the runtime claims it and closes the request, and the
// sealed identity stays sealed until its own request at the end of seal_ttl.
func TestRetireRuntimeInternalPurge(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE workers SET hello = jsonb_set(coalesce(hello, '{}'), '{runtime}', '{"session": "runtime_internal", "memory": "core"}') WHERE id = $1`, b.agent.ID); err != nil {
		t.Fatal(err)
	}
	old := b.botOf(t, b.agent.ID)
	if _, err := b.c.BotRetire(ctx, b.op, BotRetireRequest{Bot: old.ID}); err != nil {
		t.Fatal(err)
	}
	var req, state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, state FROM erasure_requests WHERE reason = 'bot_retired' AND scope->>'worker' = $1`, b.agent.ID).Scan(&req, &state); err != nil {
		t.Fatalf("MEM-9: no bot_retired request for the runtime: %v", err)
	}
	if n := b.count(t, `SELECT count(*) FROM erasure_steps WHERE request = $1`, req); n != 1 {
		t.Fatalf("MEM-9: %d steps", n)
	}
	if n := b.count(t, `SELECT count(*) FROM erasure_steps WHERE request = $1 AND kind = 'runtime' AND target = $2`, req, b.agent.ID); n != 1 {
		t.Fatal("MEM-9: the step is not the purge of the worker's runtime")
	}
	b.loop(t, "erasure")
	w := b.claim(t, b.agent, "erasure")
	if w == nil {
		t.Fatal("MEM-9: the runtime got no purge")
	}
	if w.Payload["op"] != "erase" || w.Payload["reason"] != "bot_retired" || w.Payload["target"].(map[string]any)["worker_id"] != b.agent.ID {
		t.Fatalf("MEM-9: purge payload %v", w.Payload)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, w), CompleteRequest{Erasure: map[string]any{"outcome": "done"}}); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "erasure")
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM erasure_requests WHERE id = $1`, req).Scan(&state); err != nil || state != "done" {
		t.Fatalf("MEM-9: request %s %v", state, err)
	}
	var botState string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM bots WHERE id = $1`, old.ID).Scan(&botState); err != nil || botState != "retired" {
		t.Fatalf("MEM-9: the purge erased the sealed identity: %s %v", botState, err)
	}
	// A runtime without runtime_internal gets no such request.
	b2 := newBench(t)
	o2 := b2.botOf(t, b2.agent.ID)
	if _, err := b2.c.BotRetire(ctx, b2.op, BotRetireRequest{Bot: o2.ID}); err != nil {
		t.Fatal(err)
	}
	if n := b2.count(t, `SELECT count(*) FROM erasure_requests WHERE reason = 'bot_retired'`); n != 0 {
		t.Fatalf("MEM-9: %d requests for a runtime without its own store", n)
	}
}

// FEN-7: the service_grant record whose number a bot's write call carries names, beside the
// bot as author, the initiator whose own right service.control:<service> the run stands on
// (D-73); a person's own call names the person.
func TestServiceGrantInitiator(t *testing.T) {
	ctx := context.Background()
	b := newServiceBench(t)
	_, h := b.attemptTools(t, "2", true)
	ch := callAsync(b.bench, h, mcp.CallParams{Name: serviceToolName, Arguments: map[string]any{"service": "nginx", "verb": "restart"}})
	item := b.claimTool(t, b.svc)
	if _, err := b.c.Complete(ctx, b.svc, header(t, item), CompleteRequest{Output: map[string]any{"content": []any{}, "structuredContent": map[string]any{"state": "active"}}}); err != nil {
		t.Fatal(err)
	}
	if got := <-ch; got.err != nil {
		t.Fatal(got.err)
	}
	gs := b.grants(t)
	if len(gs) != 1 {
		t.Fatalf("grants %v", gs)
	}
	g := gs[0]["service_grant"].(map[string]any)
	if g["author"].(map[string]any)["kind"] != "bot" || g["initiator"] != b.alice || gs[0]["target"].(map[string]any)["id"] != item.Payload["grant_id"] {
		t.Fatalf("FEN-7: grant %v, write call %v", g, item.Payload["grant_id"])
	}
	validOut(t, b.bench, "audit-record", gs[0])
}

// DLV-11: progress is only a stage from the closed list of the type's stages: a heartbeat with
// another stage is refused, one from the list extends the lease.
func TestHeartbeatStageFromList(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" {
				m["spec"].(map[string]any)["stages"] = []any{"reading_input", "writing_answer"}
			}
		}
		return ms
	})
	task := b.hello(t, "1")
	att := claimFor(t, b, b.agent, task)
	h := header(t, att)
	p := 50.0
	if _, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{Stage: "writing_answer", Progress: &p}); err != nil {
		t.Fatalf("DLV-11: a stage of the list %v", err)
	}
	_, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{Stage: "Here is the first half of the answer", Progress: &p})
	wantReason(t, err, "input_schema_invalid")
	if _, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{}); err != nil {
		t.Fatalf("DLV-11: a heartbeat without a stage %v", err)
	}
}

// MON-4, OBS-11: the adapter's account report decides what is wider than the rule allows, by
// the catalogs and the farm node list: the right to acknowledge problems, a demo account that
// sees a demo2 node or a farm node, a node no catalog of demo links, a script on core1; the farm
// account sees the farm nodes but no tenant node.
func TestAccountWider(t *testing.T) {
	hosts := func(nodes ...string) map[string]any {
		var hs []any
		for i, n := range nodes {
			hs = append(hs, map[string]any{"id": "h" + string(rune('1'+i)), "monitor_host": n})
		}
		return map[string]any{"spec": map[string]any{"hosts": hs}}
	}
	cat := &Catalog{
		Tenants: map[string]*TenantView{"demo": {Name: "demo"}, "demo2": {Name: "demo2"}, "farm": {Name: "farm"}},
		RawGlobals: map[manifest.Key]map[string]any{
			{Kind: "Catalog", Tenant: "demo", Name: "demo"}:   hosts("web1.example.org"),
			{Kind: "Catalog", Tenant: "demo2", Name: "demo2"}: hosts("web2.example.org"),
		},
		Hashes: map[manifest.Key]string{},
	}
	farm := []string{"core1", "backup1"}
	h := nodeHash
	if got := accountWider(cat, farm, "demo", accountReport{Visible: []string{h("web1.example.org")}, Scripts: []string{h("web1.example.org")}}); len(got) != 0 {
		t.Fatalf("MON-4: a narrow demo account %v", got)
	}
	got := accountWider(cat, farm, "demo", accountReport{Findings: []string{"acknowledge_problems"},
		Visible: []string{h("web1.example.org"), h("web2.example.org"), h("core1"), h("db9.example.org")}, Scripts: []string{h("core1")}})
	if want := []string{"acknowledge_problems", "sees:core1", "sees:web2.example.org", "sees_unlinked:1", "script:core1"}; !slices.Equal(got, want) {
		t.Fatalf("MON-4: a wide demo account %v", got)
	}
	if got := accountWider(cat, farm, "farm", accountReport{Visible: []string{h("core1"), h("backup1")}}); len(got) != 0 {
		t.Fatalf("MON-4: a narrow farm account %v", got)
	}
	if got := accountWider(cat, farm, "farm", accountReport{Visible: []string{h("core1"), h("web2.example.org")}, Scripts: []string{h("backup1")}, Truncated: true}); !slices.Equal(got, []string{"sees:web2.example.org", "script:backup1", "truncated"}) {
		t.Fatalf("MON-4: a wide farm account %v", got)
	}
}

// OBS-11: doctor shows per tenant whether its adapter account sees only what MON-4 allows: a
// warning while no report is younger than an hour, a failure on a wider account, a pass on a
// narrow one; the adapter's report arrives as ext.monitor.account.v1.
func TestDoctorAccountScope(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	mon := b.monitorOn(t)
	b.c.cfg.FarmHosts = []string{"core1", "backup1"}
	line := func() DoctorCheck {
		t.Helper()
		d, err := b.c.Doctor(ctx, b.op, TenantRequest{})
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range d.Checks {
			if ch.ID == "monitor.account_scope" && ch.Tenant == "demo" {
				return ch
			}
		}
		t.Fatalf("no account line in %v", d.Checks)
		return DoctorCheck{}
	}
	if ch := line(); ch.OK || ch.Severity != "warn" || ch.Value != "unchecked" {
		t.Fatalf("OBS-11: no report yet %+v", ch)
	}
	report := func(findings []any, visible ...string) {
		t.Helper()
		hs := []any{}
		for _, v := range visible {
			hs = append(hs, nodeHash(v))
		}
		a := Activity{Kind: "signal", Channel: "zabbix", UpdateID: "account:" + strings.Join(visible, ","), Chat: Chat{Kind: "direct", ExternalID: "monitor"},
			Event: &SignalEvent{Type: "ext.monitor.account.v1", Data: map[string]any{"findings": findings, "visible": hs, "scripts": []any{}}}}
		a.Sender.Identity, a.Sender.Verified = "hook:monitor", true
		if _, err := b.c.SubmitActivity(ctx, mon, a); err != nil {
			t.Fatal(err)
		}
	}
	report([]any{"acknowledge_problems"}, "core1")
	if ch := line(); ch.OK || ch.Severity != "fail" || ch.Value != "acknowledge_problems, sees:core1" {
		t.Fatalf("MON-4: a wide account %+v", ch)
	}
	if n := countEvents(t, b, "ext.monitor.account.v1", "demo"); n != 1 {
		t.Fatalf("%d account events", n)
	}
	report([]any{})
	if ch := line(); !ch.OK || ch.Severity != "info" {
		t.Fatalf("MON-4: a narrow account %+v", ch)
	}
	b.advance(t, 2*3600)
	if ch := line(); ch.OK || ch.Value != "unchecked" {
		t.Fatalf("OBS-11: a stale report %+v", ch)
	}
	// A report outside its schema is refused.
	a := Activity{Kind: "signal", Channel: "zabbix", UpdateID: "account:bad", Chat: Chat{Kind: "direct", ExternalID: "monitor"},
		Event: &SignalEvent{Type: "ext.monitor.account.v1", Data: map[string]any{"findings": []any{"Admin " + "role"}, "visible": []any{}, "scripts": []any{}}}}
	a.Sender.Identity, a.Sender.Verified = "hook:monitor", true
	_, err := b.c.SubmitActivity(ctx, mon, a)
	wantReason(t, err, "input_schema_invalid")
}
