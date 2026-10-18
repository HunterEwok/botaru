// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// VariantListRequest is P2 variant.list: the archive of the person's tenant, optionally of one
// target `<Kind>:<name>`.
type VariantListRequest struct {
	Target string `json:"target"`
	Limit  int    `json:"limit"`
}

// VariantShowRequest is P2 variant.show.
type VariantShowRequest struct {
	Variant string `json:"variant"`
}

// ReviewRuleListRequest is P2 review_rule.list, optionally of one role.
type ReviewRuleListRequest struct {
	Role string `json:"role"`
}

// ReviewRuleDropRequest is P2 review_rule.drop.
type ReviewRuleDropRequest struct {
	Rule int64 `json:"rule"`
}

const variantColumns = `v.variant_id, v.target_kind, v.target_name, v.parent_variant_id, v.base_commit, v.proposal_id,
	coalesce(p.record->'body'->>'category', ''), v.author_role, v.author_bot_id, v.diversity_signature, v.trial_outcome, v.reason_code,
	v.live, v.children_count, v.successful_children_count, v.created_at`

func scanVariant(row pgx.Row) (map[string]any, string, error) {
	var id, kind, name, prop, category, role, sig, outcome string
	var parent, base, bot, reason *string
	var live bool
	var children, good int
	var at time.Time
	if err := row.Scan(&id, &kind, &name, &parent, &base, &prop, &category, &role, &bot, &sig, &outcome, &reason, &live, &children, &good, &at); err != nil {
		return nil, "", err
	}
	return map[string]any{
		"variant_id": id, "target_kind": kind, "target_name": name, "parent_variant_id": parent, "base_commit": base,
		"proposal_id": prop, "author_role": role, "author_bot_id": bot, "diversity_signature": sig, "trial_outcome": outcome,
		"reason_code": reason, "live": live, "children_count": children, "successful_children_count": good,
		"created_at": fmtTime(at),
	}, category, nil
}

// VariantList is P2 variant.list: the variants of the person's tenant in the categories it may
// decide, grouped by target and in the order of their lineage (parents before children), with
// the reason codes and the outcome a person may see (EVO-6). A read goes into the audit.
func (c *Core) VariantList(ctx context.Context, op Operator, req VariantListRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	kind, name, _ := strings.Cut(req.Target, ":")
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		cats, err := v.proposalCategories()
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+variantColumns+` FROM scaffold_variants v LEFT JOIN proposals p ON p.id = v.proposal_id
			WHERE v.tenant = $1 AND ($2 = '' OR v.target_kind = $2 AND v.target_name = $3)
			ORDER BY v.target_kind, v.target_name, v.created_at, v.variant_id`, tv.Name, kind, name)
		if err != nil {
			return err
		}
		var all []map[string]any
		for rows.Next() {
			it, category, err := scanVariant(rows)
			if err != nil {
				rows.Close()
				return err
			}
			if slices.Contains(cats, category) {
				all = append(all, it)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		items := []any{}
		for _, it := range lineageOrder(all) {
			if len(items) == limit {
				break
			}
			items = append(items, it)
		}
		out = map[string]any{"items": items}
		_, err = c.appendAudit(ctx, tx, now, Audit{Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "variant.list", OK: true,
			After: map[string]any{"count": len(items)}})
		return err
	})
	return out, err
}

// lineageOrder puts every variant after its parent, roots by creation time, and gives each its
// depth in the tree.
func lineageOrder(list []map[string]any) []map[string]any {
	byParent := map[string][]map[string]any{}
	have := map[string]bool{}
	for _, it := range list {
		have[it["variant_id"].(string)] = true
	}
	var roots []map[string]any
	for _, it := range list {
		p, _ := it["parent_variant_id"].(*string)
		if p == nil || !have[*p] {
			roots = append(roots, it)
			continue
		}
		byParent[*p] = append(byParent[*p], it)
	}
	var out []map[string]any
	var walk func(it map[string]any, depth int)
	walk = func(it map[string]any, depth int) {
		it["depth"] = depth
		out = append(out, it)
		for _, ch := range byParent[it["variant_id"].(string)] {
			walk(ch, depth+1)
		}
	}
	for _, r := range roots {
		walk(r, 0)
	}
	return out
}

