// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// A runtime with model_attribution per_profile holds one attempt lease at a time: while its
// lease is live it gets no second attempt, so the profile's handle and usage belong to that
// lease (RT-3).
func TestPerProfileOneLease(t *testing.T) {
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
	if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"}, Capacity: 1,
		Runtime: &RuntimeDecl{Session: "farm", Memory: "farm", ModelAttribution: "per_profile"}}); err != nil {
		t.Fatal(err)
	}
	if w, err = b.c.AuthenticateWorker(ctx, r.Token); err != nil {
		t.Fatal(err)
	}
	b.agent = w
	b.helloAttempt(t, "1")
	b.hello(t, "2")
	b.drain(t)
	if item := b.claim(t, w, "attempt"); item != nil {
		t.Fatalf("a second attempt under a live lease: %+v", item)
	}
}

// A runtime that keeps sessions itself gets match work for a person's erase_match, by its
// worker id, with the needles and the request (RT-5, RET-11).
func TestRuntimeInternalMatch(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.exec(t, `UPDATE workers SET hello = jsonb_set(hello, '{runtime}', '{"session": "runtime_internal", "erasure": ["erase", "match"]}') WHERE id = $1`, b.agent.ID)
	if _, err := b.c.ErasureMatch(ctx, b.op, MatchRequest{Subject: "system", Needles: []string{"Mari Tamm"}}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, b.agent, "erasure")
	if item == nil || item.Payload["op"] != "match" {
		t.Fatalf("match work of the runtime %+v", item)
	}
	tgt := item.Payload["target"].(map[string]any)
	if tgt["kind"] != "runtime" || tgt["worker_id"] != b.agent.ID || !slices.Equal(item.Payload["needles"].([]string), []string{"Mari Tamm"}) {
		t.Fatalf("target %+v", item.Payload)
	}
	validOut(t, b, "erasure-work", item.Payload)
}

// A channel's last successful poll and P4's provider pair state show in doctor (OBS-10).
func TestDoctorUpstream(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	at := b.now(t).Add(-time.Minute).UTC().Format(time.RFC3339Nano)
	if _, err := b.c.Hello(ctx, b.channel, HelloRequest{Kind: "channel", ProtocolVersions: []string{"v1"},
		Health: []ComponentHealth{{Component: "channel_upstream", Name: "telegram", State: "ok", Time: at}}}); err != nil {
		t.Fatal(err)
	}
	b.exec(t, `INSERT INTO provider_pairs (tenant, pool, state, updated_at) VALUES ('demo', 'family-a', 'open', now())`)
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]DoctorCheck{}
	for _, ch := range doc.Checks {
		checks[ch.ID] = ch
	}
	if ch, ok := checks["channel.last_poll_age_s.telegram"]; !ok || !ch.OK || ch.Value.(int64) < 50 || ch.Value.(int64) > 120 {
		t.Fatalf("poll age %+v", ch)
	}
	if ch, ok := checks["provider.state.family_a"]; !ok || ch.OK || ch.Value != "open" || ch.Severity != "warn" {
		t.Fatalf("provider state %+v", ch)
	}
	if ch := checks["deploy.description"]; ch.Severity != "warn" {
		t.Fatalf("no deployment description %+v", ch)
	}
}

