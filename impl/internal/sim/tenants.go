// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/oklog/ulid/v2"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The tenants of a farm profile on the bench: the bench gives each tenant a copy of the example
// tenant demo (its Tenant, channel, catalog, groups and the stand's own manifests of demo), and
// the functions of this file make each copy the tenant's own: its people and chats, its catalog
// of its own machines, its switches, and the toolhosts of its machines.

// templateTenant is the tenant of the example manifests and of the seed people.
const templateTenant = "demo"

// farmSeedTenants gives the seed the people and chats of every tenant of the farm. The people of
// demo move to the home tenant under their own names; every other tenant gets a copy of them
// named <tenant>-<name>, with an id, a Telegram identity, direct and group chats and a canary of
// its own. With more than one tenant each tenant also gets a tenant canary, which its data holds
// and no other tenant may show. The seed tenants become those of the farm and farm.
func farmSeedTenants(sd *evals.Seed, f *Farm) {
	home := f.Home().Name
	var people []evals.SeedPerson
	var chats []evals.SeedChat
	for _, p := range sd.People {
		if p.Tenant == templateTenant {
			p.Tenant = home
		}
		people = append(people, p)
	}
	for _, c := range sd.Chats {
		if c.Tenant == templateTenant {
			c.Tenant = home
		}
		chats = append(chats, c)
	}
	for i, t := range f.Tenants[1:] {
		off := int64(10000 * (i + 1))
		for _, p := range sd.People {
			if p.Tenant != templateTenant || p.Kind != "human" || !p.Subject || p.ID == "" {
				continue
			}
			c := p
			c.Name, c.Tenant, c.ID = t.Name+"-"+p.Name, t.Name, copyID(t.Name, p.Name)
			c.Groups = slices.Clone(p.Groups)
			c.Chats = nil
			for _, ch := range p.Chats {
				c.Chats = append(c.Chats, t.Name+"-"+ch)
			}
			if uid, err := strconv.ParseInt(strings.TrimPrefix(p.Identity, "telegram:"), 10, 64); err == nil {
				c.Identity = "telegram:" + strconv.FormatInt(uid+off, 10)
			}
			if p.Canary != "" {
				c.Canary = strings.Replace(p.Canary, "-P-"+p.Name+"-", "-P-"+c.Name+"-", 1)
			}
			people = append(people, c)
		}
		for _, ch := range sd.Chats {
			if ch.Tenant != templateTenant {
				continue
			}
			c := ch
			c.Name, c.Tenant = t.Name+"-"+ch.Name, t.Name
			if ext, err := strconv.ParseInt(ch.ExternalID, 10, 64); err == nil {
				if ext < 0 {
					c.ExternalID = strconv.FormatInt(ext-off, 10)
				} else {
					c.ExternalID = strconv.FormatInt(ext+off, 10)
				}
			}
			chats = append(chats, c)
		}
	}
	sd.People, sd.Chats = people, chats
	// The smoke farm keeps demo as its home and the seed's demo2 with it, as before the profiles.
	if f.Home().Name == templateTenant {
		for _, n := range f.TenantNames() {
			if !slices.Contains(sd.Tenants, n) {
				sd.Tenants = append(sd.Tenants, n)
			}
		}
	} else {
		sd.Tenants = append([]string{"farm"}, f.TenantNames()...)
	}
	if len(f.Tenants) > 1 {
		if sd.TenantCanaries == nil {
			sd.TenantCanaries = map[string]string{}
		}
		for _, t := range f.Tenants {
			sum := sha256.Sum256([]byte("botaru-sim tenant canary " + t.Name))
			sd.TenantCanaries[t.Name] = fmt.Sprintf("CANARY-NOT-A-SECRET-T-%s-%x", t.Name, sum[:2])
		}
	}
}

