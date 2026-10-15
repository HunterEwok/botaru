// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"fmt"
	"path"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// forbiddenPrograms reads the program base names of spec/model/forbidden-programs.yaml.
func (c *Core) forbiddenPrograms() (map[string]bool, error) {
	reg, err := c.spec.Registry("spec/model/forbidden-programs.yaml")
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range reg.Entries {
		if id, ok := e["id"].(string); ok {
			out[id] = true
		}
	}
	return out, nil
}

// checkPlugin runs the plugin checks that the schema cannot express on manifest i: a call
// target (TEN-3a), redaction of untrusted output on a machine (INJ-4) and program commands
// without a shell (INJ-14).
func (c *Core) checkPlugin(d manifest.Doc, i int) ([]ApplyError, error) {
	spec, _ := d.Value["spec"].(map[string]any)
	p, err := toolspec.Decode(d.Name, spec)
	if err != nil {
		return []ApplyError{{Kind: d.Kind, Name: d.Name, Code: "decode_failed", Path: fmt.Sprintf("/manifests/%d/spec", i)}}, nil
	}
	forbidden, err := c.forbiddenPrograms()
	if err != nil {
		return nil, err
	}
	var out []ApplyError
	for _, is := range toolspec.Check(p, forbidden) {
		j := slices.IndexFunc(p.Tools, func(t toolspec.Tool) bool { return t.Name == is.Tool })
		out = append(out, ApplyError{Kind: d.Kind, Name: d.Name, Code: is.Code, Path: fmt.Sprintf("/manifests/%d/spec/tools/%d", i, j)})
	}
	return out, nil
}

// catalogHosts returns the machine ids of a tenant's catalog.
func catalogHosts(cat *Catalog, tenant string) []string {
	spec, _ := tenantCatalog(cat, tenant)
	hosts, _ := spec["hosts"].([]any)
	var out []string
	for _, h := range hosts {
		if id, ok := h.(map[string]any)["id"].(string); ok {
			out = append(out, id)
		}
	}
	return out
}

// checkToolhostEnroll checks the enroll record of a toolhost: plugins of the farm with this
// placement, and for placement host a machine of the tenant's catalog (DEP-2, TEN-4, D-75).
func (c *Core) checkToolhostEnroll(cat *Catalog, req EnrollRequest) error {
	if len(req.Plugins) == 0 || req.Placement == "" {
		return refuse("input_schema_invalid", "a toolhost needs plugins and a placement")
	}
	for _, name := range req.Plugins {
		p, ok := cat.Plugins[name]
		if !ok {
			return refuse("not_found", "no plugin %s", name)
		}
		if p.Placement != req.Placement {
			return refuse("input_schema_invalid", "plugin %s has placement %s", name, p.Placement)
		}
		// Only an own plugin without secrets and network runs as a child process of a shared
		// toolhost (DEP-7, INJ-6). The core cannot see a container, so a plugin with secrets or
		// network is refused, except the farm's own read plugin monitor (MON-6) alone in a
		// toolhost of its own, which the deployment runs under its own OS user.
		if len(p.Secrets) > 0 || len(p.Network.Egress) > 0 {
			if name != "monitor" {
				return refuse("input_schema_invalid", "plugin %s has secrets or network egress; it runs in a container the toolhost does not provide (DEP-7)", name)
			}
			if len(req.Plugins) > 1 {
				return refuse("input_schema_invalid", "plugin %s has secrets or network egress and needs a toolhost of its own (DEP-7)", name)
			}
		}
	}
	switch req.Placement {
	case "host":
		if req.Host == "" {
			return refuse("input_schema_invalid", "a toolhost of placement host needs the catalog machine")
		}
		if !slices.Contains(catalogHosts(cat, req.Tenant), req.Host) {
			return refuse("not_found", "no machine %s in the catalog of tenant %s", req.Host, req.Tenant)
		}
	case "sandbox":
		if req.Host != "" {
			return refuse("input_schema_invalid", "a sandbox toolhost has no catalog machine")
		}
	}
	return nil
}

// expectedFingerprint is the tools/list fingerprint that the plugins of a toolhost give for
// their manifests, combined as P1 hello carries it.
func expectedFingerprint(cat *Catalog, plugins []string) (string, error) {
	by := map[string]string{}
	for _, name := range plugins {
		p, ok := cat.Plugins[name]
		if !ok {
			return "", fmt.Errorf("no plugin %s", name)
		}
		fp, err := toolspec.Fingerprint(p.LiveTools())
		if err != nil {
			return "", err
		}
		by[name] = fp
	}
	return toolspec.CombinedFingerprint(by)
}

// toolhostMismatch says why the hello of a toolhost does not match its enroll record and the
// manifests, or "" when it matches (DEP-2, INJ-2).
func toolhostMismatch(cat *Catalog, w *Worker, req HelloRequest) string {
	got := slices.Clone(req.Plugins)
	slices.Sort(got)
	if !slices.Equal(got, w.Plugins) || req.Placement != w.Placement {
		return "the plugins or the placement differ from the enroll record"
	}
	want, err := expectedFingerprint(cat, w.Plugins)
	if err != nil {
		return "a plugin of the enroll record has no manifest"
	}
	if req.ToolsFingerprint != want {
		return "the live tools/list differs from the plugin manifests (INJ-2)"
	}
	return ""
}

// enrollAfter is the after field of the worker.enroll audit record.
func enrollAfter(w Worker) map[string]any {
	a := map[string]any{"kind": w.Kind, "role": w.Role}
	if w.Kind == "toolhost" {
		a["plugins"], a["placement"] = w.Plugins, w.Placement
		if w.Host != "" {
			a["host"] = w.Host
		}
	}
	return a
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// roleTools expands the tool references of a role: <plugin>/<tool>, or <plugin> for every
// tool of the plugin. A reference to an unknown plugin or tool gives nothing.
func roleTools(cat *Catalog, role manifest.Role) []string {
	var out []string
	for _, x := range role.Tools {
		ref, _ := x.(string)
		plugin, tool, whole := cutTool(ref)
		p, ok := cat.Plugins[plugin]
		if !ok {
			continue
		}
		for _, t := range p.Tools {
			if whole || t.Name == tool {
				name := toolspec.RightName(plugin, t.Name)
				if !slices.Contains(out, name) {
					out = append(out, name)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

func cutTool(ref string) (plugin, tool string, whole bool) {
	dir, file := path.Split(ref)
	if dir == "" {
		return ref, "", true
	}
	return dir[:len(dir)-1], file, false
}
