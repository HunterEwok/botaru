// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// gwTenant is a tenant set with one subprocessor entry of provider-a and the profile base-std
// over it, for the lint of provider terms.
func gwTenant(kind, sub, transfer, profile string) string {
	return `apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: demo}
spec:
  kind: ` + kind + `
  farm_manager: {enabled: false}
  models_allowed: [base-std]
  privacy:
    subprocessors:
      - ` + sub + `
` + transfer + `---
apiVersion: botaru/v1alpha1
kind: ModelProfile
metadata: {name: base-std}
spec: {provider: provider-a, model: model-a-small-2026-09, family: family-a, rank: 1, max_output_tokens: 8000, tokens_per_s: 60,
       price: {input: 1.0, output: 4.0, checked: '` + time.Now().Format("2006-01-02") + `'}, ` + profile + `}
`
}

// A profile whose provider terms are worse than the tenant's entry fails apply in the tenant's
// models_allowed, and P4 refuses a call on it with provider_config: retention above max_days, a
// model that requires retention under zero retention, review by the provider's people above
// what the terms allow, training on requests, a subprocessor outside the tenant's list (GW-15).
func TestBackfillProviderTerms(t *testing.T) {
	c := lintCore(t)
	const ok = `provider_training: {value: false, checked: '2026-10-01'}, provider_retention_days: {value: 30, checked: '2026-10-01'}`
	for _, tc := range []struct {
		name, sub, profile, want string
	}{
		{"within the terms", `{provider: provider-a, retention: {mode: standard, max_days: 30}, terms: {human_review: abuse_only}}`,
			ok + `, provider_human_review: {value: abuse_only, checked: '2026-10-01'}`, ""},
		{"human review yes over abuse_only", `{provider: provider-a, retention: {mode: standard, max_days: 30}, terms: {human_review: abuse_only}}`,
			ok + `, provider_human_review: {value: "yes", checked: '2026-10-01'}`, "provider_human_review"},
		{"human review abuse_only over none", `{provider: provider-a, retention: {mode: standard, max_days: 30}, terms: {human_review: none}}`,
			ok + `, provider_human_review: {value: abuse_only, checked: '2026-10-01'}`, "provider_human_review"},
		{"retention above max_days", `{provider: provider-a, retention: {mode: standard, max_days: 7}}`,
			ok + `, provider_human_review: {value: abuse_only, checked: '2026-10-01'}`, "provider_retention"},
		{"requires retention under zero", `{provider: provider-a, retention: {mode: zero}}`,
			`provider_training: {value: false, checked: '2026-10-01'}, provider_retention_days: {value: 0, checked: '2026-10-01'}, requires_retention: true, provider_human_review: {value: abuse_only, checked: '2026-10-01'}`, "provider_retention"},
		{"training on requests", `{provider: provider-a, retention: {mode: standard, max_days: 30}}`,
			`provider_training: {value: true, checked: '2026-10-01'}, provider_retention_days: {value: 30, checked: '2026-10-01'}, provider_human_review: {value: abuse_only, checked: '2026-10-01'}`, "provider_training"},
		{"subprocessor outside the list", `{provider: provider-a, retention: {mode: standard, max_days: 30}}`,
			ok + `, provider_human_review: {value: abuse_only, checked: '2026-10-01'}, subprocessors: [{provider: provider-z}]`, "subprocessor_undeclared"},
	} {
		got := lintCodes(t, c, gwTenant("test", tc.sub, "", tc.profile))
		// GW-15: apply refuses a profile worse than the tenant's entry.
		if tc.want == "" && len(got) > 0 || tc.want != "" && !slices.Contains(got, tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, got, tc.want)
		}
	}

	// GW-15: P4 refuses a call on such a profile with provider_config.
	sub := manifest.Subprocessor{Provider: "provider-a"}
	sub.Retention.Mode = "standard"
	days := 30
	sub.Retention.MaxDays = &days
	sub.Terms.HumanReview = "abuse_only"
	tv := &TenantView{Name: "demo"}
	tv.Spec.Privacy.Subprocessors = []manifest.Subprocessor{sub}
	good := func() manifest.ModelProfile {
		p := manifest.ModelProfile{Provider: "provider-a"}
		p.ProviderTraining = &struct {
			Value bool `json:"value"`
		}{false}
		p.ProviderRetentionDays = &struct {
			Value int `json:"value"`
		}{30}
		p.ProviderHumanReview = &struct {
			Value string `json:"value"`
		}{"abuse_only"}
		return p
	}
	if _, err := profileAdmitted(tv, "base-std", good()); err != nil {
		t.Fatalf("a profile within the terms: %v", err)
	}
	review := good()
	review.ProviderHumanReview.Value = "yes"
	foreign := good()
	foreign.Subprocessors = []struct {
		Provider string `json:"provider"`
	}{{Provider: "provider-z"}}
	long := good()
	long.ProviderRetentionDays.Value = 31
	for name, p := range map[string]manifest.ModelProfile{"human review": review, "subprocessor": foreign, "retention": long} {
		if _, err := profileAdmitted(tv, "base-std", p); err == nil {
			t.Errorf("%s: P4 admitted the profile", name)
		} else if pr, _ := AsProblem(err); pr == nil || pr.Reason != "provider_config" {
			t.Errorf("%s: %v", name, err)
		}
	}
	zero := *tv
	zsub := sub
	zsub.Retention.Mode, zsub.Retention.MaxDays = "zero", nil
	zero.Spec.Privacy.Subprocessors = []manifest.Subprocessor{zsub}
	needs := good()
	needs.ProviderRetentionDays.Value = 0
	needs.RequiresRetention = true
	if _, err := profileAdmitted(&zero, "base-std", needs); err == nil {
		t.Error("P4 admitted a model that requires retention under zero retention")
	}
}

