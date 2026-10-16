// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/monitor/zabbix"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
)

// The monitoring system of the bench (docs/quality.md, "Monitoring uses a mock monitoring
// system"): a fake Zabbix JSON-RPC API that knows the problems the fixture lets occur and
// records what is pushed into the trapper items of the farm host, and the reference E5
// adapter (internal/monitor/zabbix) enrolled as the monitor worker of tenant demo. A fixture
// step `occur: {event: {type: ext.monitor.problem.v1, data}}` is brought about as in a
// deployment: the problem is registered in the fake, its webhook goes to the adapter, the
// adapter re-checks it through the API and submits the signal over P1 (MON-5); then the
// adapter serves the monitor_alarm deliveries of the core and pushes `botaru.alarm[<severity>]`,
// which the alarm fact is read from (MON-1, MON-12).

// monitorTarget is the target name the monitoring adapter enrolls with (the core's default).
const monitorTarget = "zabbix"

// catalogKeys are the Catalog spec fields a fixture's given catalog may set; the shorthand
// catalogs of other fixtures (a resource name as a key) stay unchecked.
var catalogKeys = map[string]bool{"hosts": true, "monitor_triggers": true}

// problemFields are the data fields of ext.monitor.problem.v1 the bench can play: values are
// numbers of the problem that the webhook does not carry (the job task reads them from the
// history of the item).
var problemFields = map[string]bool{"eventid": true, "triggerid": true, "resource": true, "severity": true, "state": true, "values": true}

// alarmFields are the fields of the alarm fact: the alarm the monitoring system was given.
var alarmFields = map[string]bool{"type": true, "severity": true, "origin": true, "alarmkey": true, "state": true, "seq": true}

// catalogNeeds says what a given catalog needs that the runner cannot seed.
func catalogNeeds(v any) string {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return "given catalog " + fmt.Sprint(v)
	}
	keys := sortedKeys(m)
	for _, k := range keys {
		if !catalogKeys[k] {
			return "given catalog " + k
		}
	}
	return ""
}

// occurNeeds says what an occur step needs that the runner cannot bring about.
func occurNeeds(v any) string {
	m := asMap(v)
	ev := asMap(m["event"])
	if len(m) != 1 || ev == nil {
		return "step occur"
	}
	if t := str(ev["type"]); t != "ext.monitor.problem.v1" {
		return "step occur event " + t
	}
	for _, k := range sortedKeys(asMap(ev["data"])) {
		if !problemFields[k] {
			return "step occur event data " + k
		}
	}
	return ""
}

// alarmNeeds says what an alarm fact needs that the runner cannot read.
func alarmNeeds(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return "fact alarm " + fmt.Sprint(v)
	}
	for _, k := range sortedKeys(m) {
		if !alarmFields[k] {
			return "fact alarm." + k
		}
	}
	return ""
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// seedCatalog applies the given catalog as the Catalog of the case tenant.
func (x *exec) seedCatalog(ctx context.Context) (*outcome, error) {
	sp := asMap(x.c.Given["catalog"])
	if sp == nil {
		return nil, nil
	}
	x.catalog = deepCopy(sp)
	md := map[string]any{"name": x.tenant()}
	md["tenant"] = x.tenant()
	m := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": md, "spec": x.catalog}
	return x.apply(ctx, []map[string]any{m}, false)
}

// mockE5 is the fake monitoring system.
type mockE5 struct {
	mu sync.Mutex
	// events are the problems by event id as event.get returns them.
	events map[string]map[string]any
	// hosts are the node names of the catalog with a host id each.
	hosts map[string]string
	// pushes are the history.push values in order.
	pushes []pushed
}

type pushed struct{ Key, Value string }

