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
		return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: " + name + "}\nspec: {budgets: {month_eur: " + strconv.Itoa(month) + "}}\n---\n"
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

// The review line covers max_per_week x 4.4 x the price of the reviews of one proposal; a line
// below it is rejected with the missing amount (BUD-13).
func TestLintReviewLine(t *testing.T) {
	c := lintCore(t)
	types := c.reviewLensTypes(nil, true)
	var docs strings.Builder
	for _, ty := range types {
		docs.WriteString("apiVersion: botaru/v1alpha1\nkind: TaskType\nmetadata: {name: " + ty + "}\nspec: {tree_budget_eur: 0.06}\n---\n")
	}
	docs.WriteString("apiVersion: botaru/v1alpha1\nkind: Role\nmetadata: {name: analyst}\nspec: {insight: {max_per_week: 3}}\n---\n")
	tenant := func(review string) string {
		return "apiVersion: botaru/v1alpha1\nkind: Tenant\nmetadata: {name: demo}\nspec: {budgets: {lines: {insight: {month_eur: 5}, review: {month_eur: " + review + "}}}}\n"
	}
	need := reviewNeed([]int{3}, int64(len(types))*60000)
	got := lintDetails(t, c, docs.String()+tenant("2"))
	d, ok := got["review_line_too_small demo"]
	if !ok || !strings.Contains(d, eurCents(need)+" EUR a month, "+eurCents(need-2000000)+" EUR more") {
		t.Fatalf("a short review line %v (need %d)", got, need)
	}
	if _, ok := lintDetails(t, c, docs.String()+tenant("10"))["review_line_too_small demo"]; ok {
		t.Fatal("a covering review line rejected")
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
	c = lintCore(t)
	c.cfg.DeployFile = good
	if err := c.loadDeploy(); err != nil || c.deploy == nil || !slices.Equal(c.cfg.FarmHosts, []string{"core1"}) {
		t.Fatalf("good description: %v %v", err, c.cfg.FarmHosts)
	}
	if ch := checks(c); !ch["deploy.lint"].OK {
		t.Fatalf("doctor %+v", ch)
	}
}

// secret.revoke reads from the deployment description which tenant processes use a secret: a
// channel's token holds the channel, and a secret of farm processes only is revoked with no
// hold instead of not_found (SEC-6, DEP-10).
func TestSecretRevokeByDescription(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	l, err := LintDeploy(b.c.spec, []byte(deployBase))
	if err != nil || l.Err() != nil {
		t.Fatal(err, l.Err())
	}
	b.c.deploy = l.Description
	if _, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: "store-core", Compromised: true}); err != nil {
		t.Fatalf("a farm secret: %v", err)
	}
	if n := b.count(t, `SELECT count(*) FROM holds WHERE reason = 'compromised'`); n != 0 {
		t.Fatalf("holds for a farm secret %d", n)
	}
	if _, err := b.c.SecretRevoke(ctx, b.op, SecretRevokeRequest{Secret: "wt-channel", Compromised: true}); err != nil {
		t.Fatal(err)
	}
	if n := b.count(t, `SELECT count(*) FROM holds WHERE reason = 'compromised' AND scope_kind = 'channel' AND scope_name = 'telegram'`); n != 1 {
		t.Fatalf("channel holds %d", n)
	}
}