// tenantManifests makes each tenant's copy of the demo manifests its own: its catalog names its
// own machines (catalogFor), and its Tenant has the switches of its profile.
func tenantManifests(ms []map[string]any, f *Farm, work string) ([]map[string]any, error) {
	for _, m := range ms {
		md := mapOf(m["metadata"])
		switch m["kind"] {
		case "Catalog":
			name, _ := md["tenant"].(string)
			t := f.Tenant(name)
			if t == nil {
				continue
			}
			sp, err := catalogFor(mapOf(m["spec"]), t)
			if err != nil {
				return nil, err
			}
			m["spec"] = sp
			if err := writeCatalog(work, t.Name, m); err != nil {
				return nil, err
			}
		case "Tenant":
			name, _ := md["name"].(string)
			if t := f.Tenant(name); t != nil {
				tenantSwitches(mapOf(m["spec"]), t)
			}
		}
	}
	return ms, nil
}

// catalogFile is the catalog of a tenant as the monitor plugin reads it.
func catalogFile(work, tenant string) string {
	return filepath.Join(work, "catalogs", tenant+".yaml")
}

// writeCatalog writes the catalog manifest of a tenant for its monitor plugin (JSON is YAML).
func writeCatalog(work, tenant string, m map[string]any) error {
	raw, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(catalogFile(work, tenant)), 0o755); err != nil {
		return err
	}
	return writeAtomic(catalogFile(work, tenant), raw)
}

var reTemplateHost = regexp.MustCompile(`\b(web1|db1|ctd1)\b`)

// catalogFor turns the example catalog of demo into the catalog of a tenant. The tenant's first
// machine of each kind takes the place of web1, db1 and ctd1 with every entry, resource,
// selector and volume of it; an entry of a kind the tenant has no machine of is left out. Every
// further machine gets a host entry, and a web, db or ctd machine its log resource and services
// under names prefixed with its id, so the names stay unique in the tenant.
func catalogFor(sp map[string]any, t *FarmTenant) (map[string]any, error) {
	rename := map[string]string{}
	for kind, h := range templateHosts {
		rename[h] = t.First(kind)
	}
	keep := func(host any) bool {
		h, _ := host.(string)
		return h == "" || rename[h] != "" || !reTemplateHost.MatchString(h)
	}
	filter := func(list []any, field string) []any {
		var out []any
		for _, e := range list {
			if keep(mapOf(e)[field]) {
				out = append(out, e)
			}
		}
		return out
	}
	sp["hosts"] = filter(asSlice(sp["hosts"]), "id")
	sp["resources"] = filter(asSlice(sp["resources"]), "host")
	vols := filter(asSlice(sp["volumes"]), "host")
	for _, v := range vols {
		for _, wr := range asSlice(mapOf(v)["writers"]) {
			if mapOf(wr)["tenant"] == templateTenant {
				mapOf(wr)["tenant"] = t.Name
			}
		}
	}
	if vols == nil {
		delete(sp, "volumes")
	} else {
		sp["volumes"] = vols
	}
	if sel := mapOf(sp["selectors"]); sel != nil {
		for name, v := range sel {
			var hosts []any
			for _, h := range asSlice(mapOf(v)["hosts"]) {
				if keep(h) {
					hosts = append(hosts, h)
				}
			}
			if hosts == nil {
				delete(sel, name)
				continue
			}
			mapOf(v)["hosts"] = hosts
		}
		if len(sel) == 0 {
			delete(sp, "selectors")
		}
	}
	raw, err := json.Marshal(sp)
	if err != nil {
		return nil, err
	}
	text := reTemplateHost.ReplaceAllStringFunc(string(raw), func(h string) string { return rename[h] })
	text = strings.ReplaceAll(text, UnitCTDQCOf(templateTenant), UnitCTDQCOf(t.Name))
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	hosts, resources := asSlice(out["hosts"]), asSlice(out["resources"])
	for _, h := range t.Hosts {
		if t.Primary(h.ID) && templateHosts[h.Kind] != "" {
			continue
		}
		entry := map[string]any{"id": h.ID, "monitor_host": h.ID + ".example.org"}
		logs := func(values ...string) {
			var vs []any
			for _, v := range values {
				vs = append(vs, h.ID+"-"+v)
			}
			resources = append(resources, map[string]any{"id": h.ID + "-logs", "title": "Logs on " + h.ID, "host": h.ID,
				"read_by": []any{"logs/tail"}, "list": "log_sources", "format": "id", "values": vs, "data_class": "raw_logs"})
		}
		switch h.Kind {
		case KindWeb:
			entry["services"] = []any{
				map[string]any{"name": h.ID + "-nginx", "unit": UnitNginx, "control": []any{"start", "stop", "restart"}, "via": "host", "no_card": []any{"restart"}},
				map[string]any{"name": h.ID + "-app-api", "unit": UnitAppAPI, "control": []any{"restart"}, "via": "host"},
			}
			logs("nginx-access", "nginx-error", "app-api")
		case KindDB:
			logs("postgresql")
		case KindCTD:
			entry["services"] = []any{map[string]any{"name": h.ID + "-ctd-qc", "unit": UnitCTDQCOf(t.Name), "control": []any{"start", "stop", "restart"}, "via": "host", "no_card": []any{}}}
		}
		hosts = append(hosts, entry)
	}
	out["hosts"], out["resources"] = hosts, resources
	return out, nil
}

