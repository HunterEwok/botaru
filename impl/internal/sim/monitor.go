// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Monitor is the monitoring system of the world: a Zabbix 7.0 JSON-RPC API over world.json and
// the metric series. It serves the monitor plugin (problems, items, history, maintenance) and
// the adapter botaru-zabbix (event.get, history.push of the alarm export and the audit head, the
// account check, the scripts of each tenant's first web machine and their runs, the
// acknowledgements), and sends the problem webhook to the adapter of the machine's tenant when
// the physics opens or closes a problem.
type Monitor struct {
	Work string
	// Hooks are the addresses of the adapters' webhooks by tenant and HookToken their token; a
	// tenant without one gets no webhook.
	Hooks     map[string]string
	HookToken string
	// OnPush sees each history.push item; OnScript each script run, OnAck each note.
	OnPush   func(key, value string)
	OnScript func(host, scriptid, input string)
	OnAck    func(eventid, message string)

	mu sync.Mutex
	// downUntil makes the API answer 503 until then (F-5) and twice sends the next problem
	// webhook twice (F-6); refused and repeated count them.
	downUntil         time.Time
	twice             bool
	refused, repeated int
	// doubled is the event of the last webhook that went out twice.
	doubled string
	served  int
}

// Served counts the API requests the monitor answered.
func (m *Monitor) Served() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.served
}

// Down makes the API answer 503 for a time (F-5).
func (m *Monitor) Down(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.downUntil = time.Now().Add(d)
}

// Twice sends the next webhook of an opened problem twice (F-6).
func (m *Monitor) Twice() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.twice = true
}

// Doubled is the event of the last problem webhook sent twice, "" for none.
func (m *Monitor) Doubled() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.doubled
}

// FaultCounts are the API requests refused while down and the webhooks sent twice.
func (m *Monitor) FaultCounts() (refused, repeated int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refused, m.repeated
}

// The scripts of a tenant's first web machine the catalog allows (examples/tenants/demo/catalog.yaml).
const (
	scriptAppRestart = "17"
	scriptCacheClear = "21"
)

// scriptHost says whether a machine has the monitoring scripts: a tenant's first web machine.
func scriptHost(w *World, host string) bool {
	h := w.Hosts[host]
	return h != nil && h.Kind == KindWeb && w.First(h.Tenant, KindWeb) == host
}

// zid is the monitoring host id of a machine.
func zid(w *World, host string) string {
	if h := w.Hosts[host]; h != nil {
		return h.ZabbixID
	}
	return ""
}

