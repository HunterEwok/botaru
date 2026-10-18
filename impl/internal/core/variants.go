// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// The outcomes of a variant (EVO-4): pending while its proposal is open, the trial outcome once
// measured, or the way it left.
const (
	variantPending   = "pending"
	variantGate      = "rejected_at_gate"
	variantCommittee = "rejected_by_commission"
	variantHuman     = "rejected_by_human"
	variantReverted  = "reverted"
	variantExpired   = "expired"
	variantCanceled  = "canceled"
)

// variantRuleReasons are the reasons of a refusal whose lesson becomes a review rule (EVO-3):
// people's reasons about the change itself, a veto of the commission and a revert for harm.
// "Not now", a duplicate, "already done" and "other" teach nothing about the approach.
var variantRuleReasons = map[string]map[string]bool{
	variantHuman:     {"too_risky": true, "wrong_data": true, "too_costly": true, "not_relevant": true},
	variantCommittee: {"veto": true},
	variantReverted:  {"harm_observed": true},
}

// variantTarget is the part of a manifest change that sets the fields of one object.
type variantTarget struct {
	Kind, Name string
	Ops        []manifestOp
}

// variantTargets groups the operations of a manifest change by their target, in the order of
// the operations.
func variantTargets(b proposalBody) []variantTarget {
	if b.Change.Kind != "manifest_change" || b.Change.ManifestChange == nil {
		return nil
	}
	var out []variantTarget
	for _, op := range b.Change.ManifestChange.Ops {
		i := slices.IndexFunc(out, func(t variantTarget) bool { return t.Kind == op.Target.Kind && t.Name == op.Target.Name })
		if i < 0 {
			out = append(out, variantTarget{Kind: op.Target.Kind, Name: op.Target.Name})
			i = len(out) - 1
		}
		out[i].Ops = append(out[i].Ops, op)
	}
	return out
}

// diversitySignature is the structural print of an approach (EVO-5): the sorted entries
// `<Kind>.<field>:<direction>`, the direction up or down for numbers and durations, more_often
// or less_often or shifted for a cron expression, set for a field the object did not have,
// changed otherwise.
func diversitySignature(kind string, doc map[string]any, ops []manifestOp, now time.Time) string {
	var parts []string
	for _, op := range ops {
		old, had := pathValue(doc, op.Field)
		parts = append(parts, kind+"."+op.Field+":"+direction(op.Field, old, had, op.Value, now))
	}
	slices.Sort(parts)
	return strings.Join(slices.Compact(parts), ",")
}

func direction(field string, old any, had bool, nv any, now time.Time) string {
	if !had || old == nil {
		return "set"
	}
	if strings.HasSuffix(field, ".cron") {
		a, ok1 := cronRuns(asString(old), now)
		b, ok2 := cronRuns(asString(nv), now)
		switch {
		case !ok1 || !ok2:
			return "changed"
		case b > a:
			return "more_often"
		case b < a:
			return "less_often"
		}
		return "shifted"
	}
	if a, ok := toFloat(old); ok {
		if b, ok := toFloat(nv); ok {
			return upDown(b - a)
		}
	}
	if a, ok := parseDuration(asString(old)); ok {
		if b, ok := parseDuration(asString(nv)); ok {
			return upDown(float64(b - a))
		}
	}
	return "changed"
}

func upDown(d float64) string {
	switch {
	case d > 0:
		return "up"
	case d < 0:
		return "down"
	}
	return "changed"
}

// signatureContains says whether every entry of the rule's signature is in the signature of a
// new approach (EVO-3): adding a field to a refused approach does not escape its rule.
func signatureContains(sig, rule string) bool {
	have := strings.Split(sig, ",")
	for _, e := range strings.Split(rule, ",") {
		if !slices.Contains(have, e) {
			return false
		}
	}
	return true
}

