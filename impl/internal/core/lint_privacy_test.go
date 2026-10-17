// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// Tests of the privacy lint of a tenant: the privacy block, the notice version and regions.

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

// SEC-8: an apply that changes the privacy block gives the event tenant.privacy_changed.v1 with the
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
	before := countEvents(t, b, "tenant.privacy_changed.v1", "demo")
	apply("2", set(7, []any{"model_provider", "messenger", "monitoring"}))
	if n := countEvents(t, b, "tenant.privacy_changed.v1", "demo"); n != before+1 {
		t.Fatalf("SEC-8: %d events after a change", n-before)
	}
	var fields, version string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data->>'fields', data->>'notice_version' FROM events WHERE type = 'tenant.privacy_changed.v1' ORDER BY time DESC, id DESC LIMIT 1`).Scan(&fields, &version); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fields, "notice_version") || version != "7" {
		t.Fatalf("SEC-8: event fields %s version %s", fields, version)
	}
	// The event is the one of the events catalog, with its data schema.
	reg, err := b.c.spec.Registry("spec/bindings/events/catalog.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := reg.Entry("tenant.privacy_changed.v1"); !ok || e["data_schema"] != "spec/model/event-data/privacy-event.schema.json" {
		t.Fatalf("SEC-8: the event is not in the catalog: %v", e)
	}
	var evData map[string]any
	var evRaw []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'tenant.privacy_changed.v1' ORDER BY time DESC, id DESC LIMIT 1`).Scan(&evRaw); err != nil {
		t.Fatal(err)
	}
	_ = json.Unmarshal(evRaw, &evData)
	validOut(t, b, "privacy-event", evData)
	if n := notices(); n != 1 {
		t.Fatalf("SEC-8: %d notices to the admin", n)
	}
	var params string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body->'params'->>'fields' FROM deliveries WHERE body->>'text_key' = 'notice.privacy_changed'`).Scan(&params); err != nil || !strings.Contains(params, "notice_version") {
		t.Fatalf("SEC-8: notice params %q %v", params, err)
	}
	// The same block again: nothing new.
	apply("3", set(7, []any{"model_provider", "messenger", "monitoring"}))
	if n := countEvents(t, b, "tenant.privacy_changed.v1", "demo"); n != before+1 || notices() != 1 {
		t.Fatalf("SEC-8: an unchanged block gave an event or a notice: %d events, %d notices", n-before, notices())
	}
	// SEC-13 at apply: recipients change at the same version is refused.
	res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat("4", 40), Manifests: set(7, []any{"messenger", "monitoring"})})
	if err != nil || res.Applied || !slices.ContainsFunc(res.Errors, func(e ApplyError) bool { return e.Code == "notice_version_not_raised" }) {
		t.Fatalf("SEC-13: %+v %v", res, err)
	}
}