// tenantSwitches sets the switches of a tenant's profile: the farm manager, and for a tenant with
// evolution the analysis presented to the deciders, the scaffold evolution with its merge gate
// (D-120) and the one dispatcher rule the evolution tunes: the attempt deadline of the
// investigations, as in their type (PRP-12).
func tenantSwitches(sp map[string]any, t *FarmTenant) {
	sp["farm_manager"] = map[string]any{"enabled": t.Manager}
	if !t.Evolution {
		return
	}
	ins := mapOf(sp["insights"])
	if ins == nil {
		ins = map[string]any{}
		sp["insights"] = ins
	}
	ins["present_to"] = "group:deciders"
	ins["digest"] = map[string]any{"schedule": "* * * * *", "max_items": 5}
	ins["evolution"] = map[string]any{"enabled": true}
	ins["gate"] = map[string]any{"required": true}
	disp := mapOf(sp["dispatch"])
	if disp == nil {
		disp = map[string]any{}
		sp["dispatch"] = disp
	}
	disp["rules"] = []any{map[string]any{"match": map[string]any{"type": "ops.investigate.v1"},
		"timeouts": map[string]any{"attempt": InvestigateAttempt}}}
}

// hostPlugins are the toolhosts of the machines of a farm. A tenant's first web machine reads the
// web logs and the check results and controls its services; its first db machine reads the
// database log and serves the pipeline queue and the data plugins of the world; its first ctd
// machine serves the pipeline runs. A further machine reads its own logs and controls its own
// services. Every machine runs commands. The monitor plugin has a toolhost of its own (DEP-7),
// on the tenant's first web machine, else its first db machine.
func hostPlugins(cfg Config, f *Farm, roots map[string]string, e evals.HostEnv) []evals.HostPlugins {
	m := func(p string) string { return filepath.Join(cfg.Root, "examples/tools", p, "tool.yaml") }
	logs := func(h, file string) string { return logPath(cfg.Work, h, file) }
	var out []evals.HostPlugins
	for _, t := range f.Tenants {
		for _, fh := range t.Hosts {
			h := fh.ID
			primary := t.Primary(h)
			pre := ""
			if !primary || templateHosts[fh.Kind] == "" {
				pre = h + "-"
			}
			ps := map[string][]string{
				"cmd": {"-manifest", m("cmd"), "-host", h, "-spec-root", cfg.Root, "-program-root", roots[h]},
			}
			service := []string{"-manifest", m("service"), "-systemctl", filepath.Join(roots[h], "usr/bin/systemctl")}
			world := func(kind string) []string {
				return []string{"-manifest", m(kind), "-world", cfg.Work, "-tenant", t.Name}
			}
			switch fh.Kind {
			case KindWeb:
				ps["logs"] = []string{"-manifest", m("logs"), "-source", pre + "nginx-access=" + logs(h, "access.log"),
					"-source", pre + "nginx-error=" + logs(h, "error.log"), "-source", pre + "app-api=" + logs(h, "app-api.log")}
				ps["service"] = service
				if primary {
					ps["checks"] = []string{"-manifest", m("checks"), "-results", checksPath(cfg.Work, h)}
				}
			case KindDB:
				ps["logs"] = []string{"-manifest", m("logs"), "-source", pre + "postgresql=" + logs(h, "postgresql.log")}
				if primary {
					ps["pipeline"] = []string{"-manifest", m("pipeline"), "-queue", queueDir(cfg.Work, t.Name)}
					ps["geo"], ps["storage"], ps["data"] = world("geo"), world("storage"), world("data")
				}
			case KindCTD:
				ps["service"] = service
				if primary {
					ps["batch"] = world("batch")
				}
			}
			out = append(out, evals.HostPlugins{Host: h, Tenant: t.Name, Plugins: ps})
		}
		mon := t.First(KindWeb)
		if mon == "" {
			mon = t.First(KindDB)
		}
		out = append(out, evals.HostPlugins{Host: mon, Tenant: t.Name, Plugins: map[string][]string{
			"monitor": {"-manifest", m("monitor"), "-api", e.MonitorAPI, "-api-token-file", e.MonitorTokenFile, "-catalog", catalogFile(cfg.Work, t.Name), "-tenant", t.Name},
		}})
	}
	return out
}

