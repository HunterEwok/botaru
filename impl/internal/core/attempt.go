// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// attemptRecord is what the core fixes for an attempt at claim: the run fingerprint with the
// hashes of its parts (PRV-1), the version of the tenant catalog, and the enum_from values of
// the type's result schema resolved against that version and filtered by the task's data mark
// (OUT-1). It is stored on the lease of the attempt.
type attemptRecord struct {
	Fingerprint string           `json:"fingerprint"`
	Parts       map[string]any   `json:"parts"`
	Catalog     *string          `json:"catalog"`
	EnumFrom    map[string][]any `json:"enum_from,omitempty"`
	// ResultSchema is the result_schema of the type at claim: the result is checked against the
	// type the attempt ran under, even if an apply changes the type meanwhile.
	ResultSchema map[string]any `json:"result_schema,omitempty"`
}

// runFingerprint computes the run fingerprint by the row "run fingerprint" of spec/hashing.md:
// an object of part hashes, an absent part null, and the hash of that object. The skills part is
// the role's skill tree hashes (an empty array without skills); this version has no sandbox. The persona part is the hashes of the persona and
// lens addendum the attempt got (GOV-4), null without one; the catalog part is the P3
// catalog of the attempt, null for a role without tools. The
// model part is the profile configured for the slot at claim. The runtime part takes the
// role's runtime driver, or the implementation name the worker reported at hello, and the
// version it reported; P1 hello carries no describe() values, so describe is null.
func runFingerprint(cat *Catalog, w *Worker, roleName, typ, slot, profile string, tools []catalogTool, persona []string) (string, map[string]any, error) {
	hashOf := func(v any) (any, error) {
		h, err := jcs.Hash(v)
		if err != nil {
			return nil, err
		}
		return h, nil
	}
	parts := map[string]any{"role": nil, "type": nil, "skills": nil, "persona": nil, "catalog": nil, "model": nil, "runtime": nil, "sandbox": nil}
	var err error
	if h, ok := cat.Hashes[manifest.Key{Kind: "Role", Name: roleName}]; ok {
		parts["role"] = h
	}
	if h, ok := cat.Hashes[manifest.Key{Kind: "TaskType", Name: typ}]; ok {
		parts["type"] = h
	}
	// The skills part is the role's pinned tree hashes (PRV-1, SKL-2).
	if parts["skills"], err = skillsPart(cat.Roles[roleName].Skills); err != nil {
		return "", nil, err
	}
	if len(persona) > 0 {
		if parts["persona"], err = hashOf(persona); err != nil {
			return "", nil, err
		}
	}
	if len(tools) > 0 {
		h, err := catalogPart(tools)
		if err != nil {
			return "", nil, err
		}
		parts["catalog"] = h
	}
	if slot != "" {
		if parts["model"], err = hashOf(map[string]any{"slot": slot, "profile": profile}); err != nil {
			return "", nil, err
		}
	}
	driver, ver := "", ""
	if w.Hello != nil {
		driver, ver, _ = strings.Cut(strings.TrimSpace(w.Hello.Implementation), " ")
	}
	if rt := cat.Roles[roleName].Runtime.Driver; rt != "" {
		driver = rt
	}
	runtime := map[string]any{"driver": nullable(driver), "version": nullable(ver), "describe": nil}
	if parts["runtime"], err = hashOf(runtime); err != nil {
		return "", nil, err
	}
	fp, err := jcs.Hash(parts)
	if err != nil {
		return "", nil, err
	}
	return fp, parts, nil
}

// enumFromNames collects the enum_from list names of a manifest schema.
func enumFromNames(node any, out map[string]bool) {
	switch x := node.(type) {
	case map[string]any:
		if n, ok := x["enum_from"].(string); ok {
			out[n] = true
		}
		for _, v := range x {
			enumFromNames(v, out)
		}
	case []any:
		for _, v := range x {
			enumFromNames(v, out)
		}
	}
}

// tenantCatalog returns the spec of the tenant's Catalog manifest and its hash, if any.
func tenantCatalog(cat *Catalog, tenant string) (map[string]any, *string) {
	var keys []manifest.Key
	for k := range cat.RawGlobals {
		if k.Kind == "Catalog" && k.Tenant == tenant {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })
	h := cat.Hashes[keys[0]]
	spec, _ := cat.RawGlobals[keys[0]]["spec"].(map[string]any)
	return spec, &h
}

// markOrder returns the order of a data class from spec/model/capabilities.yaml; a class
// without an order is not comparable.
func (c *Core) markOrder(mark string) (int, bool) {
	reg, err := c.spec.Registry("spec/model/capabilities.yaml")
	if err != nil {
		return 0, false
	}
	e, ok := reg.Entry(mark)
	if !ok || e["kind"] != "data_class" {
		return 0, false
	}
	n, ok := e["order"].(int)
	return n, ok
}

// resolveEnumFrom fills the values of enum_from lists for an attempt (OUT-1):
// catalog.hosts gives the machine ids, catalog.services the service names,
// catalog.monitor_triggers the trigger ids of monitor_triggers, catalog.monitor_items their
// items, catalog.<list> the
// values of the resources with that list whose data class is not above the task's mark;
// task.attachments is empty, because this version has no files; reasons.<name> gives the ids
// of spec/model/<name>-reasons.yaml. An unknown list resolves to no values, so no value fits.
func (c *Core) resolveEnumFrom(cat *Catalog, tenant, mark string, names map[string]bool) map[string][]any {
	return c.resolveEnumFor(cat, tenant, mark, "", names)
}