func lintDetails(t *testing.T, c *Core, docs string) map[string]string {
	t.Helper()
	ds, err := manifest.ParseYAML(strings.NewReader(docs), "test")
	if err != nil {
		t.Fatal(err)
	}
	index := make([]int, len(ds))
	for i := range ds {
		index[i] = i
		if ds[i].Kind == "Role" || ds[i].Kind == "TaskType" {
			ds[i].Tenant = ""
		}
	}
	errs, err := c.lintManifests(ds, index, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range errs {
		out[e.Code+" "+e.Name] = e.Detail
	}
	return out
}

// A tenant's month limit within the share of its workspace ceiling, the tenants together within
// the share of the organization ceiling, and a tenant without a described workspace is named
// (GW-9).
func TestLintWorkspaceCeilings(t *testing.T) {
	c := lintCore(t)
	c.cfg.Workspaces = WorkspaceCeilings{Providers: map[string]ProviderCeilings{
		"provider-a": {OrganizationEUR: 1000, Workspaces: map[string]float64{"demo": 100, "demo3": 1000}},
	}}
	tenant := func(name string, month int) string {
		return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: " + name + "}\nspec: {kind: test, farm_manager: {enabled: false}, budgets: {month_eur: " + strconv.Itoa(month) + "}}\n---\n"
	}
	got := lintDetails(t, c, tenant("demo", 90)+tenant("demo2", 10))
	if _, ok := got["workspace_ceiling_missing demo2"]; !ok || len(got) != 1 {
		t.Fatalf("within the ceiling %v", got)
	}
	got = lintDetails(t, c, tenant("demo", 91))
	if _, ok := got["workspace_ceiling_exceeded demo"]; !ok {
		t.Fatalf("above the workspace ceiling %v", got)
	}
	got = lintDetails(t, c, tenant("demo", 50)+tenant("demo3", 880))
	if _, ok := got["workspace_ceiling_exceeded demo"]; !ok {
		t.Fatalf("above the organization ceiling %v", got)
	}
}

// The review line covers max_per_week x 4.4 x the price COM-6 reserves for the reviews of
// one proposal, the lenses with an executor; a line below it is rejected with the missing
// amount, and without an executor of the veto lenses no review is reserved (BUD-13, COM-6).
func TestLintReviewLine(t *testing.T) {
	c := lintCore(t)
	var types []string
	for _, d := range c.lensDefs() {
		if d.Veto {
			types = append(types, d.ReviewType)
		}
	}
	var docs strings.Builder
	for _, ty := range types {
		docs.WriteString("apiVersion: botaru/v1alpha1\nkind: TaskType\nmetadata: {name: " + ty + "}\nspec: {tree_budget_eur: 0.06, purpose: review}\n---\n")
	}
	docs.WriteString("apiVersion: botaru/v1alpha1\nkind: Role\nmetadata: {name: analyst}\nspec: {insight: {max_per_week: 3}}\n---\n")
	reviewer := "apiVersion: botaru/v1alpha1\nkind: Role\nmetadata: {name: reviewer}\nspec: {profile: reviewer, claims: [" + strings.Join(types, ", ") + "]}\n---\n"
	tenant := func(review string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: demo}\nspec: {kind: test, budgets: {lines: {insight: {month_eur: 5}, review: {month_eur: " + review + "}}}}\n"
	}
	need := reviewNeed([]int{3}, int64(len(types))*60000)
	got := lintDetails(t, c, docs.String()+reviewer+tenant("0.5"))
	d, ok := got["review_line_too_small demo"]
	if !ok || !strings.Contains(d, eurCents(need)+" EUR a month, "+eurCents(need-500000)+" EUR more") {
		t.Fatalf("a short review line %v (need %d)", got, need)
	}
	if _, ok := lintDetails(t, c, docs.String()+reviewer+tenant("10"))["review_line_too_small demo"]; ok {
		t.Fatal("a covering review line rejected")
	}
	if _, ok := lintDetails(t, c, docs.String()+tenant("0.5"))["review_line_too_small demo"]; ok {
		t.Fatal("a review line rejected for lenses without an executor")
	}
	if reviewNeed([]int{3}, 240000) != 3168000 || eurCents(1168000) != "1.17" {
		t.Fatal("the formula")
	}
}

