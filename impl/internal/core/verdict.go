// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"math"
	"slices"
)

// commissionPolicy is the policy of a tenant's Commission manifest over the skeleton defaults
// (COM-8): thresholds and weights per lens. A veto lens minimum and the untrusted threshold are
// only raised by the manifest.
type commissionPolicy struct {
	ReviewDeadline string
	Min            map[string]float64
	Weights        map[string]float64
	RecommendMean  float64
	RecommendValue *float64
	ParkMean       float64
	HighRisk       map[string]float64 // lens -> score at least
	UntrustedValue float64
	UntrustedRisk  string
}

// defaultPolicy is the skeleton policy a Commission manifest refines (docs/improvements.md).
func defaultPolicy() commissionPolicy {
	v := 3.0
	return commissionPolicy{
		ReviewDeadline: "24h",
		Min:            map[string]float64{"security": 2, "operational": 2},
		Weights:        map[string]float64{},
		RecommendMean:  2.5, RecommendValue: &v, ParkMean: 1.5,
		HighRisk:       map[string]float64{"security": 3, "operational": 3},
		UntrustedValue: 4, UntrustedRisk: "medium",
	}
}

// policyFrom reads the spec of a Commission manifest over the defaults.
func policyFrom(spec map[string]any) commissionPolicy {
	p := defaultPolicy()
	if spec == nil {
		return p
	}
	if d := asString(spec["review_deadline"]); d != "" {
		p.ReviewDeadline = d
	}
	if m, ok := spec["min"].(map[string]any); ok {
		for k, v := range m {
			if n, ok := toFloat(v); ok && n > p.Min[k] {
				p.Min[k] = n
			}
		}
	}
	if m, ok := spec["weights"].(map[string]any); ok {
		for k, v := range m {
			if n, ok := toFloat(v); ok {
				p.Weights[k] = n
			}
		}
	}
	if m, ok := spec["recommend_if"].(map[string]any); ok {
		if n, ok := toFloat(m["weighted_mean_gte"]); ok {
			p.RecommendMean = n
		}
		if n, ok := toFloat(m["value_gte"]); ok {
			p.RecommendValue = &n
		} else {
			p.RecommendValue = nil
		}
	}
	if m, ok := spec["park_if"].(map[string]any); ok {
		if n, ok := toFloat(m["weighted_mean_gte"]); ok {
			p.ParkMean = n
		}
	}
	if m, ok := spec["high_risk"].(map[string]any); ok {
		p.HighRisk = map[string]float64{}
		if n, ok := toFloat(m["security_gte"]); ok {
			p.HighRisk["security"] = n
		}
		if n, ok := toFloat(m["operational_gte"]); ok {
			p.HighRisk["operational"] = n
		}
	}
	if m, ok := spec["untrusted"].(map[string]any); ok {
		if n, ok := toFloat(m["value_gte"]); ok && n > p.UntrustedValue {
			p.UntrustedValue = n
		}
		if r := asString(m["risk_max"]); r != "" && riskOrder(r) < riskOrder(p.UntrustedRisk) {
			p.UntrustedRisk = r
		}
	}
	return p
}

func riskOrder(r string) int {
	return slices.Index([]string{"low", "medium", "high"}, r)
}

// lensReview is what the verdict reads of one lens: a review (Score, Veto) or its absence
// (State other than done: pending, failed, late, schema_invalid).
type lensReview struct {
	Lens   string
	State  string
	Score  int
	Veto   bool
	Shadow bool
}

// verdictInput is everything the verdict depends on (COM-4): the same input gives the same
// verdict.
type verdictInput struct {
	KindNone  bool
	Risk      string
	Guarded   bool
	Untrusted bool
	Lenses    []string // the lenses enqueued for this proposal
	VetoLens  map[string]bool
	Reviews   []lensReview
}

// verdict computes the commission verdict by COM-4, COM-14 and PRP-12. The order: every veto
// lens needs a review; a veto dismisses; a veto lens below min dismisses; the weighted mean
// over the reviewed lenses; high risk or a guarded field need the high_risk threshold (a
// guarded field both veto lenses at 3 or more), untrusted text the untrusted threshold, where a
// threshold that names a lens without a review is not met; then recommend_if and park_if.
func verdict(p commissionPolicy, in verdictInput) map[string]any {
	got := map[string]lensReview{}
	for _, r := range in.Reviews {
		if r.Shadow || r.State != "done" {
			continue
		}
		got[r.Lens] = r
	}
	out := map[string]any{"weighted_mean": nil, "value_score": nil}
	var missing []any
	var lenses []any
	for _, l := range in.Lenses {
		r, ok := got[l]
		if !ok {
			missing = append(missing, l)
			continue
		}
		lenses = append(lenses, map[string]any{"lens": l, "score": r.Score, "veto": r.Veto, "weight": int(weightOf(p, l))})
	}
	if missing == nil {
		missing = []any{}
	}
	if lenses == nil {
		lenses = []any{}
	}
	out["missing_lenses"], out["lenses"] = missing, lenses
	done := func(o string) map[string]any { out["outcome"] = o; return out }
	for _, l := range in.Lenses {
		if _, ok := got[l]; !ok && in.VetoLens[l] {
			return done("parked(veto_lens_missing)")
		}
	}
	for _, l := range in.Lenses {
		if r, ok := got[l]; ok && r.Veto && in.VetoLens[l] {
			return done("dismissed(veto)")
		}
	}
	for _, l := range in.Lenses {
		if r, ok := got[l]; ok && in.VetoLens[l] && float64(r.Score) < p.Min[l] {
			return done("dismissed(below_min)")
		}
	}
	var sum, wsum float64
	for _, l := range in.Lenses {
		if r, ok := got[l]; ok {
			w := weightOf(p, l)
			sum += w * float64(r.Score)
			wsum += w
		}
	}
	mean := 0.0
	if wsum > 0 {
		mean = math.Round(sum/wsum*1000) / 1000
	}
	out["weighted_mean"] = mean
	value, hasValue := got["value"]
	if hasValue {
		out["value_score"] = value.Score
	}
	score := func(l string) (float64, bool) {
		r, ok := got[l]
		return float64(r.Score), ok
	}
	strict := []any{}
	met := true
	if in.Risk == "high" || in.Guarded {
		strict = append(strict, "high_risk")
		for l, min := range p.HighRisk {
			// For kind none only the security score counts (COM-14).
			if in.KindNone && l != "security" {
				continue
			}
			if s, ok := score(l); !ok || s < min {
				met = false
			}
		}
		if in.Guarded {
			for _, l := range []string{"security", "operational"} {
				if s, ok := score(l); !ok || s < 3 {
					met = false
				}
			}
		}
	}
	if in.Untrusted {
		strict = append(strict, "untrusted")
		if in.KindNone {
			if _, ok := score("security"); !ok {
				met = false
			}
		} else if s, ok := score("value"); !ok || s < p.UntrustedValue {
			met = false
		}
		if riskOrder(in.Risk) > riskOrder(p.UntrustedRisk) {
			met = false
		}
	}
	out["strict"] = strict
	if !met {
		return done("parked(score)")
	}
	valueOK := p.RecommendValue == nil || hasValue && float64(value.Score) >= *p.RecommendValue
	switch {
	case mean >= p.RecommendMean && valueOK:
		return done("recommended")
	case mean >= p.ParkMean:
		return done("parked(score)")
	}
	return done("dismissed(low_score)")
}

func weightOf(p commissionPolicy, lens string) float64 {
	if w, ok := p.Weights[lens]; ok && w >= 1 {
		return w
	}
	return 1
}