func (z *mockE5) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     int             `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	z.mu.Lock()
	defer z.mu.Unlock()
	var result any
	switch req.Method {
	case "history.push":
		var items []struct{ Key, Value string }
		_ = json.Unmarshal(req.Params, &items)
		data := []any{}
		for _, it := range items {
			z.pushes = append(z.pushes, pushed{it.Key, it.Value})
			data = append(data, map[string]any{"itemid": "1"})
		}
		result = map[string]any{"response": "success", "data": data}
	case "event.get":
		var p struct {
			EventIDs []string `json:"eventids"`
		}
		_ = json.Unmarshal(req.Params, &p)
		out := []any{}
		for _, id := range p.EventIDs {
			if e, ok := z.events[id]; ok {
				out = append(out, e)
			}
		}
		result = out
	case "host.get":
		out := []any{}
		for _, node := range sortedKeys(anyMap(z.hosts)) {
			out = append(out, map[string]any{"hostid": z.hosts[node], "host": node, "maintenance_status": "0", "maintenanceid": "0"})
		}
		result = out
	default:
		// Scripts, maintenance and notes are not played by these fixtures: nothing is there.
		result = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

func anyMap(m map[string]string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// problem registers a problem as the monitoring system knows it: open, or closed by a
// recovery event.
func (z *mockE5) problem(eventid, triggerid, node string, severity int, open bool) {
	z.mu.Lock()
	defer z.mu.Unlock()
	r := "0"
	if !open {
		r = "9" + eventid
	}
	z.events[eventid] = map[string]any{"eventid": eventid, "objectid": triggerid, "severity": strconv.Itoa(severity), "r_eventid": r,
		"hosts": []any{map[string]any{"host": node}}}
}

// alarms are the pushed alarm episodes as their key=value fields.
func (z *mockE5) alarms() []map[string]string {
	z.mu.Lock()
	defer z.mu.Unlock()
	var out []map[string]string
	for _, p := range z.pushes {
		if !strings.HasPrefix(p.Key, "botaru.alarm[") {
			continue
		}
		f := map[string]string{}
		for _, kv := range strings.Fields(p.Value) {
			if k, v, ok := strings.Cut(kv, "="); ok {
				f[k] = v
			}
		}
		out = append(out, f)
	}
	return out
}

// bench5 is the monitoring system and the adapter of one case.
type bench5 struct {
	z       *mockE5
	adapter *zabbix.Adapter
	hook    string
	token   string
	nodes   map[string]string // catalog host id -> node name
}

// e5 starts the monitoring system and the adapter on first use.
func (x *exec) e5(ctx context.Context) (*bench5, error) {
	if x.mon != nil {
		return x.mon, nil
	}
	nodes, hosts, zhosts := map[string]string{}, map[string]string{}, map[string]string{}
	for i, h := range asList(x.catalog["hosts"]) {
		hm := asMap(h)
		id, node := str(hm["id"]), str(hm["monitor_host"])
		if id == "" || node == "" {
			continue
		}
		nodes[id], hosts[node], zhosts[node] = node, id, strconv.Itoa(10501+i)
	}
	status, resp, err := x.post(ctx, x.ep.P2+"/v1/p2/worker.enroll", x.ep.Operators["demo"],
		map[string]any{"tenant": "demo", "role": monitorTarget, "kind": "monitor", "target": monitorTarget})
	if err != nil {
		return nil, err
	}
	if status != 200 {
		o := problemOutcome(status, resp)
		if o.Unimplemented != "" {
			return nil, errUnchecked("core: " + o.Unimplemented)
		}
		return nil, fmt.Errorf("worker.enroll of the monitoring adapter: %s%s", o.Reason, o.why())
	}
	z := &mockE5{events: map[string]map[string]any{}, hosts: zhosts}
	api := httptest.NewServer(z)
	b := &bench5{z: z, token: "hook-" + ids.Secret(16), nodes: nodes}
	b.adapter = zabbix.New(zabbix.Config{
		Core:   &p1client.Client{BaseURL: x.ep.P1, Token: str(resp["token"]), HTTP: x.r.Client},
		Tenant: "demo", Target: monitorTarget, API: api.URL + "/api_jsonrpc.php", APIToken: "bench-monitor-" + ids.Secret(8),
		Hosts: hosts, WebhookToken: b.token, HTTP: x.r.Client, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	hook := httptest.NewServer(b.adapter.Webhook())
	b.hook = hook.URL
	x.cleanup = append(x.cleanup, api.Close, hook.Close)
	if err := b.adapter.Hello(ctx); err != nil {
		return nil, fmt.Errorf("hello of the monitoring adapter: %v", err)
	}
	x.mon = b
	return b, nil
}

// occur brings about an event the fixture states as an outcome.
func (x *exec) occur(ctx context.Context, v any) (*outcome, error) {
	data := asMap(asMap(asMap(v)["event"])["data"])
	b, err := x.e5(ctx)
	if err != nil {
		return nil, err
	}
	node := b.nodes[str(data["resource"])]
	if node == "" {
		return nil, errUnchecked("a problem on a resource without a monitor_host in the given catalog")
	}
	sev, _ := strconv.Atoi(fmt.Sprint(data["severity"]))
	open := str(data["state"]) != "resolved"
	b.z.problem(str(data["eventid"]), str(data["triggerid"]), node, sev, open)
	value := "1"
	if !open {
		value = "0"
	}
	body, _ := json.Marshal(map[string]any{"eventid": str(data["eventid"]), "triggerid": str(data["triggerid"]), "host": node, "value": value})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.hook, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := x.r.Client.Do(req)
	if err != nil {
		return nil, err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		return nil, fmt.Errorf("the problem webhook answered %d", resp.StatusCode)
	}
	// The adapter serves what the core handed it: the alarm episodes to export.
	for range 64 {
		more, err := b.adapter.ServeOne(ctx, 0)
		if err != nil {
			return nil, fmt.Errorf("the monitoring adapter: %v", err)
		}
		if !more {
			break
		}
	}
	return &outcome{Result: "ok"}, nil
}

// alarmFact compares the alarm fact with the last alarm episode the monitoring system got of
// the alarm type the fact names (or the last one when it names none).
func (x *exec) alarmFact(want map[string]any) string {
	if x.mon == nil {
		return "alarm: no monitoring system was involved"
	}
	var got map[string]string
	for _, a := range x.mon.z.alarms() {
		if t := str(want["type"]); t == "" || "alarm."+a["alarm"]+".v1" == t {
			got = a
		}
	}
	if got == nil {
		return fmt.Sprintf("alarm: no %s reached the monitoring system", str(want["type"]))
	}
	have := map[string]string{"type": "alarm." + got["alarm"] + ".v1", "severity": got["severity"], "origin": got["origin"],
		"alarmkey": strings.TrimPrefix(got["key"], got["tenant"]+":"), "state": got["state"], "seq": got["seq"]}
	for _, k := range sortedKeys(want) {
		if w := fmt.Sprint(want[k]); have[k] != w {
			return fmt.Sprintf("alarm.%s %q, expected %q", k, have[k], w)
		}
	}
	return ""
}