// setPath writes a value at a field path of a document copy; a `[]` part names the single
// element of a list, `[i]` the element i.
func setPath(doc map[string]any, path string, v any) bool {
	var cur any = doc
	parts := strings.Split(path, ".")
	for i, part := range parts {
		idx := -1
		if name, rest, ok := strings.Cut(part, "["); ok {
			part = name
			n := strings.TrimSuffix(rest, "]")
			if n == "" {
				idx = 0
			} else if k, err := strconv.Atoi(n); err == nil {
				idx = k
			} else {
				return false
			}
		}
		m, ok := cur.(map[string]any)
		if !ok {
			return false
		}
		last := i == len(parts)-1
		if idx < 0 {
			if last {
				m[part] = v
				return true
			}
			next, ok := m[part]
			if !ok {
				next = map[string]any{}
				m[part] = next
			}
			cur = next
			continue
		}
		list, ok := m[part].([]any)
		if !ok || idx >= len(list) || last {
			return false
		}
		cur = list[idx]
	}
	return false
}

func cloneDoc(doc map[string]any) map[string]any {
	raw, _ := json.Marshal(doc)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

// createVariants writes the variants of a new manifest change proposal: one per target object,
// its parent the live variant of the object while the object is still what that variant made it,
// else none with the applied commit as the base (EVO-4).
func createVariants(ctx context.Context, tx pgx.Tx, now time.Time, id, tenant, role, bot string, b proposalBody) error {
	for _, t := range variantTargets(b) {
		doc, commit, err := storedDoc(ctx, tx, t.Kind, tenant, t.Name)
		if err != nil {
			return err
		}
		if doc == nil {
			continue
		}
		baseHash, err := jcs.Hash(doc)
		if err != nil {
			return err
		}
		after := cloneDoc(doc)
		patch, revert := []any{}, []any{}
		for _, op := range t.Ops {
			old, had := pathValue(doc, op.Field)
			if !had {
				old = nil
			}
			setPath(after, op.Field, op.Value)
			patch = append(patch, map[string]any{"field": op.Field, "value": op.Value})
			revert = append(revert, map[string]any{"field": op.Field, "previous": old})
		}
		hash, err := jcs.Hash(after)
		if err != nil {
			return err
		}
		var parent *string
		var p string
		err = tx.QueryRow(ctx, `SELECT variant_id FROM scaffold_variants WHERE tenant = $1 AND target_kind = $2 AND target_name = $3
			AND live AND manifest_sha256 = $4 ORDER BY created_at DESC LIMIT 1`, tenant, t.Kind, t.Name, baseHash).Scan(&p)
		switch {
		case err == nil:
			parent = &p
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		var botID any
		if bot != "" {
			botID = bot
		}
		pj, _ := json.Marshal(patch)
		rj, _ := json.Marshal(revert)
		if _, err := tx.Exec(ctx, `INSERT INTO scaffold_variants (variant_id, tenant, target_kind, target_name, parent_variant_id, base_commit,
			base_sha256, proposal_id, author_bot_id, author_role, manifest_sha256, patch_json, revert_patch_json, diversity_signature,
			created_at, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $15)`,
			ids.New(ids.Variant), tenant, t.Kind, t.Name, parent, commit, baseHash, id, botID, role, hash, pj, rj,
			diversitySignature(t.Kind, doc, t.Ops, now), now); err != nil {
			return err
		}
		if parent != nil {
			if _, err := tx.Exec(ctx, `UPDATE scaffold_variants SET children_count = children_count + 1, updated_at = $2 WHERE variant_id = $1`, *parent, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// variantOutcome is the outcome and reason code of a proposal for its variants (EVO-4).
func variantOutcome(p *proposalRow) (string, string) {
	switch p.State {
	case "dismissed":
		if p.reason() == "gate_failed" {
			return variantGate, asString(mmapAny(p.Rec["gate"])["code"])
		}
		return variantCommittee, p.reason()
	case "declined":
		return variantHuman, asString(mmapAny(p.Rec["decision"])["reason_code"])
	case "expired":
		return variantExpired, ""
	case "realization_failed":
		return variantCanceled, p.reason()
	case "reverted":
		return variantReverted, asString(mmapAny(p.Rec["realization"])["revert_reason"])
	case "realized":
		if o := asString(mmapAny(p.Rec["trial"])["outcome"]); o != "" {
			return o, ""
		}
	}
	return variantPending, ""
}

// syncVariants brings the variants of a manifest change proposal to its current outcome: the
// live flag while the change is realized, the success counter of the parent on a better
// outcome, and a review rule for a lesson (EVO-3, EVO-4). It is called on every write of the
// proposal record and changes nothing when nothing moved.
func syncVariants(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	if p.kind() != "manifest_change" {
		return nil
	}
	out, reason := variantOutcome(p)
	live := p.State == "realized"
	var gate []byte
	if g := mmapAny(p.Rec["gate"]); g != nil {
		gate, _ = json.Marshal(variantGateMetrics(g))
	}
	rows, err := tx.Query(ctx, `UPDATE scaffold_variants v SET trial_outcome = $2, reason_code = $3, live = $4,
		gate_metrics = coalesce($5::jsonb, v.gate_metrics), updated_at = $6
		FROM (SELECT variant_id, trial_outcome AS old, live AS was FROM scaffold_variants WHERE proposal_id = $1 FOR UPDATE) o
		WHERE v.variant_id = o.variant_id AND (o.old <> $2 OR o.was <> $4 OR v.reason_code IS DISTINCT FROM $3
			OR $5::jsonb IS NOT NULL AND v.gate_metrics IS DISTINCT FROM $5::jsonb)
		RETURNING v.variant_id, v.tenant, v.target_kind, v.target_name, v.parent_variant_id, v.author_role, v.diversity_signature, o.old, o.was`,
		p.ID, out, nullable(reason), live, nullBytes(gate), now)
	if err != nil {
		return err
	}
	type changed struct {
		id, tenant, kind, name, role, sig, old string
		parent                                 *string
		was                                    bool
	}
	var list []changed
	for rows.Next() {
		var c changed
		if err := rows.Scan(&c.id, &c.tenant, &c.kind, &c.name, &c.parent, &c.role, &c.sig, &c.old, &c.was); err != nil {
			rows.Close()
			return err
		}
		list = append(list, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range list {
		// A realized variant replaces the live one of its object.
		if live && !c.was {
			if _, err := tx.Exec(ctx, `UPDATE scaffold_variants SET live = false, updated_at = $4
				WHERE tenant = $1 AND target_kind = $2 AND target_name = $3 AND live AND variant_id <> $5`, c.tenant, c.kind, c.name, now, c.id); err != nil {
				return err
			}
		}
		if c.parent != nil && out == "better" && c.old != "better" {
			if _, err := tx.Exec(ctx, `UPDATE scaffold_variants SET successful_children_count = successful_children_count + 1, updated_at = $2
				WHERE variant_id = $1`, *c.parent, now); err != nil {
				return err
			}
		}
		if c.old == out {
			continue
		}
		lesson := out
		if out == "worse" {
			reason = "worse"
		} else if !variantRuleReasons[out][reason] {
			continue
		}
		if err := recordReviewRule(ctx, tx, now, p, c.tenant, c.role, c.kind, c.name, c.sig, lesson, reason); err != nil {
			return err
		}
	}
	return nil
}

// variantGateMetrics is the closed part of a gate result the archive keeps (EVO-7): no case of
// the hidden set, only the verdict, the step code and the numbers.
func variantGateMetrics(g map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"state", "code", "base_sha", "holdout_passed", "regression_pass_rate", "diff_lines", "fields", "at"} {
		if v, ok := g[k]; ok {
			out[k] = v
		}
	}
	return out
}

func nullBytes(b []byte) any {
	if b == nil {
		return nil
	}
	return string(b)
}

// recordReviewRule keeps the lesson of a refused, reverted or worse change of a role (EVO-3):
// one live rule per role, target, problem and approach, counted on every repeat.
func recordReviewRule(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow, tenant, role, kind, name, sig, outcome, reason string) error {
	problem := p.body().Problem.Code
	ids, _ := json.Marshal([]string{p.ID})
	_, err := tx.Exec(ctx, `INSERT INTO review_rules (tenant, role, category, target_kind, target_name, problem_code, signature, outcome, reason_code,
		proposal_ids, first_at, last_at) VALUES ($1, $2, $11, $3, $4, $5, $6, $7, $8, $9, $10, $10)
		ON CONFLICT (tenant, role, target_kind, target_name, problem_code, signature) WHERE dropped_at IS NULL DO UPDATE
		SET count = review_rules.count + 1, outcome = EXCLUDED.outcome, reason_code = EXCLUDED.reason_code, last_at = EXCLUDED.last_at,
			proposal_ids = (SELECT coalesce(jsonb_agg(x), '[]') FROM (SELECT x FROM jsonb_array_elements(review_rules.proposal_ids || EXCLUDED.proposal_ids) x
				ORDER BY 1 DESC LIMIT 10) s)`,
		tenant, role, kind, name, problem, sig, outcome, reason, ids, now, p.body().Category)
	return err
}

// reviewRuleHit says whether a live review rule of the role refuses a new manifest change
// (EVO-3): the same target and problem, and an approach that contains the rule's signature.
func reviewRuleHit(ctx context.Context, tx pgx.Tx, now time.Time, tenant, role string, b proposalBody) (bool, error) {
	for _, t := range variantTargets(b) {
		rows, err := tx.Query(ctx, `SELECT signature FROM review_rules WHERE tenant = $1 AND role = $2 AND target_kind = $3
			AND target_name = $4 AND problem_code = $5 AND dropped_at IS NULL`, tenant, role, t.Kind, t.Name, b.Problem.Code)
		if err != nil {
			return false, err
		}
		sigs, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return false, err
		}
		if len(sigs) == 0 {
			continue
		}
		doc, _, err := storedDoc(ctx, tx, t.Kind, tenant, t.Name)
		if err != nil {
			return false, err
		}
		sig := diversitySignature(t.Kind, doc, t.Ops, now)
		for _, r := range sigs {
			if signatureContains(sig, r) {
				return true, nil
			}
		}
	}
	return false, nil
}

// evolutionRows bound the blocks of the evolution input of an analysis task (EVO-5, EVO-3).
const (
	evolutionVariants = 5
	evolutionRules    = 20
)

// evolutionInput is the closed block `evolution` of an analysis task of a role (EVO-5): up to
// five variants of the archive with different approaches, the live ones first, and the live
// review rules of the role. Only outcome classes: no reason codes of people's decisions and
// refusals, no gate step codes, no text (GOV-29, EVO-7).
func evolutionInput(ctx context.Context, tx pgx.Tx, tenant, role string) (map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT v.variant_id, v.target_kind, v.target_name, v.patch_json, v.diversity_signature, v.trial_outcome,
		v.live, v.children_count, v.successful_children_count
		FROM scaffold_variants v WHERE v.tenant = $1 AND (v.author_role = $2 OR v.live AND EXISTS (SELECT 1 FROM scaffold_variants o
			WHERE o.tenant = v.tenant AND o.author_role = $2 AND o.target_kind = v.target_kind AND o.target_name = v.target_name))
		ORDER BY v.live DESC, v.successful_children_count DESC, v.created_at DESC, v.variant_id`, tenant, role)
	if err != nil {
		return nil, err
	}
	variants := []any{}
	seen := map[string]bool{}
	for rows.Next() {
		var id, kind, name, sig, outcome string
		var patch []byte
		var live bool
		var children, good int
		if err := rows.Scan(&id, &kind, &name, &patch, &sig, &outcome, &live, &children, &good); err != nil {
			rows.Close()
			return nil, err
		}
		key := kind + "/" + name + "/" + sig
		if seen[key] || len(variants) >= evolutionVariants {
			continue
		}
		seen[key] = true
		var ops []map[string]any
		_ = json.Unmarshal(patch, &ops)
		fields := []any{}
		for _, op := range ops {
			fields = append(fields, op["field"])
		}
		variants = append(variants, map[string]any{
			"id": id, "target_kind": kind, "target_name": name, "fields": fields, "signature": sig,
			"outcome": outcome, "current": live, "children": children, "successful_children": good,
		})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rr, err := tx.Query(ctx, `SELECT target_kind, target_name, problem_code, signature, outcome, count FROM review_rules
		WHERE tenant = $1 AND role = $2 AND dropped_at IS NULL ORDER BY last_at DESC, id DESC LIMIT $3`, tenant, role, evolutionRules)
	if err != nil {
		return nil, err
	}
	rules := []any{}
	for rr.Next() {
		var kind, name, problem, sig, outcome string
		var count int
		if err := rr.Scan(&kind, &name, &problem, &sig, &outcome, &count); err != nil {
			rr.Close()
			return nil, err
		}
		rules = append(rules, map[string]any{"target_kind": kind, "target_name": name, "problem_code": problem,
			"signature": sig, "outcome": outcome, "count": count})
	}
	rr.Close()
	if err := rr.Err(); err != nil {
		return nil, err
	}
	return map[string]any{"variants": variants, "review_rules": rules}, nil
}
