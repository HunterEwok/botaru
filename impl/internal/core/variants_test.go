// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

type variantRow struct {
	ID, Kind, Name, Sig, Outcome, Proposal string
	Parent                                 *string
	Live                                   bool
	Children, Good                         int
}

func variantsOf(t *testing.T, b *bench) []variantRow {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT variant_id, target_kind, target_name, diversity_signature, trial_outcome,
		proposal_id, parent_variant_id, live, children_count, successful_children_count FROM scaffold_variants ORDER BY created_at, variant_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []variantRow
	for rows.Next() {
		var v variantRow
		if err := rows.Scan(&v.ID, &v.Kind, &v.Name, &v.Sig, &v.Outcome, &v.Proposal, &v.Parent, &v.Live, &v.Children, &v.Good); err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	return out
}

// evolutionEdit turns on the learning part of the evolution for the tenant demo.
func evolutionEdit(ms []map[string]any) {
	specOf(ms, "Tenant", "demo")["insights"].(map[string]any)["evolution"] = map[string]any{"enabled": true}
}

func variantOf(t *testing.T, b *bench, proposal string) variantRow {
	t.Helper()
	for _, v := range variantsOf(t, b) {
		if v.Proposal == proposal {
			return v
		}
	}
	t.Fatalf("no variant of %s", proposal)
	return variantRow{}
}

// Every manifest change proposal leaves a variant with its approach; a realized one is live, a
// reverted one keeps its outcome and teaches its role a review rule, which refuses the same
// approach to the same problem at creation; the role's analysis input carries the variants and
// the rule as outcome classes only (EVO-3, EVO-4, EVO-5, GOV-29).
func TestVariantArchiveRevert(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit, evolutionEdit)
	id, other := acceptLimits(t, b)
	vs := variantsOf(t, b)
	if len(vs) != 2 {
		t.Fatalf("variants %+v", vs)
	}
	for _, v := range vs {
		if v.Kind != "Tenant" || v.Name != "demo" || v.Outcome != "pending" || v.Parent != nil || v.Live {
			t.Fatalf("variant %+v", v)
		}
	}
	if s := variantOf(t, b, id).Sig; s != "Tenant.spec.limits.max_queued:down" {
		t.Fatalf("signature %q", s)
	}
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Botaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	if v := variantOf(t, b, id); !v.Live || v.Outcome != "pending" {
		t.Fatalf("realized variant %+v", v)
	}
	prev := asList(recordOf(t, b, id)["realization"].(map[string]any)["revert_patch"])[0].(map[string]any)["previous"]
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "a000000000000000000000000000000000000003", Message: "Botaru-Revert: " + id + " harm_observed",
		Manifests: withLimit(t, b, int(prev.(float64)))}); err != nil || !res.Applied {
		t.Fatalf("revert %+v %v", res, err)
	}
	v := variantOf(t, b, id)
	if v.Live || v.Outcome != "reverted" {
		t.Fatalf("reverted variant %+v", v)
	}
	var reason, sig, outcome, role string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason_code FROM scaffold_variants WHERE variant_id = $1`, v.ID).Scan(&reason); err != nil || reason != "harm_observed" {
		t.Fatalf("reason %q %v", reason, err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT role, signature, outcome FROM review_rules WHERE dropped_at IS NULL`).Scan(&role, &sig, &outcome); err != nil {
		t.Fatal(err)
	}
	if sig != v.Sig || outcome != "reverted" {
		t.Fatalf("rule %s %s %s", role, sig, outcome)
	}
	// The same approach to the same problem is refused at creation, without a record.
	b.asOtherAuthor(t)
	before := len(proposals(t, b))
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{changeBody("slow", limitsChange(6))}}); err != nil {
		t.Fatal(err)
	}
	if rj := rejections(t, b); len(rj) == 0 || rj[len(rj)-1] != "review_rule" {
		t.Fatalf("rejections %v", rj)
	}
	if n := len(proposals(t, b)); n != before {
		t.Fatalf("a refused approach got a record: %d -> %d", before, n)
	}
	// The analysis input of the role: variants and rules, outcome classes only.
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	evo, err := evolutionInput(ctx, tx, "demo", role)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.c.spec.Validate("urn:botaru:schema:insight-evolution:1", evo); err != nil {
		t.Fatalf("evolution %v: %v", evo, err)
	}
	raw, _ := json.Marshal(evo)
	if strings.Contains(string(raw), "harm_observed") || strings.Contains(string(raw), "reason") {
		t.Fatalf("a reason code reached the bot input: %s", raw)
	}
	vars := asList(evo["variants"])
	rules := asList(evo["review_rules"])
	if len(vars) != 1 || len(rules) != 1 {
		t.Fatalf("variants %v rules %v", vars, rules)
	}
	_ = other
}

