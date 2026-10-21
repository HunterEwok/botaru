// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// The governance work of the mock model: the analysis of a role's own results (insight), the
// lens reviews of the commission and the farm manager's answer to an incident. These task types
// declare no result schema in the prompt (the core checks the result against its own), so the
// mock tells them by their instruction.
var (
	reReviewLens = regexp.MustCompile(`review-result for the lens ([a-z]+)`)
	reInsight    = regexp.MustCompile(`(?i)propose one change, the one with the|load check of this stand`)
	reIncident   = regexp.MustCompile(`(?i)read the incident`)
)

// governance is the next step of a governance task, or ok false for any other task. A step is
// a tool call (call set) or the final answer; evolve makes an analysis propose a manifest change.
func governance(system, goal, seen string, names []string, offered map[string]map[string]any, called map[string]int, evolve bool) (call map[string]any, answer string, ok bool) {
	switch {
	case reReviewLens.MatchString(system):
		return nil, reviewAnswer(reReviewLens.FindStringSubmatch(system)[1]), true
	case reInsight.MatchString(system):
		n := toolName(names, "", "observations_query")
		if n != "" && called[n] == 0 {
			if in := insightQuery(offered[n]); in != nil {
				return map[string]any{"type": "tool_use", "name": n, "input": in}, "", true
			}
		}
		return nil, insightAnswer(seen, evolve), true
	case reIncident.MatchString(system):
		return nil, managerAnswer(goal), true
	}
	return nil, "", false
}

// reviewAnswer passes the proposal with a forecast that agrees with the score (COM-4, COM-17).
func reviewAnswer(lens string) string {
	out := map[string]any{"lens": lens, "score": 3, "confidence": "medium"}
	switch lens {
	case "security", "operational":
		out["forecast"] = map[string]any{"bad": 0.1, "ok": 0.9}
	case "cost":
		out["forecast"] = map[string]any{"over": 0.2, "within": 0.8}
	default:
		out["forecast"] = map[string]any{"better": 0.6, "unchanged": 0.3, "worse": 0.1}
	}
	out["note"] = fmt.Sprintf("Through the %s lens the change is small and the evidence is the farm's own series.", lens)
	raw, _ := json.Marshal(out)
	return string(raw)
}

// pickEnum is the first wanted value the property's enum offers, else its first value, else
// the first wanted value.
func pickEnum(schema map[string]any, prop string, want ...string) string {
	props, _ := schema["properties"].(map[string]any)
	p, _ := props[prop].(map[string]any)
	enum, _ := p["enum"].([]any)
	for _, w := range want {
		if slices.Contains(enum, any(w)) {
			return w
		}
	}
	if len(enum) > 0 {
		return fmt.Sprint(enum[0])
	}
	if len(want) > 0 {
		return want[0]
	}
	return ""
}

// insightQuery reads how many attempts the investigations of ops-reader took over a day.
func insightQuery(schema map[string]any) map[string]any {
	return map[string]any{
		"series":  pickEnum(schema, "series", "farm.task.attempts", "farm.task.outcome"),
		"subject": pickEnum(schema, "subject", "task_type:ops.investigate.v1"),
		"window":  pickEnum(schema, "window", "1d", "7d"),
		"rollup":  pickEnum(schema, "rollup", "window.mean", "window.sum"),
	}
}

// EvolveQueued is the queue limit of demo the analysis proposes in the evolution mode: lower than
// the example's 5000, so the change takes the safe direction a Tenant limit may take (PRP-3).
const EvolveQueued = 4000

// insightAnswer proposes to look at the investigations, citing the value it read; without a
// value it found nothing. In the evolution mode it proposes to lower the queue limit of demo
// instead (a manifest change of PRP-3 with the attempts of the investigations as its trial
// metric), so the change goes through the merge gate, the archive and the revert (D-120).
func insightAnswer(seen string, evolve bool) string {
	var res struct {
		Series string `json:"series"`
		Window string `json:"window"`
		Rollup string `json:"rollup"`
		Groups []struct {
			Subject string `json:"subject"`
			Points  []struct {
				Value any `json:"value"`
			} `json:"points"`
		} `json:"groups"`
	}
	for _, l := range strings.Split(seen, "\n") {
		if i := strings.IndexByte(l, '{'); i >= 0 && json.Unmarshal([]byte(l[i:]), &res) == nil && len(res.Groups) > 0 {
			break
		}
	}
	if len(res.Groups) != 1 || len(res.Groups[0].Points) == 0 {
		return `{"code":"nothing_found"}`
	}
	g := res.Groups[0]
	body := map[string]any{
		"category": "reliability",
		"problem":  map[string]any{"code": "investigation_attempts", "subjects": []any{g.Subject}, "severity": "low"},
		"evidence": []any{map[string]any{"kind": "observation", "series": res.Series, "subject": g.Subject, "window": res.Window,
			"rollup": res.Rollup, "value": g.Points[len(g.Points)-1].Value}},
		"change": map[string]any{"kind": "none"},
	}
	if evolve {
		body["change"] = map[string]any{"kind": "manifest_change", "manifest_change": map[string]any{"ops": []any{
			map[string]any{"op": "set", "target": map[string]any{"kind": "Tenant", "name": "demo"}, "field": "spec.limits.max_queued", "value": EvolveQueued}}}}
		body["benefit"] = map[string]any{"metric": "farm.task.attempts", "subject": g.Subject, "direction": "down", "expected_change_pct": 10, "confidence": "low"}
		body["cost"] = map[string]any{"one_time_eur": 0, "monthly_eur": 0, "effort": "minutes"}
		body["risk"] = map[string]any{"level": "low"}
	}
	raw, _ := json.Marshal(map[string]any{"proposals": []any{body}})
	return string(raw)
}

// managerAnswer is the farm manager's reading of an incident: what it found and the operation
// it proposes; the core and people decide what runs.
func managerAnswer(input string) string {
	f := "The incident is open on the resource named in its block."
	if l := strings.ToLower(input); strings.Contains(l, "disk") || strings.Contains(l, "free_pct") {
		f = "The data volume of db1 is almost full; the pipeline temporary files fill it."
	}
	raw, _ := json.Marshal(map[string]any{"summary": f + " I propose to keep the automatic hold and to ask the people of the tenant for the cleanup of the pipeline temporary files (cleanup_pipeline_tmp)."})
	return string(raw)
}
