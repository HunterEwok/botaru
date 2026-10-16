// SPDX-License-Identifier: AGPL-3.0-only

package zabbix

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// fakeAccount answers user.get, role.get, host.get and script.getscriptsbyhosts for one
// account, the way the Zabbix 7.0 JSON-RPC API does.
type fakeAccount struct {
	roleType string
	actions  map[string]string
	ui       map[string]string
	// hosts are the nodes the account sees, by name, with their host ids; scripts the hosts
	// with a script the account may run.
	hosts   map[string]string
	scripts map[string]bool
}

func (z *fakeAccount) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     int             `json:"id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	var result any
	rules := func(m map[string]string) []any {
		out := []any{}
		for k, v := range m {
			out = append(out, map[string]any{"name": k, "status": v})
		}
		return out
	}
	switch req.Method {
	case "user.get":
		result = []any{map[string]any{"userid": "7", "roleid": "11", "role": map[string]any{"roleid": "11", "type": z.roleType}}}
	case "role.get":
		result = []any{map[string]any{"roleid": "11", "type": z.roleType, "rules": map[string]any{
			"ui": rules(z.ui), "ui.default_access": "0", "actions": rules(z.actions), "actions.default_access": "0"}}}
	case "host.get":
		// The account reads every host it sees, without a filter.
		out := []any{}
		for _, n := range sortedKeys(z.hosts) {
			out = append(out, map[string]any{"hostid": z.hosts[n], "host": n})
		}
		result = out
	case "script.getscriptsbyhosts":
		var hosts []struct {
			HostID string `json:"hostid"`
		}
		_ = json.Unmarshal(req.Params, &hosts)
		out := map[string]any{}
		for _, h := range hosts {
			list := []any{}
			if z.scripts[h.HostID] {
				list = append(list, map[string]any{"scriptid": "3", "name": "restart " + canary, "scope": "2"})
			}
			out[h.HostID] = list
		}
		result = out
	default:
		result = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// MON-4, OBS-11: the adapter reads its own user's role and scope through the API and reports
// the rights wider than the rule allows by their rule names, and the hosts it sees and may run
// scripts on only as hashes of their names: the right to acknowledge problems; a demo account
// that sees a demo2 node; a script on the farm node core1. No Zabbix name leaves the adapter.
func TestAccountReport(t *testing.T) {
	ctx := context.Background()
	report := func(z *fakeAccount) map[string]any {
		t.Helper()
		srv := httptest.NewServer(z)
		defer srv.Close()
		got, err := New(Config{API: srv.URL, APIToken: "t"}).AccountReport(ctx)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		if strings.Contains(string(raw), "example.org") || strings.Contains(string(raw), canary) || strings.Contains(string(raw), "core1") {
			t.Fatalf("a Zabbix name left the adapter: %s", raw)
		}
		return got
	}
	narrow := func() *fakeAccount {
		return &fakeAccount{roleType: "1", actions: map[string]string{"add_problem_comments": "1", "execute_scripts": "1", "acknowledge_problems": "0"},
			hosts: map[string]string{"web1.example.org": "10501"}, scripts: map[string]bool{"10501": true}}
	}
	got := report(narrow())
	if f := got["findings"].([]string); len(f) != 0 {
		t.Fatalf("MON-4: a narrow account %v", f)
	}
	if v := got["visible"].([]string); !slices.Equal(v, []string{NodeHash("web1.example.org")}) || !slices.Equal(got["scripts"].([]string), v) {
		t.Fatalf("MON-4: visible %v scripts %v", v, got["scripts"])
	}
	z := narrow()
	z.actions["acknowledge_problems"] = "1"
	if f := report(z)["findings"].([]string); !slices.Equal(f, []string{"acknowledge_problems"}) {
		t.Fatalf("MON-4: the right to acknowledge %v", f)
	}
	z = narrow()
	z.hosts["web2.example.org"] = "10601"
	if v := report(z)["visible"].([]string); !slices.Contains(v, NodeHash("web2.example.org")) || len(v) != 2 {
		t.Fatalf("MON-4: demo sees a demo2 node %v", v)
	}
	z = narrow()
	z.hosts["core1"], z.scripts["10001"] = "10001", true
	if s := report(z)["scripts"].([]string); !slices.Contains(s, NodeHash("core1")) {
		t.Fatalf("MON-4: a script on core1 %v", s)
	}
	// An Admin role of the farm process: the notification actions section is wider; a role of
	// type User has no such section, whatever its default.
	farm := &fakeAccount{roleType: "2", actions: map[string]string{}, ui: map[string]string{"data_collection.hosts": "1", "alerts.actions": "1"}, hosts: map[string]string{}}
	if f := report(farm)["findings"].([]string); !slices.Equal(f, []string{"alerts.actions"}) {
		t.Fatalf("MON-4: an Admin with notification actions %v", f)
	}
	user := &fakeAccount{roleType: "1", ui: map[string]string{"alerts.actions": "1"}, hosts: map[string]string{}}
	if f := report(user)["findings"].([]string); len(f) != 0 {
		t.Fatalf("MON-4: a User role %v", f)
	}
}
