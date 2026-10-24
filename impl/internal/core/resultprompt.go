// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// coreResultSchema is the result schema of a task type whose result the core checks against
// its own: the analysis envelope insight-result (INS-1), the
// review-result of a lens review or the necessity output (COM-13, COM-15), the findings of a
// reading for the farm manager (MGR-25), the translation-result of a translator (TRN-3), which
// stands in for the type's own looser schema. It goes into the claim payload like a declared
// result_schema, so that the runtime can ask the model for a matching answer; nil for any other
// type.
func (c *Core) coreResultSchema(tt manifest.TaskType, record map[string]any) map[string]any {
	switch tt.Purpose {
	case "insight", "aggregate":
		return c.inlineSchema("urn:botaru:schema:insight-result:1")
	case "review":
		if record["lens"] == necessityLens {
			// A copy: the payload must not share the map the core validates with.
			var out map[string]any
			if b, err := json.Marshal(necessitySchema); err == nil && json.Unmarshal(b, &out) == nil {
				return out
			}
			return nil
		}
		return c.inlineSchema(reviewResultSchema(record))
	case "inspect":
		return c.findingsSchema()
	case "translate":
		return c.inlineSchema(translationSchema)
	}
	return nil
}

// findingsSchema is the result a reading hands in, as takeFindings checks it: {findings: [...]}
// with at most 10 findings manager-finding/v1, whose numbers are named from the closed list.
func (c *Core) findingsSchema() map[string]any {
	f := c.inlineSchema("urn:botaru:schema:manager-finding:1")
	if f == nil {
		return nil
	}
	if v, ok := f["properties"].(map[string]any)["values"].(map[string]any); ok {
		names := make([]any, len(findingValueNames))
		for i, n := range findingValueNames {
			names[i] = n
		}
		v["propertyNames"] = map[string]any{"enum": names}
	}
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []any{"findings"},
		"properties": map[string]any{
			"findings": map[string]any{"type": "array", "maxItems": maxFindings, "items": f},
		},
	}
}

// fullResultSchema is the full result schema of a claim payload for the runtime: the payload's
// result schema with enum_from turned into the values of the attempt (the lists the attempt
// record does not hold, such as those of the facts field, resolved by the same mark) and each
// x-botaru-format into its pattern and maximum length; nil when it cannot be built.
func (c *Core) fullResultSchema(cat *Catalog, tenant, mark string, schema map[string]any, fixed map[string][]any) map[string]any {
	names := map[string]bool{}
	enumFromNames(schema, names)
	enums := map[string][]any{}
	missing := map[string]bool{}
	for n := range names {
		if v, ok := fixed[n]; ok {
			enums[n] = v
		} else {
			missing[n] = true
		}
	}
	if len(missing) > 0 {
		for n, v := range c.resolveEnumFrom(cat, tenant, mark, missing) {
			enums[n] = v
		}
	}
	full, err := c.fullSchema(schema, enums)
	if err != nil {
		return nil
	}
	return flattenFormats(full)
}

// flattenFormats moves the pattern and maxLength that fullSchema puts under allOf up into the
// leaf when the leaf has neither, so the model reads them as plain keywords; the schema checks
// the same values either way.
func flattenFormats(node map[string]any) map[string]any {
	var walk func(any) any
	walk = func(n any) any {
		switch x := n.(type) {
		case map[string]any:
			for k, v := range x {
				x[k] = walk(v)
			}
			list, ok := x["allOf"].([]any)
			if !ok {
				return x
			}
			var rest []any
			for _, e := range list {
				m, _ := e.(map[string]any)
				moved := false
				if len(m) == 1 {
					for _, k := range []string{"pattern", "maxLength"} {
						if v, ok := m[k]; ok {
							if _, has := x[k]; !has {
								x[k], moved = v, true
							}
						}
					}
				}
				if !moved {
					rest = append(rest, e)
				}
			}
			if len(rest) == 0 {
				delete(x, "allOf")
			} else {
				x["allOf"] = rest
			}
			return x
		case []any:
			for i, v := range x {
				x[i] = walk(v)
			}
		}
		return n
	}
	out, _ := walk(node).(map[string]any)
	return out
}

// copySchema is a deep copy of a schema, so the payload shares no map with the core.
func copySchema(s map[string]any) map[string]any {
	var out map[string]any
	if b, err := json.Marshal(s); err == nil && json.Unmarshal(b, &out) == nil {
		return out
	}
	return nil
}