// P4 calls only a provider among the tenant's subprocessors, and a production tenant whose
// allowed profile calls a provider in a region outside the EU passes apply only with a transfer
// basis of its own for the provider or the region (GW-14).
func TestBackfillProviderRegion(t *testing.T) {
	tv := &TenantView{Name: "demo"}
	tv.Spec.Privacy.Subprocessors = []manifest.Subprocessor{{Provider: "provider-b"}}
	p := manifest.ModelProfile{Provider: "provider-a"}
	if _, err := profileAdmitted(tv, "base-std", p); err == nil {
		t.Fatal("P4 admitted a provider outside the tenant's subprocessors")
	}
	c := lintCore(t)
	const terms = `provider_training: {value: false, checked: '2026-10-01'}, provider_retention_days: {value: 30, checked: '2026-10-01'}, provider_human_review: {value: abuse_only, checked: '2026-10-01'}`
	us := `{provider: provider-a, region: us, retention: {mode: standard, max_days: 30}}`
	for _, tc := range []struct {
		name, kind, sub, transfer string
		refused                   bool
	}{
		{"production, region us, no basis", "production", us, "", true},
		{"production, region us, basis for the provider", "production", us, "  model_transfer: [{provider: provider-a, basis: scc, checked: '2026-10-01'}]\n", false},
		{"production, region us, basis for the region", "production", us, "  model_transfer: [{region: us, basis: adequacy, checked: '2026-10-01'}]\n", false},
		{"production, region us, basis for another provider", "production", us, "  model_transfer: [{provider: provider-b, basis: scc, checked: '2026-10-01'}]\n", true},
		{"production, a test region", "production", `{provider: provider-a, region: region-a, retention: {mode: standard, max_days: 30}}`, "", false},
		{"test tenant", "test", us, "", false},
		{"production, region eu-west", "production", `{provider: provider-a, region: eu-west, retention: {mode: standard, max_days: 30}}`, "", false},
	} {
		// The regions outside the EU come from the binding note of the provider (anthropic.md).
		got := lintCodes(t, c, strings.ReplaceAll(gwTenant(tc.kind, tc.sub, tc.transfer, terms), "provider-a", "anthropic"))
		// GW-14: the transfer basis belongs to the controller tenant.
		if refused := slices.Contains(got, "transfer_basis_missing"); refused != tc.refused {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
	// GW-14: a provider without a binding note has no region outside the EU.
	if got := lintCodes(t, c, gwTenant("production", us, "", terms)); slices.Contains(got, "transfer_basis_missing") {
		t.Errorf("a provider without a binding: %v", got)
	}
}

// A person sets the prices with a check date: a price older than 120 days fails apply, one
// older than 60 days only warns in doctor, and doctor shows the stale one as an error (GW-17).
func TestBackfillPriceAge(t *testing.T) {
	c := lintCore(t)
	profile := func(days int) string {
		return `apiVersion: botaru/v1alpha1
kind: ModelProfile
metadata: {name: base-std}
spec: {provider: provider-a, model: model-a-small-2026-09, family: family-a, rank: 1, max_output_tokens: 8000, tokens_per_s: 60,
       price: {input: 1.0, output: 4.0, checked: '` + time.Now().AddDate(0, 0, -days).Format("2006-01-02") + `'},
       provider_training: {value: false, checked: '2026-10-01'}, provider_human_review: {value: abuse_only, checked: '2026-10-01'}}
`
	}
	// GW-17: 121 days fail apply, 70 days pass it.
	if got := lintCodes(t, c, profile(121)); !slices.Contains(got, "price_stale") {
		t.Fatalf("a price 121 days old: %v", got)
	}
	if got := lintCodes(t, c, profile(70)); slices.Contains(got, "price_stale") {
		t.Fatalf("a price 70 days old: %v", got)
	}

	ctx := context.Background()
	b := newBench(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	key := [2]string{"", "base-std"}
	p, ok := cat.Profiles[key]
	if !ok {
		t.Fatal("the bench catalog has no profile base-std")
	}
	now := b.now(t)
	for _, tc := range []struct {
		days     int
		severity string
	}{{10, "info"}, {70, "warn"}, {130, "fail"}} {
		p.Price.Checked = now.AddDate(0, 0, -tc.days).Format("2006-01-02")
		cat.Profiles[key] = p
		doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ch := range doc.Checks {
			if strings.HasSuffix(ch.ID, "model.price_age_days.base_std") {
				found = true
				// GW-17: doctor warns after 60 days and shows an error after 120.
				if ch.Severity != tc.severity || ch.OK != (tc.severity == "info") {
					t.Errorf("%d days: %+v", tc.days, ch)
				}
			}
		}
		if !found {
			t.Fatalf("%d days: no price age check", tc.days)
		}
	}
}