// VariantShow is P2 variant.show: one variant with its fields before and after, the gate result
// without any case of the hidden set, the trial of its proposal and its chain of parents (EVO-6).
func (c *Core) VariantShow(ctx context.Context, op Operator, req VariantShowRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		cats, err := v.proposalCategories()
		if err != nil {
			return err
		}
		one := func(id string) (map[string]any, string, error) {
			return scanVariant(tx.QueryRow(ctx, `SELECT `+variantColumns+` FROM scaffold_variants v LEFT JOIN proposals p ON p.id = v.proposal_id
				WHERE v.tenant = $1 AND v.variant_id = $2`, tv.Name, id))
		}
		it, category, err := one(req.Variant)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !slices.Contains(cats, category) {
			return refuse("not_found", "no such variant")
		}
		if err != nil {
			return err
		}
		var patch, revert, gate []byte
		var hash, baseHash *string
		if err := tx.QueryRow(ctx, `SELECT patch_json, revert_patch_json, gate_metrics, manifest_sha256, base_sha256 FROM scaffold_variants WHERE variant_id = $1`,
			req.Variant).Scan(&patch, &revert, &gate, &hash, &baseHash); err != nil {
			return err
		}
		it["patch"], it["revert_patch"], it["gate"] = rawJSON(patch), rawJSON(revert), rawJSON(gate)
		it["manifest_sha256"], it["base_sha256"], it["tenant"] = hash, baseHash, tv.Name
		var state string
		var rec []byte
		if err := tx.QueryRow(ctx, `SELECT state, record FROM proposals WHERE id = $1`, it["proposal_id"]).Scan(&state, &rec); err == nil {
			var r map[string]any
			_ = json.Unmarshal(rec, &r)
			it["proposal_state"], it["trial"] = state, r["trial"]
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		chain := []any{}
		for p, _ := it["parent_variant_id"].(*string); p != nil && len(chain) < 50; {
			anc, ancCategory, err := one(*p)
			if err != nil {
				return err
			}
			// The chain stops at an ancestor of a category the person may not decide.
			if !slices.Contains(cats, ancCategory) {
				break
			}
			chain = append(chain, map[string]any{"variant_id": anc["variant_id"], "trial_outcome": anc["trial_outcome"],
				"diversity_signature": anc["diversity_signature"], "live": anc["live"]})
			p, _ = anc["parent_variant_id"].(*string)
		}
		it["lineage"] = chain
		out = it
		_, err = c.appendAudit(ctx, tx, now, Audit{Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "variant.show",
			Target: &Target{Kind: "variant", ID: req.Variant}, OK: true})
		return err
	})
	return out, err
}

func rawJSON(b []byte) any {
	if b == nil {
		return nil
	}
	var v any
	_ = json.Unmarshal(b, &v)
	return v
}

// ReviewRuleList is P2 review_rule.list: the live review rules of the tenant in the categories
// the person may decide, with their reason codes (EVO-3).
func (c *Core) ReviewRuleList(ctx context.Context, op Operator, req ReviewRuleListRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		cats, err := v.proposalCategories()
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT id, role, category, target_kind, target_name, problem_code, signature, outcome, reason_code, count,
			proposal_ids, first_at, last_at FROM review_rules r WHERE tenant = $1 AND dropped_at IS NULL AND ($2 = '' OR role = $2)
			AND category = ANY($3) AND NOT EXISTS (SELECT 1 FROM proposals p WHERE p.tenant = r.tenant AND r.proposal_ids ? p.id AND (p.state IN ('sampled', 'sample_presented')
				OR p.state = 'sample_closed' AND coalesce(p.record->'sample_answer'->>'answer', '') <> 'would_accept'))
			ORDER BY last_at DESC, id DESC`, tv.Name, req.Role, cats)
		if err != nil {
			return err
		}
		items := []any{}
		for rows.Next() {
			var id int64
			var role, category, kind, name, problem, sig, outcome, reason string
			var count int
			var props []byte
			var first, last time.Time
			if err := rows.Scan(&id, &role, &category, &kind, &name, &problem, &sig, &outcome, &reason, &count, &props, &first, &last); err != nil {
				rows.Close()
				return err
			}
			items = append(items, map[string]any{"rule": id, "role": role, "category": category, "target_kind": kind, "target_name": name,
				"problem_code": problem, "signature": sig, "outcome": outcome, "reason_code": reason, "count": count,
				"proposals": rawJSON(props), "first_at": fmtTime(first), "last_at": fmtTime(last)})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		out = map[string]any{"items": items}
		_, err = c.appendAudit(ctx, tx, now, Audit{Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "review_rule.list", OK: true,
			After: map[string]any{"count": len(items)}})
		return err
	})
	return out, err
}

// ReviewRuleDrop is P2 review_rule.drop: a person who may decide the rule's category drops it,
// and the role may propose that approach again (EVO-3, C1).
func (c *Core) ReviewRuleDrop(ctx context.Context, op Operator, req ReviewRuleDropRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		var category string
		var droppedBy *string
		err = tx.QueryRow(ctx, `SELECT category, dropped_by FROM review_rules WHERE id = $1 AND tenant = $2 FOR UPDATE`, req.Rule, tv.Name).Scan(&category, &droppedBy)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && droppedBy != nil && *droppedBy != op.Principal {
			return refuse("not_found", "no such live rule")
		}
		if err != nil {
			return err
		}
		if !v.can("proposal.decide:" + category) {
			pr := refuse("forbidden", "proposal.decide of the rule's category is needed")
			pr.Needs = []string{"proposal.decide:" + category}
			return pr
		}
		if droppedBy != nil {
			// A repeat by the same person gets the same answer (IDM-3).
			out = map[string]any{"rule": req.Rule, "dropped": true}
			return nil
		}
		if _, err := tx.Exec(ctx, `UPDATE review_rules SET dropped_at = $2, dropped_by = $3 WHERE id = $1`, req.Rule, now, op.Principal); err != nil {
			return err
		}
		out = map[string]any{"rule": req.Rule, "dropped": true}
		_, err = c.appendAudit(ctx, tx, now, Audit{Tenant: tv.Name, Kind: "command", Actor: v.actor(), Command: "review_rule.drop",
			Target: &Target{Kind: "review_rule", ID: fmt.Sprintf("rule-%d", req.Rule)}, OK: true})
		return err
	})
	return out, err
}
