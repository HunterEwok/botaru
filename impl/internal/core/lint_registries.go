// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"regexp"
	"strings"
)

// The registries a lint checks names against, and closed schemas resolved against a catalog.

// lintRegs are the registries the set lint reads.
type lintRegs struct {
	formats      map[string]*regexp.Regexp
	capForms     map[string]map[string]any
	dataClasses  map[string]map[string]any
	roleProfiles map[string]map[string]any
	lenses       map[string]map[string]any
	purposes     map[string]bool
	forbidden    map[string]bool
	kinds        map[string]map[string]any
	retention    map[string]map[string]any
	driverForbid map[string]bool
}

func (c *Core) lintRegistries() (*lintRegs, error) {
	r := &lintRegs{formats: map[string]*regexp.Regexp{}, capForms: map[string]map[string]any{}, dataClasses: map[string]map[string]any{},
		roleProfiles: map[string]map[string]any{}, lenses: map[string]map[string]any{}, purposes: map[string]bool{}, kinds: map[string]map[string]any{},
		retention: map[string]map[string]any{}, driverForbid: map[string]bool{}}
	load := func(path string, fn func(id string, e map[string]any)) error {
		reg, err := c.spec.Registry(path)
		if err != nil {
			return err
		}
		for _, e := range reg.Entries {
			if id, ok := e["id"].(string); ok {
				fn(id, e)
			}
		}
		return nil
	}
	if err := load("spec/model/formats.yaml", func(id string, e map[string]any) {
		if p, ok := e["pattern"].(string); ok {
			// Go regexp has no lookaround; a pattern it cannot compile is left to the schema.
			if re, err := regexp.Compile(p); err == nil {
				r.formats[id] = re
			} else {
				r.formats[id] = nil
			}
		}
	}); err != nil {
		return nil, err
	}
	if err := load("spec/model/capabilities.yaml", func(id string, e map[string]any) {
		switch e["kind"] {
		case "capability":
			r.capForms[id] = e
		case "data_class":
			r.dataClasses[id] = e
		}
	}); err != nil {
		return nil, err
	}
	if err := load("spec/model/lenses.yaml", func(id string, e map[string]any) {
		if e["since"] == "v0.1" && e["subject"] == "proposal" {
			r.lenses[id] = e
		}
	}); err != nil {
		return nil, err
	}
	if err := load("spec/model/role-profiles.yaml", func(id string, e map[string]any) { r.roleProfiles[id] = e }); err != nil {
		return nil, err
	}
	if err := load("spec/model/task-purposes.yaml", func(id string, _ map[string]any) { r.purposes[id] = true }); err != nil {
		return nil, err
	}
	if err := load("spec/manifests/kinds.yaml", func(id string, e map[string]any) { r.kinds[id] = e }); err != nil {
		return nil, err
	}
	if err := load("spec/model/retention-classes.yaml", func(id string, e map[string]any) { r.retention[id] = e }); err != nil {
		return nil, err
	}
	return r, nil
}

// classRefused says whether a data class is refused where a manifest sets one: unknown or
// reserved (pii), and for an output class one that is never an output (secret) (PRM-5).
func (r *lintRegs) classRefused(cls string, output bool) bool {
	dc, ok := r.dataClasses[cls]
	return !ok || truthy(dc["reserved"]) || output && dc["output"] == false
}

// formatOK says whether v passes a registry format; a format the registry has without a
// pattern Go can run passes here and is left to the schema checks.
func (r *lintRegs) formatOK(format, v string) bool {
	re := r.formats[format]
	return re == nil || re.MatchString(v)
}

// parseCap splits a capability into its form and argument; "" when the form is unknown.
func (r *lintRegs) parseCap(cp string) (string, string) {
	if e, ok := r.capForms[cp]; ok && !strings.Contains(fmt.Sprint(e["form"]), "<") {
		return cp, ""
	}
	if base, arg, ok := strings.Cut(cp, ":"); ok {
		if e, ok := r.capForms[base]; ok && strings.Contains(fmt.Sprint(e["form"]), "<") {
			return base, arg
		}
		// A capability whose form has a fixed argument, such as governance.confirm:commission.
		if e, ok := r.capForms[base]; ok && fmt.Sprint(e["form"]) == cp {
			return base, arg
		}
	}
	if _, ok := r.capForms[cp]; ok {
		return cp, ""
	}
	return "", ""
}

// lintManifests runs the set lint (MAN-6: an error fails the whole set).
// personaLookup gives the text of a persona of a tenant by its hash, when apply has it.
type personaLookup func(tenant, sha string) (string, bool)

// resolveSchema turns a closed manifest schema into a checkable one: enum_from from the
// tenant catalog, the format from the registry, as the skeleton lint does.
func resolveSchema(schema map[string]any, lists map[string][]string, r *lintRegs) map[string]any {
	var res func(v any) any
	res = func(v any) any {
		switch x := v.(type) {
		case []any:
			out := make([]any, len(x))
			for i, e := range x {
				out[i] = res(e)
			}
			return out
		case map[string]any:
			out := map[string]any{}
			for k, e := range x {
				switch {
				case k == "enum_from":
					s, _ := e.(string)
					src, name, _ := strings.Cut(s, ".")
					if vals, ok := lists[name]; src == "catalog" && lists != nil && ok {
						l := make([]any, len(vals))
						for i, v := range vals {
							l[i] = v
						}
						out["enum"] = l
					} else {
						out["type"] = "string"
					}
				case k == "x-botaru-format":
					if s, _ := e.(string); r.formats[s] != nil {
						out["pattern"] = r.formats[s].String()
					}
				case k == "properties" || k == "$defs":
					m, _ := e.(map[string]any)
					pm := map[string]any{}
					for pk, pv := range m {
						pm[pk] = res(pv)
					}
					out[k] = pm
				case k == "items" || k == "additionalProperties" || k == "anyOf" || k == "oneOf" || k == "allOf":
					out[k] = res(e)
				case strings.HasPrefix(k, "x-botaru-") || k == "default":
				default:
					out[k] = e
				}
			}
			return out
		}
		return v
	}
	if schema == nil {
		return map[string]any{}
	}
	return res(schema).(map[string]any)
}
