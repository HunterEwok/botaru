// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Seed is what the bench takes from evals/fixtures/seed.yaml: the people and programs with their
// ids, identities, groups, chats and canaries, the groups, the chats and the tenant canaries.
type Seed struct {
	People  []SeedPerson
	Groups  map[string]map[string]any
	Chats   []SeedChat
	Tenants []string
	// TenantCanaries are the canaries in the data of each tenant (demo2: CANARY-...-T2-...).
	TenantCanaries map[string]string
	Roles          []string
}

// SeedPerson is one principal of the seed.
type SeedPerson struct {
	Name, ID, Tenant, Kind, Identity, Canary string
	Groups, Chats                            []string
	// Subject is false for a person without a subject record (erin).
	Subject bool
}

// SeedChat is one chat of the seed.
type SeedChat struct {
	Name, Tenant, Kind, ExternalID, Ceiling string
}

// LoadSeed reads evals/fixtures/seed.yaml.
func LoadSeed(root string) (*Seed, error) {
	b, err := os.ReadFile(filepath.Join(root, "evals/fixtures/seed.yaml"))
	if err != nil {
		return nil, err
	}
	var raw map[string]any
	if err := yamlx.Unmarshal(b, &raw); err != nil {
		return nil, fmt.Errorf("seed.yaml: %w", err)
	}
	s := &Seed{Groups: map[string]map[string]any{}, TenantCanaries: map[string]string{}, Roles: strs(raw["roles"])}
	for _, t := range asList(raw["tenants"]) {
		s.Tenants = append(s.Tenants, str(asMap(t)["name"]))
	}
	for _, p := range asList(raw["principals"]) {
		m := asMap(p)
		sp := SeedPerson{Name: str(m["name"]), ID: str(m["id"]), Tenant: str(m["tenant"]), Kind: str(m["kind"]), Canary: str(m["canary"]),
			Groups: strs(m["groups"]), Chats: strs(m["chats"]), Subject: m["subject"] != false}
		if ids := strs(m["identities"]); len(ids) > 0 {
			sp.Identity = ids[0]
		}
		s.People = append(s.People, sp)
	}
	for name, g := range asMap(raw["groups"]) {
		s.Groups[name] = asMap(g)
	}
	for _, c := range asList(raw["chats"]) {
		m := asMap(c)
		s.Chats = append(s.Chats, SeedChat{Name: str(m["name"]), Tenant: str(m["tenant"]), Kind: str(m["kind"]), ExternalID: str(m["external_id"]),
			Ceiling: str(m["ceiling"])})
	}
	for t, c := range asMap(asMap(raw["canaries"])["tenant"]) {
		s.TenantCanaries[t] = str(c)
	}
	return s, nil
}

// Person returns a seed principal by name.
func (s *Seed) Person(name string) *SeedPerson {
	for i := range s.People {
		if s.People[i].Name == name {
			return &s.People[i]
		}
	}
	return nil
}

// Chat returns a seed chat by name.
func (s *Seed) Chat(name string) *SeedChat {
	for i := range s.Chats {
		if s.Chats[i].Name == name {
			return &s.Chats[i]
		}
	}
	return nil
}

// Canaries are the canaries a case must not show: the other tenants' and every seed person's
// except the case's own initiator's (evals/README.md, "Common checks").
func (s *Seed) Canaries(from string) []string {
	var out []string
	for t, c := range s.TenantCanaries {
		if t != "demo" {
			out = append(out, c)
		}
	}
	for _, p := range s.People {
		if p.Canary != "" && p.Name != from {
			out = append(out, p.Canary)
		}
	}
	sort.Strings(out)
	return out
}

// GroupManifests writes the seed groups as Group manifests of a tenant. classes are the data
// classes in their order: data_up_to grants every class up to the named one.
func (s *Seed) GroupManifests(tenant string, classes []string, skip []string) []map[string]any {
	names := slices.Sorted(mapKeys(s.Groups))
	var out []map[string]any
	for _, n := range names {
		if slices.Contains(skip, n) {
			continue
		}
		g := s.Groups[n]
		spec := map[string]any{"caps": toAny(strs(g["caps"]))}
		if d := str(g["data_up_to"]); d != "" {
			var data []any
			for _, c := range classes {
				data = append(data, c)
				if c == d {
					break
				}
			}
			spec["data"] = data
		}
		if inh := strs(g["inherits"]); len(inh) > 0 {
			spec["inherits"] = toAny(inh)
		}
		out = append(out, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": n, "tenant": tenant}, "spec": spec})
	}
	return out
}

// telegramUserID is the Telegram user id of a seed identity telegram:<id>.
func telegramUserID(identity string) string {
	id, ok := strings.CutPrefix(identity, "telegram:")
	if !ok {
		return ""
	}
	return id
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}