// inlineSchema is a schema of spec/model with every $ref replaced by its target, for a model
// that reads the schema as text and resolves no references; nil when it cannot be read.
func (c *Core) inlineSchema(id string) map[string]any {
	raw, err := c.spec.Raw(id)
	if err != nil {
		return nil
	}
	out, _ := c.inline(raw, id, 0).(map[string]any)
	if out != nil {
		delete(out, "$schema")
		delete(out, "$id")
	}
	return out
}

func (c *Core) inline(node any, doc string, depth int) any {
	if depth > 32 {
		return map[string]any{}
	}
	switch x := node.(type) {
	case map[string]any:
		if ref, ok := x["$ref"].(string); ok {
			target, tdoc := c.refTarget(ref, doc)
			if target == nil {
				return map[string]any{}
			}
			out, _ := c.inline(target, tdoc, depth+1).(map[string]any)
			if out == nil {
				out = map[string]any{}
			}
			for k, v := range x {
				if k != "$ref" {
					out[k] = c.inline(v, doc, depth+1)
				}
			}
			delete(out, "$schema")
			delete(out, "$id")
			delete(out, "$defs")
			return out
		}
		out := make(map[string]any, len(x))
		for k, v := range x {
			if k == "$defs" {
				continue
			}
			out[k] = c.inline(v, doc, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = c.inline(v, doc, depth+1)
		}
		return out
	}
	return node
}

// refTarget is the node a $ref names, as a copy, and the document it lives in.
func (c *Core) refTarget(ref, doc string) (map[string]any, string) {
	id, frag, _ := strings.Cut(ref, "#")
	if id == "" {
		id = doc
	}
	raw, err := c.spec.Raw(id)
	if err != nil {
		return nil, ""
	}
	var node any = raw
	for _, seg := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		if seg == "" {
			continue
		}
		m, _ := node.(map[string]any)
		node = m[strings.ReplaceAll(strings.ReplaceAll(seg, "~1", "/"), "~0", "~")]
	}
	m, _ := node.(map[string]any)
	if m == nil {
		return nil, ""
	}
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp, id
}

// insightContext is the reference of an analysis task: role_task_types are the subjects of the
// work types of the role (the analysis types left out), and with the evolution on
// proposable_targets lists the changes a manifest_change of this tenant may hold, with the
// current values of the Tenant limits and the cron of each proposable schedule (PRP-3,
// PRP-10, PRP-11). The core checks every proposal; this only spares the model guesses.
func insightContext(cat *Catalog, tenant, role string, evolution bool) map[string]any {
	types := []any{}
	if r, ok := cat.Roles[role]; ok {
		for _, t := range r.Claims {
			if tt, ok := cat.TaskTypes[t]; ok && tt.Purpose != "insight" {
				types = append(types, "task_type:"+t)
			}
		}
	}
	out := map[string]any{"role_task_types": types}
	if !evolution {
		return out
	}
	tv := cat.Tenants[tenant]
	if tv == nil {
		return out
	}
	limits := map[string]any{}
	spec, _ := tv.Raw["spec"].(map[string]any)
	for _, sec := range []string{"limits", "campaigns", "scripts"} {
		m, _ := spec[sec].(map[string]any)
		for k, v := range m {
			if _, num := toFloat(v); num {
				limits["spec."+sec+"."+k] = v
			}
		}
	}
	schedules := map[string]any{}
	for name, sc := range tv.Schedules {
		if sc.Task == nil || !typeProposable(cat.TaskTypes[sc.Task.Type]) || sc.Proposable != nil && !*sc.Proposable {
			continue
		}
		schedules[name] = map[string]any{"spec.cron": sc.Cron, "spec.timezone": scheduleTZ(tv, sc)}
	}
	out["proposable_targets"] = map[string]any{
		"change_kinds": "change.kind none for an observation without a change; manifest_change for a change of the fields below, with benefit (a series of the role with direction and expected change), cost and risk",
		"tenant": map[string]any{"target": map[string]any{"kind": "Tenant", "name": tenant}, "op": "set",
			"rule": "a field of spec.limits, spec.campaigns or spec.scripts, only lower than its current value", "current": limits},
		"schedules": map[string]any{"target_kind": "Schedule", "op": "set",
			"rule":    "spec.cron shifts the minutes and hours at no more than the current frequency and at least half of it; spec.timezone is a zone name",
			"current": schedules},
	}
	return out
}
