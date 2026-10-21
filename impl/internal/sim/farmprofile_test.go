// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// The embedded profiles load: smoke is demo with web1, db1 and ctd1 and one worker of each role,
// standard four tenants on sixteen machines with their replicas.
func TestFarmProfiles(t *testing.T) {
	smoke, err := LoadFarm("smoke")
	if err != nil {
		t.Fatal(err)
	}
	if smoke.Home().Name != "demo" || len(smoke.Tenants) != 1 || smoke.Home().Workers("ops-reader") != 1 {
		t.Fatalf("smoke %+v", smoke.Home())
	}
	std, err := LoadFarm("standard")
	if err != nil {
		t.Fatal(err)
	}
	hosts := 0
	for _, tn := range std.Tenants {
		hosts += len(tn.Hosts)
	}
	if len(std.Tenants) != 4 || hosts != 16 || std.Home().Workers("ops-reader") != 6 || std.Tenant("small-dept").Workers("farm-manager") != 0 ||
		std.Home().Workers("translator") != 1 || std.Tenant("lab-dept").Workers("translator") != 2 || std.Home().Workers("ops-operator") != 1 {
		t.Fatalf("standard: %d tenants, %d machines", len(std.Tenants), hosts)
	}
	if _, err := LoadFarm("nope"); err == nil {
		t.Fatal("an unknown profile loaded")
	}
	bad := &Farm{Name: "x", Tenants: []*FarmTenant{{Name: "a", Hosts: []FarmHost{{ID: "db1", Kind: KindDB}}}, {Name: "b", Hosts: []FarmHost{{ID: "db1", Kind: KindDB}}}}}
	if bad.check() == nil {
		t.Fatal("a machine of two tenants passed")
	}
}

// The world of a farm gives each machine its tenant, kind and monitoring id; a plain fault acts
// on the home tenant's first machine of its kind only.
func TestFarmWorld(t *testing.T) {
	std, err := LoadFarm("standard")
	if err != nil {
		t.Fatal(err)
	}
	w := NewFarmWorld(std, time.Now())
	if len(w.Hosts) != 16 || w.Hosts["db3"].Tenant != "lab-dept" || w.Hosts["web1"].ZabbixID != "10501" || w.First("lab-dept", KindCTD) != "ctd2" {
		t.Fatalf("world %+v", w.Hosts["db3"])
	}
	if w.Service("ctd3", UnitCTDQCOf("lab-dept")) == nil {
		t.Fatal("ctd3 has no runner of its tenant")
	}
	w.Faults[FaultSlowSite] = true
	w.Faults[FaultDiskFilling+":db4"] = true
	if !w.Fault(FaultSlowSite, "web1") || w.Fault(FaultSlowSite, "web2") || w.Fault(FaultSlowSite, "web5") || !w.Fault(FaultDiskFilling, "db4") || w.Fault(FaultDiskFilling, "db1") {
		t.Fatal("faults act on the wrong machines")
	}
	smoke := NewWorld(time.Now())
	if smoke.Hosts["ctd1"].Services[UnitCTDQC] == nil || smoke.Home != "demo" {
		t.Fatal("the smoke world lost its runner")
	}
}

// A tenant's catalog names its own machines: its first db machine takes db1's resources, a kind
// it lacks is left out, and a further machine gets prefixed names.
func TestCatalogFor(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/tenants/demo/catalog.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	std, err := LoadFarm("standard")
	if err != nil {
		t.Fatal(err)
	}
	sp, err := catalogFor(mapOf(evalsCopy(docs[0].Value)["spec"]), std.Tenant("lab-dept"))
	if err != nil {
		t.Fatal(err)
	}
	var hosts, res []string
	for _, h := range asSlice(sp["hosts"]) {
		hosts = append(hosts, mapOf(h)["id"].(string))
	}
	for _, r := range asSlice(sp["resources"]) {
		res = append(res, mapOf(r)["id"].(string)+"@"+fmtHost(mapOf(r)["host"]))
	}
	if !slices.Equal(hosts, []string{"db3", "ctd2", "ctd3", "geo1"}) {
		t.Errorf("hosts %v", hosts)
	}
	for _, want := range []string{"db3-logs@db3", "ctd-stations@ctd2", "geo-data@db3"} {
		if !slices.Contains(res, want) {
			t.Errorf("no %s in %v", want, res)
		}
	}
	for _, r := range res {
		if strings.Contains(r, "web") || strings.Contains(r, "db1") || strings.Contains(r, "ctd1") {
			t.Errorf("a resource of another machine: %s", r)
		}
	}
	ops, err := catalogFor(mapOf(evalsCopy(docs[0].Value)["spec"]), std.Tenant("ops-dept"))
	if err != nil {
		t.Fatal(err)
	}
	var svc []string
	for _, h := range asSlice(ops["hosts"]) {
		for _, s := range asSlice(mapOf(h)["services"]) {
			svc = append(svc, mapOf(s)["name"].(string))
		}
	}
	if !slices.Contains(svc, "nginx") || !slices.Contains(svc, "web2-nginx") || !slices.Contains(svc, "ctd-qc") {
		t.Errorf("services %v", svc)
	}
}

func fmtHost(v any) string { s, _ := v.(string); return s }

// evalsCopy copies a manifest value, so a test may change it.
func evalsCopy(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = copyValue(v)
	}
	return out
}

func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return evalsCopy(x)
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = copyValue(e)
		}
		return out
	}
	return v
}

// The people of demo move to the home tenant; every other tenant gets its own copies with an
// identity, chats and canaries of its own.
func TestFarmSeedTenants(t *testing.T) {
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	sd, err := evals.LoadSeed(root)
	if err != nil {
		t.Fatal(err)
	}
	std, err := LoadFarm("standard")
	if err != nil {
		t.Fatal(err)
	}
	farmSeedTenants(sd, std)
	alice, labAlice := sd.Person("alice"), sd.Person("lab-dept-alice")
	if alice == nil || alice.Tenant != "ops-dept" || labAlice == nil || labAlice.Tenant != "lab-dept" || labAlice.Identity == alice.Identity || labAlice.ID == alice.ID {
		t.Fatalf("alice %+v, lab alice %+v", alice, labAlice)
	}
	if c := sd.Chat("lab-dept-alice-dm"); c == nil || c.Tenant != "lab-dept" || "telegram:"+c.ExternalID != labAlice.Identity {
		t.Fatalf("chat %+v", c)
	}
	if !slices.Contains(sd.Tenants, "other-dept") || slices.Contains(sd.Tenants, "demo") || sd.TenantCanaries["small-dept"] == "" {
		t.Fatalf("tenants %v, canaries %v", sd.Tenants, sd.TenantCanaries)
	}
	if cs := sd.Canaries("lab-dept-alice"); slices.Contains(cs, sd.TenantCanaries["lab-dept"]) || !slices.Contains(cs, sd.TenantCanaries["ops-dept"]) {
		t.Fatalf("canaries of lab-dept-alice %v", cs)
	}
}