// resolveEnumFor is resolveEnumFrom for a tool: with right set, a resource list gives only the
// values of the resources whose read_by holds that tool (TEN-3a).
func (c *Core) resolveEnumFor(cat *Catalog, tenant, mark, right string, names map[string]bool) map[string][]any {
	out := map[string][]any{}
	spec, _ := tenantCatalog(cat, tenant)
	maxOrder, markOK := c.markOrder(mark)
	for name := range names {
		vals := []any{}
		src, list, _ := strings.Cut(name, ".")
		switch src {
		case "catalog":
			hosts, _ := spec["hosts"].([]any)
			switch list {
			case "hosts":
				for _, h := range hosts {
					if id, ok := h.(map[string]any)["id"]; ok {
						vals = append(vals, id)
					}
				}
			case "monitor_triggers":
				// The trigger ids of the catalog, for the monitor plugin (MON-6).
				trigs, _ := spec["monitor_triggers"].([]any)
				for _, t := range trigs {
					if id, ok := t.(map[string]any)["triggerid"]; ok {
						vals = append(vals, id)
					}
				}
			case "monitor_items":
				// The items of the catalog triggers, for the problems of the monitor plugin (MON-6).
				trigs, _ := spec["monitor_triggers"].([]any)
				for _, t := range trigs {
					if it, ok := t.(map[string]any)["item"]; ok && !slices.Contains(vals, it) {
						vals = append(vals, it)
					}
				}
			case "services":
				for _, h := range hosts {
					svcs, _ := h.(map[string]any)["services"].([]any)
					for _, s := range svcs {
						if n, ok := s.(map[string]any)["name"]; ok {
							vals = append(vals, n)
						}
					}
				}
			default:
				res, _ := spec["resources"].([]any)
				for _, r := range res {
					rm, _ := r.(map[string]any)
					if rm["list"] != list || right != "" && !readBy(rm, right) {
						continue
					}
					if dc, ok := rm["data_class"].(string); ok {
						o, ok := c.markOrder(dc)
						if !ok || !markOK || o > maxOrder {
							continue
						}
					}
					if vs, ok := rm["values"].([]any); ok {
						for _, v := range vs {
							if !slices.Contains(vals, v) { // an enum lists a value once
								vals = append(vals, v)
							}
						}
					}
				}
			}
		case "reasons":
			if reg, err := c.spec.Registry(filepath.Join("spec/model", list+"-reasons.yaml")); err == nil {
				for _, e := range reg.Entries {
					if id, ok := e["id"].(string); ok {
						vals = append(vals, id)
					}
				}
			}
		}
		out[name] = vals
	}
	return out
}

// readBy says whether a catalog resource lists the tool right in its read_by.
func readBy(resource map[string]any, right string) bool {
	rb, _ := resource["read_by"].([]any)
	for _, x := range rb {
		if x == right {
			return true
		}
	}
	return false
}

// fullSchema turns a result schema into the schema the core checks at complete (OUT-1): an
// enum_from leaf gets the enum of the values resolved for the attempt, and x-botaru-format is
// an assertion: the pattern and the length ceiling of the format from spec/model/formats.yaml.
// A leaf with no fitting value, or a format the registry does not have, accepts nothing.
func (c *Core) fullSchema(schema map[string]any, enums map[string][]any) (map[string]any, error) {
	formats, err := c.spec.Registry("spec/model/formats.yaml")
	if err != nil {
		return nil, err
	}
	var conv func(node any) any
	conv = func(node any) any {
		switch x := node.(type) {
		case map[string]any:
			out := make(map[string]any, len(x))
			for k, v := range x {
				out[k] = conv(v)
			}
			if n, ok := x["enum_from"].(string); ok {
				delete(out, "enum_from")
				if vals := enums[n]; len(vals) > 0 {
					out["enum"] = vals
				} else {
					out["not"] = map[string]any{}
				}
			}
			if f, ok := x["x-botaru-format"].(string); ok {
				e, known := formats.Entry(f)
				pat, _ := e["pattern"].(string)
				if !known || pat == "" {
					out["not"] = map[string]any{}
				} else {
					out["type"] = "string"
					out["allOf"] = append(asList(out["allOf"]), map[string]any{"pattern": pat})
					if n, ok := e["max_length"].(int); ok {
						out["allOf"] = append(asList(out["allOf"]), map[string]any{"maxLength": n})
					}
				}
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, v := range x {
				out[i] = conv(v)
			}
			return out
		}
		return node
	}
	m, _ := conv(schema).(map[string]any)
	return m, nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

// prepareAttempt computes the attempt record at claim.
func (c *Core) prepareAttempt(cat *Catalog, w *Worker, tenant, typ, mark, slot, profile string, tools []catalogTool, persona []string) (*attemptRecord, error) {
	fp, parts, err := runFingerprint(cat, w, w.Role, typ, slot, profile, tools, persona)
	if err != nil {
		return nil, err
	}
	rec := &attemptRecord{Fingerprint: fp, Parts: parts, ResultSchema: cat.TaskTypes[typ].ResultSchema}
	_, rec.Catalog = tenantCatalog(cat, tenant)
	names := map[string]bool{}
	enumFromNames(cat.TaskTypes[typ].ResultSchema, names)
	if len(names) > 0 {
		rec.EnumFrom = c.resolveEnumFrom(cat, tenant, mark, names)
	}
	return rec, nil
}

// loadAttempt reads the attempt record of a lease.
func loadAttempt(ctx context.Context, tx pgx.Tx, l *leaseRow) (*attemptRecord, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT attempt FROM leases WHERE work_id = $1 AND lease_no = $2`, l.WorkID, l.LeaseNo).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) || err == nil && len(raw) == 0 {
		return &attemptRecord{}, nil
	}
	if err != nil {
		return nil, err
	}
	var rec attemptRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("attempt record: %w", err)
	}
	return &rec, nil
}