// TenantNames are the tenants of the stand, the home first.
func (s *Stand) TenantNames() []string {
	if s.farm == nil {
		return []string{s.Home()}
	}
	return s.farm.TenantNames()
}

// LabTenant is the first tenant that runs geo-reader and pipeline-reader, where the lab asks go;
// the home tenant when none does.
func (s *Stand) LabTenant() string {
	if s.farm != nil {
		for _, t := range s.farm.Tenants {
			if t.Workers("geo-reader") > 0 && t.Workers("pipeline-reader") > 0 {
				return t.Name
			}
		}
	}
	return s.Home()
}

// PersonIn is the seed name of a person or chat of demo in a tenant: the name itself in the home
// tenant, <tenant>-<name> in another (farmSeedTenants).
func (s *Stand) PersonIn(tenant, name string) string {
	if tenant == s.Home() {
		return name
	}
	return tenant + "-" + name
}

// TakenBy counts, per tenant and task type, the workers that took an attempt: with replicas of
// a role, the tasks of one type spread over them.
func (s *Stand) TakenBy(ctx context.Context) (map[string]map[string]int, error) {
	rows, err := s.Bench.Store().Pool.Query(ctx, `SELECT t.tenant, t.type, count(DISTINCT l.worker_id) FROM leases l JOIN tasks t ON t.id = l.work_id
		WHERE l.kind = 'attempt' AND t.tenant = ANY($1) GROUP BY 1, 2`, s.TenantNames())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]int{}
	for rows.Next() {
		var tenant, typ string
		var n int
		if err := rows.Scan(&tenant, &typ, &n); err != nil {
			return nil, err
		}
		if out[tenant] == nil {
			out[tenant] = map[string]int{}
		}
		out[tenant][typ] = n
	}
	return out, rows.Err()
}

// copyID is the principal id of a person's copy in a tenant: the same on every call, so the
// stand's and the bench's copies of the seed name the same principal.
func copyID(tenant, name string) string {
	sum := sha256.Sum256([]byte("botaru-sim principal " + tenant + "/" + name))
	return "human:" + ids.Principal + "_" + ulid.MustNew(0, bytes.NewReader(sum[:])).String()
}
