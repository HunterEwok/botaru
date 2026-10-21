// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"embed"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// The farm profiles of the stand (botaru-sim -farm): its tenants, the machines of each and the
// workers of each bot role. smoke is the stand of the scenarios, standard the farm of the cloud
// load test.
//
//go:embed farms/*.yaml
var farmFS embed.FS

// Farm is a farm profile.
type Farm struct {
	Name string `yaml:"name"`
	// Tenants are the tenants of the farm; the first is the home of the scenarios.
	Tenants []*FarmTenant `yaml:"tenants"`
}

// FarmTenant is one tenant of a farm profile.
type FarmTenant struct {
	Name  string     `yaml:"name"`
	Hosts []FarmHost `yaml:"hosts"`
	// Manager turns the farm manager on in the tenant (Tenant.spec.farm_manager).
	Manager bool `yaml:"manager"`
	// Evolution gives the tenant the analysis presented to the deciders, the scaffold evolution
	// and its merge gate (D-120) and the dispatcher rule the evolution tunes.
	Evolution bool `yaml:"evolution"`
	// TightBudget marks a tenant whose budget is meant to run out in a load run: only there a
	// stop for money is no failure (the metric M-6).
	TightBudget bool `yaml:"tight_budget"`
	// Replicas are the workers of each bot role in the tenant; nil: one of every role.
	Replicas map[string]int `yaml:"replicas"`
}

// FarmHost is one machine of a tenant: web (nginx and app-api), db (postgresql and the data
// volume of the pipeline), ctd (the CTD quality control runner), mon or geo (a disk only).
type FarmHost struct {
	ID   string `yaml:"id"`
	Kind string `yaml:"kind"`
}

// Host kinds.
const (
	KindWeb = "web"
	KindDB  = "db"
	KindCTD = "ctd"
	KindMon = "mon"
	KindGeo = "geo"
)

var hostKinds = []string{KindWeb, KindDB, KindCTD, KindMon, KindGeo}

// templateHosts are the machines of the example tenant demo by kind: the example catalog names
// them, and each tenant's first machine of a kind takes their place.
var templateHosts = map[string]string{KindWeb: "web1", KindDB: "db1", KindCTD: "ctd1"}

var reFarmName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)

// FarmNames are the names of the embedded farm profiles.
func FarmNames() []string {
	ents, _ := farmFS.ReadDir("farms")
	var out []string
	for _, e := range ents {
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	slices.Sort(out)
	return out
}

// LoadFarm reads an embedded farm profile by name.
func LoadFarm(name string) (*Farm, error) {
	if !slices.Contains(FarmNames(), name) {
		return nil, fmt.Errorf("no farm profile %s (%s)", name, strings.Join(FarmNames(), ", "))
	}
	raw, err := farmFS.ReadFile("farms/" + name + ".yaml")
	if err != nil {
		return nil, err
	}
	var f Farm
	if err := yamlx.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("farm %s: %w", name, err)
	}
	return &f, f.check()
}

// check validates a profile: tenant and machine names, one tenant per machine, known kinds and
// no negative replicas.
func (f *Farm) check() error {
	if len(f.Tenants) == 0 {
		return fmt.Errorf("farm %s: no tenants", f.Name)
	}
	tenants, hosts := map[string]bool{}, map[string]bool{}
	for _, t := range f.Tenants {
		if !reFarmName.MatchString(t.Name) || t.Name == "farm" || tenants[t.Name] {
			return fmt.Errorf("farm %s: bad or repeated tenant name %q", f.Name, t.Name)
		}
		tenants[t.Name] = true
		if t.First(KindDB) == "" {
			return fmt.Errorf("farm %s: tenant %s has no db machine", f.Name, t.Name)
		}
		for _, h := range t.Hosts {
			if !reFarmName.MatchString(h.ID) || hosts[h.ID] {
				return fmt.Errorf("farm %s: bad or repeated machine %q", f.Name, h.ID)
			}
			hosts[h.ID] = true
			if !slices.Contains(hostKinds, h.Kind) {
				return fmt.Errorf("farm %s: machine %s of unknown kind %q", f.Name, h.ID, h.Kind)
			}
		}
		for r, n := range t.Replicas {
			if n < 0 {
				return fmt.Errorf("farm %s: tenant %s: negative replicas of %s", f.Name, t.Name, r)
			}
		}
	}
	return nil
}

// Home is the home tenant: the tenant of the scenarios.
func (f *Farm) Home() *FarmTenant { return f.Tenants[0] }

// Tenant finds a tenant by name.
func (f *Farm) Tenant(name string) *FarmTenant {
	for _, t := range f.Tenants {
		if t.Name == name {
			return t
		}
	}
	return nil
}

// TenantNames are the tenant names, the home first.
func (f *Farm) TenantNames() []string {
	var out []string
	for _, t := range f.Tenants {
		out = append(out, t.Name)
	}
	return out
}

// First is the tenant's first machine of a kind, "" when it has none.
func (t *FarmTenant) First(kind string) string {
	for _, h := range t.Hosts {
		if h.Kind == kind {
			return h.ID
		}
	}
	return ""
}

// Primary says whether a machine is the tenant's first of its kind: it keeps the plain names of
// the example catalog (log sources, services, checks); another machine prefixes them with its id.
func (t *FarmTenant) Primary(host string) bool {
	for _, h := range t.Hosts {
		if h.ID == host {
			return t.First(h.Kind) == host
		}
	}
	return false
}

// Workers is the number of workers of a role in the tenant.
func (t *FarmTenant) Workers(role string) int {
	if t.Replicas == nil {
		return 1
	}
	return t.Replicas[role]
}
