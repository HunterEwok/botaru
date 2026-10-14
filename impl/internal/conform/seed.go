// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"os"
	"path/filepath"
	"sort"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// seedData is what the runner takes from evals/fixtures/seed.yaml: the people with their ids,
// identities and groups, and the groups with their permissions.
type seedData struct {
	people []seedPerson
	groups map[string]map[string]any
	// classes are the data classes in their order (summary < internal < raw_logs, PRM-5).
	classes []string
}

// setClasses takes the data classes and their order from the capabilities registry.
func (s *seedData) setClasses(entries []map[string]any) {
	type cls struct {
		id    string
		order int
	}
	var cs []cls
	for _, e := range entries {
		if str(e["kind"]) == "data_class" {
			o, _ := e["order"].(int)
			cs = append(cs, cls{str(e["id"]), o})
		}
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].order < cs[j].order })
	s.classes = nil
	for _, c := range cs {
		s.classes = append(s.classes, c.id)
	}
}

// upTo returns every data class up to and including top.
func (s *seedData) upTo(top string) []any {
	var out []any
	for _, c := range s.classes {
		out = append(out, c)
		if c == top {
			return out
		}
	}
	return []any{top}
}

type seedPerson struct {
	Name, ID, Tenant, Kind, Identity string
	Groups                           []string
}

func loadSeed(root string) (*seedData, error) {
	b, err := os.ReadFile(filepath.Join(root, "evals/fixtures/seed.yaml"))
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := yamlx.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	s := &seedData{groups: map[string]map[string]any{}}
	for _, p := range asList(raw["principals"]) {
		pm := asMap(p)
		sp := seedPerson{Name: str(pm["name"]), ID: str(pm["id"]), Tenant: str(pm["tenant"]), Kind: str(pm["kind"]), Groups: strs(pm["groups"])}
		if idents := strs(pm["identities"]); len(idents) > 0 {
			sp.Identity = idents[0]
		}
		s.people = append(s.people, sp)
	}
	for name, g := range asMap(raw["groups"]) {
		s.groups[name] = asMap(g)
	}
	return s, nil
}

// baseGroups are the groups the hello-bot set already declares.
var baseGroups = map[string]bool{"hello": true}

// groupManifests writes the groups of the seed as Group manifests of a tenant.
func (s *seedData) groupManifests(owner string) []map[string]any {
	names := make([]string, 0, len(s.groups))
	for n := range s.groups {
		if !baseGroups[n] {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	var out []map[string]any
	for _, n := range names {
		g := s.groups[n]
		// The seed writes the highest data class a group sees as data_up_to, which means every
		// class up to it; a group without it sees no data class.
		spec := map[string]any{"caps": toAny(strs(g["caps"]))}
		if d := str(g["data_up_to"]); d != "" {
			spec["data"] = s.upTo(d)
		}
		if inh := strs(g["inherits"]); len(inh) > 0 {
			spec["inherits"] = toAny(inh)
		}
		md := map[string]any{"name": n}
		md["tenant"] = owner
		out = append(out, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": md, "spec": spec})
	}
	return out
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}

func newPrincipalID() string { return ids.PrincipalID() }