// itemIDs number the series keys of a world: the series in key order, from 30001.
func itemIDs(w *World) map[string]string {
	keys := make([]string, 0, len(w.Series))
	for k := range w.Series {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := make(map[string]string, len(keys))
	for i, k := range keys {
		out[k] = strconv.Itoa(30001 + i)
	}
	return out
}

func (m *Monitor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	down := time.Now().Before(m.downUntil)
	if down {
		m.refused++
	} else {
		m.served++
	}
	m.mu.Unlock()
	if down {
		http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
		return
	}
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
		ID     any             `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	result, err := m.call(req.Method, req.Params)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "error": map[string]any{"code": -32602, "message": "Invalid params.", "data": err.Error()}})
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// strList reads a parameter that is a string or a list of strings.
func strList(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case []any:
		var out []string
		for _, e := range x {
			out = append(out, fmt.Sprint(e))
		}
		return out
	}
	return nil
}

func int64Param(v any) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case string:
		n, _ := strconv.ParseInt(x, 10, 64)
		return n
	}
	return 0
}

func (m *Monitor) call(method string, raw json.RawMessage) (any, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var p map[string]any
	_ = json.Unmarshal(raw, &p)
	switch method {
	case "user.checkAuthentication":
		return map[string]any{"userid": "7", "roleid": "4", "type": "1"}, nil
	case "user.get":
		return []any{map[string]any{"userid": "7", "roleid": "4"}}, nil
	case "role.get":
		return []any{map[string]any{"roleid": "4", "type": "1", "rules": map[string]any{"ui": []any{}, "ui.default_access": "0",
			"actions": []any{}, "actions.default_access": "0"}}}, nil
	case "history.push":
		var items []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		}
		_ = json.Unmarshal(raw, &items)
		data := []any{}
		for _, it := range items {
			if m.OnPush != nil {
				m.OnPush(it.Key, it.Value)
			}
			data = append(data, map[string]any{"itemid": "1"})
		}
		return map[string]any{"response": "success", "data": data}, nil
	case "event.acknowledge":
		ids := strList(p["eventids"])
		msg, _ := p["message"].(string)
		for _, id := range ids {
			if m.OnAck != nil {
				m.OnAck(id, msg)
			}
		}
		return map[string]any{"eventids": ids}, nil
	}
	w, err := LoadWorld(m.Work)
	if err != nil {
		return nil, err
	}
	switch method {
	case "script.getscriptsbyhosts":
		var hosts []struct {
			HostID string `json:"hostid"`
		}
		if json.Unmarshal(raw, &hosts) != nil || len(hosts) == 0 {
			return nil, fmt.Errorf("an array of hosts is expected")
		}
		out := map[string]any{}
		for _, h := range hosts {
			if scriptHost(w, w.HostOfID(h.HostID)) {
				out[h.HostID] = []any{
					map[string]any{"scriptid": scriptAppRestart, "name": "Restart app-api", "scope": "2", "manualinput": "0"},
					map[string]any{"scriptid": scriptCacheClear, "name": "Clear the nginx cache", "scope": "2", "manualinput": "1",
						"manualinput_validator_type": "1", "manualinput_validator": "all,static"},
				}
			} else {
				out[h.HostID] = []any{}
			}
		}
		return out, nil
	case "host.get":
		return hostGet(w, p), nil
	case "event.get":
		var out []any
		for _, id := range strList(p["eventids"]) {
			for _, pr := range w.Problems {
				if pr.EventID == id {
					out = append(out, eventOf(w, pr))
				}
			}
		}
		if out == nil {
			out = []any{}
		}
		return out, nil
	case "problem.get":
		hosts := strList(p["hostids"])
		from := int64Param(p["time_from"])
		out := []any{}
		for i := len(w.Problems) - 1; i >= 0; i-- {
			pr := w.Problems[i]
			if len(hosts) > 0 && !slices.Contains(hosts, zid(w, pr.Host)) || pr.Clock < from {
				continue
			}
			e := eventOf(w, pr)
			acks := []any{}
			for _, c := range pr.Comments {
				acks = append(acks, map[string]any{"clock": strconv.FormatInt(pr.Clock, 10), "message": c})
			}
			e["acknowledges"] = acks
			out = append(out, e)
		}
		return out, nil
	case "item.get":
		ids := itemIDs(w)
		hosts := strList(p["hostids"])
		filter, _ := p["filter"].(map[string]any)
		keys := strList(filter["key_"])
		out := []any{}
		for _, k := range slices.Sorted(maps.Keys(ids)) {
			host, key, _ := strings.Cut(k, "|")
			if len(hosts) > 0 && !slices.Contains(hosts, zid(w, host)) || len(keys) > 0 && !slices.Contains(keys, key) {
				continue
			}
			out = append(out, map[string]any{"itemid": ids[k], "hostid": zid(w, host), "key_": key, "value_type": "0"})
		}
		return out, nil
	case "history.get", "trend.get":
		ids := itemIDs(w)
		want := strList(p["itemids"])
		from, till := int64Param(p["time_from"]), int64Param(p["time_till"])
		if till == 0 {
			till = time.Now().Unix() + 1
		}
		out := []any{}
		for k, id := range ids {
			if !slices.Contains(want, id) {
				continue
			}
			s := w.Series[k]
			if method == "trend.get" {
				for _, h := range hourly(s, from, till) {
					out = append(out, map[string]any{"itemid": id, "clock": strconv.FormatInt(h.Clock, 10), "value_avg": strconv.FormatFloat(h.Value, 'f', 4, 64)})
				}
				continue
			}
			for i := len(s) - 1; i >= 0; i-- {
				if s[i].Clock < from || s[i].Clock > till {
					continue
				}
				out = append(out, map[string]any{"itemid": id, "clock": strconv.FormatInt(s[i].Clock, 10), "value": strconv.FormatFloat(s[i].Value, 'f', 4, 64)})
			}
		}
		return out, nil
	case "script.execute":
		host := w.HostOfID(fmt.Sprint(p["hostid"]))
		id := fmt.Sprint(p["scriptid"])
		input, _ := p["manualinput"].(string)
		if !scriptHost(w, host) || id != scriptAppRestart && id != scriptCacheClear {
			return nil, fmt.Errorf("no such script on the host")
		}
		if m.OnScript != nil {
			m.OnScript(host, id, input)
		}
		value := "nginx cache cleared"
		if id == scriptAppRestart {
			err = Update(m.Work, func(w *World) error {
				s := w.Service(host, UnitAppAPI)
				s.Restarts++
				before := s.State
				s.State, s.Changed = "active", time.Now().UTC()
				w.Note("service", host, "monitoring script %s restarted %s: %s -> active (restarts %d)", id, UnitAppAPI, before, s.Restarts)
				return nil
			})
			value = "app-api restarted"
		} else {
			err = Update(m.Work, func(w *World) error {
				w.Note("service", host, "monitoring script %s cleared the nginx cache (%s)", id, input)
				return nil
			})
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"response": "success", "value": value}, nil
	}
	return nil, fmt.Errorf("the method %s is not served", method)
}

// hostGet answers host.get: the world's nodes, filtered by host name or id.
func hostGet(w *World, p map[string]any) []any {
	filter, _ := p["filter"].(map[string]any)
	names := strList(filter["host"])
	ids := strList(p["hostids"])
	out := []any{}
	for _, h := range w.HostNames() {
		node := w.Hosts[h].Node
		if len(names) > 0 && !slices.Contains(names, node) || len(ids) > 0 && !slices.Contains(ids, zid(w, h)) {
			continue
		}
		out = append(out, map[string]any{"hostid": zid(w, h), "host": node, "maintenance_status": "0", "maintenance_from": "0", "maintenanceid": "0"})
	}
	return out
}

// eventOf is a problem as event.get and problem.get return it.
func eventOf(w *World, p *Problem) map[string]any {
	r := "0"
	if !p.Open {
		r = "9" + p.EventID
	}
	e := map[string]any{"eventid": p.EventID, "objectid": p.TriggerID, "severity": strconv.Itoa(p.Severity), "clock": strconv.FormatInt(p.Clock, 10),
		"r_eventid": r, "hosts": []any{map[string]any{"host": w.Hosts[p.Host].Node, "hostid": zid(w, p.Host)}}}
	if p.Name != "" {
		e["name"] = p.Name
	}
	return e
}

// hourly averages a series per hour between from and till.
func hourly(s []Sample, from, till int64) []Sample {
	sums := map[int64][2]float64{}
	for _, x := range s {
		if x.Clock < from || x.Clock > till {
			continue
		}
		h := x.Clock - x.Clock%3600
		v := sums[h]
		sums[h] = [2]float64{v[0] + x.Value, v[1] + 1}
	}
	var out []Sample
	for _, h := range slices.Sorted(maps.Keys(sums)) {
		out = append(out, Sample{Clock: h, Value: sums[h][0] / sums[h][1]})
	}
	return out
}

// Notify sends the problem webhook of a change to the adapter of the machine's tenant, as the
// Zabbix media type does.
func (m *Monitor) Notify(ctx context.Context, w *World, c Change) error {
	h := w.Hosts[c.Problem.Host]
	if h == nil || m.Hooks[h.Tenant] == "" {
		return nil
	}
	hook := m.Hooks[h.Tenant]
	value := "0"
	if c.Opened {
		value = "1"
	}
	body, err := json.Marshal(map[string]any{"eventid": c.Problem.EventID, "triggerid": c.Problem.TriggerID, "host": w.Hosts[c.Problem.Host].Node, "value": value})
	if err != nil {
		return err
	}
	m.mu.Lock()
	twice := m.twice && c.Opened
	if twice {
		m.twice = false
	}
	m.mu.Unlock()
	if err := m.post(ctx, hook, body); err != nil || !twice {
		return err
	}
	if err := m.post(ctx, hook, body); err != nil {
		return err
	}
	m.mu.Lock()
	m.repeated++
	m.doubled = c.Problem.EventID
	m.mu.Unlock()
	return nil
}

// post sends one problem webhook.
func (m *Monitor) post(ctx context.Context, hook string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hook, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+m.HookToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("the webhook answered %d", resp.StatusCode)
	}
	return nil
}