// A variant of an object a live variant made has that variant as its parent; a better child
// counts as a success of the parent and takes the live flag (EVO-4).
func TestVariantLineage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	id, _ := acceptLimits(t, b)
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Botaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	parent := variantOf(t, b, id)
	b.asOtherAuthor(t)
	if _, err := b.analysis(t, "2", false, map[string]any{"proposals": []any{changeBody("queue_long", limitsChange(6))}}); err != nil {
		t.Fatal(err)
	}
	var child variantRow
	for _, v := range variantsOf(t, b) {
		if v.Parent != nil {
			child = v
		}
	}
	if child.Parent == nil || *child.Parent != parent.ID {
		t.Fatalf("child %+v of %+v (rejections %v)", child, parent, rejections(t, b))
	}
	if p := variantOf(t, b, id); p.Children != 1 || p.Good != 0 {
		t.Fatalf("parent %+v", p)
	}
	tx, err := b.c.st.Pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	p, err := loadProposal(ctx, tx, child.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	p.State = "realized"
	p.Rec["trial"] = map[string]any{"outcome": "better"}
	if err := syncVariants(ctx, tx, time.Now(), p); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if p := variantOf(t, b, id); p.Good != 1 || p.Live {
		t.Fatalf("parent after a better child %+v", p)
	}
	if c := variantOf(t, b, child.Proposal); !c.Live || c.Outcome != "better" {
		t.Fatalf("child %+v", c)
	}
}

// The diversity signature of an approach and its containment (EVO-5).
func TestDiversitySignature(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	doc := map[string]any{"spec": map[string]any{"cron": "0 */2 * * *", "limits": map[string]any{"max_queued": 10.0},
		"dispatch": map[string]any{"rules": []any{map[string]any{"timeouts": map[string]any{"attempt": "5m"}}}}}}
	ops := []manifestOp{
		{Op: "set", Field: "spec.cron", Value: "0 */4 * * *"},
		{Op: "set", Field: "spec.limits.max_queued", Value: 20},
		{Op: "set", Field: "spec.dispatch.rules[].timeouts.attempt", Value: "3m"},
		{Op: "set", Field: "spec.timezone", Value: "UTC"},
	}
	got := diversitySignature("Schedule", doc, ops, now)
	want := "Schedule.spec.cron:less_often,Schedule.spec.dispatch.rules[].timeouts.attempt:down,Schedule.spec.limits.max_queued:up,Schedule.spec.timezone:set"
	if got != want {
		t.Fatalf("signature\n got %s\nwant %s", got, want)
	}
	if !signatureContains(got, "Schedule.spec.cron:less_often") || signatureContains("Schedule.spec.cron:shifted", "Schedule.spec.cron:less_often") {
		t.Fatal("containment")
	}
	c := cloneDoc(doc)
	if !setPath(c, "spec.dispatch.rules[].timeouts.attempt", "3m") || !setPath(c, "spec.new.leaf", 1) || setPath(c, "spec.dispatch.rules[3].x", 1) {
		t.Fatal("setPath")
	}
	if v, _ := pathValue(c, "spec.dispatch.rules[].timeouts.attempt"); v != "3m" {
		t.Fatalf("set value %v", v)
	}
	if v, _ := pathValue(doc, "spec.dispatch.rules[].timeouts.attempt"); v != "5m" {
		t.Fatal("the clone shares the original")
	}
	if !slices.Contains(strings.Split(got, ","), "Schedule.spec.timezone:set") {
		t.Fatal("set")
	}
}