// The core refuses to start with a failing deployment description or one whose farm nodes
// differ from the settings, and doctor lints it again (DEP-10, DEP-11).
func TestDeployStartAndDoctor(t *testing.T) {
	dir := t.TempDir()
	good, bad := filepath.Join(dir, "good.yaml"), filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(good, []byte(deployBase), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte(strings.Replace(deployBase, "user: botaru-ch, ", "", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	checks := func(c *Core) map[string]DoctorCheck {
		out := map[string]DoctorCheck{}
		c.doctorDeploy(func(ch DoctorCheck) { out[ch.ID] = ch })
		return out
	}
	c := lintCore(t)
	c.cfg.DeployFile = bad
	if err := c.loadDeploy(); err == nil {
		t.Fatal("a failing description started")
	}
	if ch := checks(c); ch["deploy.lint"].OK || ch["deploy.process_user_missing"].Severity != "fail" {
		t.Fatalf("doctor %+v", ch)
	}
	c = lintCore(t, "core2")
	c.cfg.DeployFile = good
	if err := c.loadDeploy(); err == nil || !strings.Contains(err.Error(), "farm_hosts") {
		t.Fatalf("farm_hosts mismatch: %v", err)
	}
	// The core's secrets fail closed (SEC-6): one that is neither a provider key nor the core's
	// own stops the start, and so does a provider key the core does not hold.
	c = lintCore(t)
	c.cfg.DeployFile = good
	if err := c.loadDeploy(); err == nil || !strings.Contains(err.Error(), "store-core") {
		t.Fatalf("an unknown core secret: %v", err)
	}
	c = lintCore(t)
	c.cfg.DeployFile, c.cfg.CoreSecrets = good, []string{"store-core"}
	c.cfg.ProviderKeySecrets = map[string][]string{"anthropic-demo": {"demo"}}
	if err := c.loadDeploy(); err == nil || !strings.Contains(err.Error(), "anthropic-demo") {
		t.Fatalf("a provider key outside the description: %v", err)
	}
	// The shipped compose description with the secrets of compose core.yaml starts.
	c = lintCore(t)
	c.cfg.DeployFile = filepath.Join(c.spec.Root, "impl/deploy/compose/deploy.yaml")
	c.cfg.CoreSecrets = []string{"operator", "store-dsn", "token-key"}
	c.cfg.ProviderKeySecrets = map[string][]string{"provider-key-demo": {"demo"}}
	if err := c.loadDeploy(); err != nil {
		t.Fatalf("compose: %v", err)
	}
	c = lintCore(t)
	c.cfg.DeployFile, c.cfg.CoreSecrets = good, []string{"store-core"}
	if err := c.loadDeploy(); err != nil || c.deploy == nil || !slices.Equal(c.cfg.FarmHosts, []string{"core1"}) {
		t.Fatalf("good description: %v %v", err, c.cfg.FarmHosts)
	}
	if ch := checks(c); !ch["deploy.lint"].OK {
		t.Fatalf("doctor %+v", ch)
	}
}

// secret.revoke reads from the deployment description which tenant processes use a secret: a
// channel's token holds the channel, the provider key the core holds for a tenant holds the
// tenant, a secret of the farm's own processes only is revoked with no hold, a secret of a
// process without a tenant holds every tenant, and uses only in tenants the credential does not
// reach are forbidden (SEC-6, DEP-10).
func TestSecretRevokeByDescription(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	holds := func(where string) int {
		return b.count(t, `SELECT count(*) FROM holds WHERE reason = 'compromised' AND `+where)
	}
	revoke := func(doc, secret string) error {
		t.Helper()
		l, err := LintDeploy(b.c.spec, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		b.c.deploy = l.Description
		_, err = b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: secret, Compromised: true})
		return err
	}
	compose, err := os.ReadFile(filepath.Join(b.c.spec.Root, "impl/deploy/compose/deploy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	// (a) The shipped compose description: the core's secrets are the farm's, its provider key
	// is tenant demo's, the channel's tokens hold the channel.
	b.c.cfg.ProviderKeySecrets = map[string][]string{"provider-key-demo": {"demo"}}
	if err := revoke(string(compose), "store-dsn"); err != nil || holds("true") != 0 {
		t.Fatalf("a farm secret: %v, holds %d", err, holds("true"))
	}
	if err := revoke(string(compose), "provider-key-demo"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'demo'") != 1 {
		t.Fatalf("the provider key: %v, holds %d", err, holds("scope_kind = 'tenant'"))
	}
	if err := revoke(string(compose), "bot-token"); err != nil || holds("scope_kind = 'channel' AND scope_name = 'telegram'") != 1 {
		t.Fatalf("the channel token: %v", err)
	}
	// (b) A process of no known tenant: every tenant is held.
	b.exec(t, `DELETE FROM holds`)
	if err := revoke(strings.Replace(deployBase, "{name: telegram, kind: channel, tenant: demo, ", "{name: telegram, kind: channel, ", 1), "wt-channel"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'demo'") != 1 {
		t.Fatalf("an unresolved secret: %v", err)
	}
	// Uses only in a tenant the credential does not reach.
	if err := revoke(strings.Replace(deployBase, "tenant: demo, placement: host, user: botaru-read", "tenant: demo2, placement: host, user: botaru-read", 1), "wt-read"); !hasReason(err, "forbidden") {
		t.Fatalf("another tenant's secret: %v", err)
	}
	// A provider key two tenants share: forbidden unless the credential reaches both.
	b.c.cfg.ProviderKeySecrets = map[string][]string{"shared-key": {"demo", "demo2"}}
	if err := revoke(deployBase, "shared-key"); !hasReason(err, "forbidden") {
		t.Fatalf("a shared provider key: %v", err)
	}
	farmRevoke := func(doc, secret string) error {
		t.Helper()
		l, err := LintDeploy(b.c.spec, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		b.c.deploy = l.Description
		_, err = b.c.SecretRevoke(ctx, b.farmOp(), SecretRevokeRequest{Secret: secret, Compromised: true})
		return err
	}
	if err := farmRevoke(deployBase, "shared-key"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'demo2'") != 1 {
		t.Fatalf("the farm operator on a shared key: %v", err)
	}
	// A process of tenant farm: the farm operator holds the tenant farm's scope; a tenant
	// operator is forbidden.
	farmDoc := strings.Replace(deployBase, "tenant: demo, placement: sandbox", "tenant: farm, placement: sandbox", 1)
	if err := revoke(farmDoc, "wt-sbx"); !hasReason(err, "forbidden") {
		t.Fatalf("a farm process by a tenant operator: %v", err)
	}
	if err := farmRevoke(farmDoc, "wt-sbx"); err != nil || holds("scope_kind = 'tenant' AND scope_tenant = 'farm'") != 1 {
		t.Fatalf("a farm process by the farm operator: %v", err)
	}
	if err := revoke(deployBase, "nothing-uses-this"); !hasReason(err, "not_found") {
		t.Fatalf("an unknown secret: %v", err)
	}
}

// A runtime that keeps sessions itself and gave no match answer fails its erasure step
// at the person's confirmation, as a channel does: unmatched is not "not found" (RT-5,
// SEC-11).
func TestRuntimeInternalUnmatchedFails(t *testing.T) {
	ctx := context.Background()
	b, _ := newMemoryBench(t)
	b.exec(t, `UPDATE workers SET hello = jsonb_set(hello, '{runtime}', '{"session": "runtime_internal", "erasure": ["erase", "match"]}') WHERE id = $1`, b.agent.ID)
	b.exec(t, `UPDATE workers SET hello = jsonb_set(hello, '{driver}', '{"stores_content": "cache"}') WHERE id = $1`, b.channel.ID)
	b.hello(t, "1")
	b.finish(t, b.claim(t, b.agent, "attempt"))
	b.drain(t)
	d, err := b.c.ErasureMatch(ctx, b.op, MatchRequest{Subject: "system", Needles: []string{"Mari Tamm"}})
	if err != nil {
		t.Fatal(err)
	}
	// The runtime's match step is gone (as for a runtime that came after the request); the
	// channel answers its match with an id.
	b.exec(t, `DELETE FROM subject_match_steps WHERE kind = 'runtime'`)
	item := b.claim(t, b.channel, "erasure")
	if item == nil || item.Payload["op"] != "match" {
		t.Fatalf("match work of the channel %+v", item)
	}
	if _, err := b.c.Complete(ctx, b.channel, header(t, item), CompleteRequest{Erasure: map[string]any{"outcome": "done", "ids": []any{"tg:out:17"}}}); err != nil {
		t.Fatal(err)
	}
	card := cardOf(t, b.drain(t), subjectCard, "direct:1002")
	cd := card.body["card"].(map[string]any)
	req := DecideRequest{SubjectKind: "subject_request", ID: d.ID, Nonce: cd["nonce"].(string), Decision: "approve"}
	if _, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external), req); err != nil {
		t.Fatal(err)
	}
	b.tick(t, "erasure")
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT s.state FROM erasure_steps s JOIN subject_requests r ON r.erasure = s.request WHERE r.id = $1 AND s.kind = 'runtime'`, d.ID).Scan(&state); err != nil || state != "failed" {
		t.Fatalf("runtime step %s %v", state, err)
	}
}