// People read the archive and the review rules in P2 and drop a rule; every read is audited
// (EVO-3, EVO-6).
func TestP2VariantViews(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newCommissionBench(t, true, presentEdit)
	alice := Operator{Principal: b.alice, Tenant: "demo"}
	id, _ := acceptLimits(t, b)
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "a000000000000000000000000000000000000002", Message: "Botaru-Proposal: " + id, Manifests: withLimit(t, b, 7)}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	l, err := b.c.VariantList(ctx, alice, VariantListRequest{Target: "Tenant:demo"})
	if err != nil || len(asList(l["items"])) != 2 {
		t.Fatalf("list %v %v", l, err)
	}
	validOut(t, b, "p2-variant-list-response", l)
	if l, err := b.c.VariantList(ctx, alice, VariantListRequest{Target: "Schedule:none"}); err != nil || len(asList(l["items"])) != 0 {
		t.Fatalf("a target filter %v %v", l, err)
	}
	v := variantOf(t, b, id)
	show, err := b.c.VariantShow(ctx, alice, VariantShowRequest{Variant: v.ID})
	if err != nil || show["proposal_state"] != "realized" || show["live"] != true || len(asList(show["patch"])) != 1 {
		t.Fatalf("show %v %v", show, err)
	}
	validOut(t, b, "p2-variant-show-response", show)
	if _, err := b.c.VariantShow(ctx, alice, VariantShowRequest{Variant: "var_01JB00000000000000000000A1"}); reasonOf(err) != "not_found" {
		t.Fatalf("an unknown variant: %v", err)
	}
	prev := asList(recordOf(t, b, id)["realization"].(map[string]any)["revert_patch"])[0].(map[string]any)["previous"]
	if res, err := b.c.Apply(ctx, farmOf(b.op), ApplyRequest{Commit: "a000000000000000000000000000000000000003", Message: "Botaru-Revert: " + id + " harm_observed",
		Manifests: withLimit(t, b, int(prev.(float64)))}); err != nil || !res.Applied {
		t.Fatalf("revert %+v %v", res, err)
	}
	rules, err := b.c.ReviewRuleList(ctx, alice, ReviewRuleListRequest{})
	if err != nil || len(asList(rules["items"])) != 1 {
		t.Fatalf("rules %v %v", rules, err)
	}
	validOut(t, b, "p2-review-rule-list-response", rules)
	rule := asList(rules["items"])[0].(map[string]any)
	if rule["reason_code"] != "harm_observed" || rule["outcome"] != "reverted" {
		t.Fatalf("rule %v", rule)
	}
	n, _ := toFloat(rule["rule"])
	dropped, err := b.c.ReviewRuleDrop(ctx, alice, ReviewRuleDropRequest{Rule: int64(n)})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-review-rule-drop-response", dropped)
	// A repeat by the same person is the same answer; for anyone else the rule is gone.
	if again, err := b.c.ReviewRuleDrop(ctx, alice, ReviewRuleDropRequest{Rule: int64(n)}); err != nil || again["dropped"] != true {
		t.Fatalf("a second drop: %v %v", again, err)
	}
	if _, err := b.c.ReviewRuleDrop(ctx, Operator{Principal: b.op.Principal, Tenant: "demo"}, ReviewRuleDropRequest{Rule: int64(n)}); reasonOf(err) != "not_found" {
		t.Fatalf("a drop by another person: %v", err)
	}
	if rules, err := b.c.ReviewRuleList(ctx, alice, ReviewRuleListRequest{}); err != nil || len(asList(rules["items"])) != 0 {
		t.Fatalf("after the drop %v %v", rules, err)
	}
	var audits int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' IN ('variant.list', 'variant.show', 'review_rule.list', 'review_rule.drop')`).Scan(&audits); err != nil {
		t.Fatal(err)
	}
	if audits != 6 {
		t.Fatalf("audited reads %d", audits)
	}
}
