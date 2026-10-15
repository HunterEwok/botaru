// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// Monitor is the read plugin of the monitoring system for Zabbix 7.0 and later (MON-6): only
// read methods, only the hosts that the tenant catalog bound with monitor_host, checked by this
// code against the catalog, not by the model. problems, maintenance and item_history give
// closed fields and numbers under the closed output schemas of the manifest (trusted output); item_text and problem_comments give text people
// and machines wrote (untrusted output, the core sets untrusted_seen). Trigger names, tags and
// problem texts are never returned (INJ-16).
type Monitor struct {
	API, Token string
	// Hosts maps a catalog host id to its Zabbix host name (hosts[].monitor_host).
	Hosts map[string]string
	// Triggers maps a catalog triggerid to its class and item (monitor_triggers).
	Triggers map[string]MonitorTrigger
	// Items maps the closed item fields of item_history to Zabbix item keys.
	Items map[string]string
	HTTP  *http.Client
	reqID int
}

// MonitorTrigger is a monitor_triggers row of the catalog.
type MonitorTrigger struct{ Class, Item string }

// DefaultMonitorItems are the Zabbix keys of the closed fields of item_history in the
// standard Linux template; -item flags override them per installation.
var DefaultMonitorItems = map[string]string{
	"cpu_load":  "system.cpu.load[all,avg1]",
	"mem_used":  "vm.memory.utilization",
	"disk_used": "vfs.fs.size[/,pused]",
	"net_in":    "net.if.in[eth0]",
	"net_out":   "net.if.out[eth0]",
}

const monitorMaxPoints = 500

// monitorHistoryLimit caps the raw values one history.get reads.
const monitorHistoryLimit = 10000

// monitorClasses are the classes of spec/model/monitor-map.yaml other than unknown.
var monitorClasses = map[string]bool{"disk_free": true, "disk_fill": true}

// Call runs one tool.
func (m *Monitor) Call(ctx context.Context, tool string, args map[string]any, _ *mcp.Meta) (*mcp.CallResult, error) {
	host, err := argString(args, "host")
	if err != nil {
		return nil, err
	}
	node, ok := m.Hosts[host]
	if !ok {
		// A host outside the tenant catalog, or one without a monitoring node (MON-6).
		return nil, errors.New("the host is not bound to the monitoring system")
	}
	hostid, err := m.hostID(ctx, node)
	if err != nil {
		return nil, err
	}
	switch tool {
	case "problems":
		return m.problems(ctx, host, hostid, args)
	case "maintenance":
		return m.maintenance(ctx, hostid)
	case "item_history":
		return m.itemHistory(ctx, hostid, args)
	case "item_text":
		return m.itemText(ctx, hostid, args)
	case "problem_comments":
		return m.comments(ctx, hostid)
	}
	return nil, errors.New("unknown tool")
}

func (m *Monitor) call(ctx context.Context, method string, params, out any) error {
	m.reqID++
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": m.reqID})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.API, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("Authorization", "Bearer "+m.Token)
	hc := m.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return errors.New("the monitoring system did not answer")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code int `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return errors.New("the monitoring system answer is not valid")
	}
	if r.Error != nil {
		// The error text of the API is not passed on.
		return fmt.Errorf("the monitoring system refused the request (%d)", r.Error.Code)
	}
	return json.Unmarshal(r.Result, out)
}

func (m *Monitor) hostID(ctx context.Context, node string) (string, error) {
	var hosts []struct {
		HostID string `json:"hostid"`
		Host   string `json:"host"`
	}
	if err := m.call(ctx, "host.get", map[string]any{"output": []string{"hostid", "host"}, "filter": map[string]any{"host": []string{node}}}, &hosts); err != nil {
		return "", err
	}
	for _, h := range hosts {
		if h.Host == node {
			return h.HostID, nil
		}
	}
	return "", errors.New("the host is not visible to the plugin")
}

func since(args map[string]any, name string, def time.Duration) (int64, error) {
	s, _ := args[name].(string)
	if s == "" {
		return time.Now().Add(-def).Unix(), nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return 0, errors.New("bad " + name)
	}
	return t.Unix(), nil
}

func structured(out map[string]any) *mcp.CallResult {
	text, _ := json.Marshal(out)
	return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: string(text)}}, StructuredContent: out}
}

func unixTime(s string) string {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 {
		return ""
	}
	return time.Unix(n, 0).UTC().Format(time.RFC3339)
}

// problems lists the problems of the host: ids, the class from the catalog, the severity, the
// state and the time; no name, tag or text (MON-6, INJ-16).
func (m *Monitor) problems(ctx context.Context, host, hostid string, args map[string]any) (*mcp.CallResult, error) {
	from, err := since(args, "since", 24*time.Hour)
	if err != nil {
		return nil, err
	}
	var events []struct {
		EventID  string `json:"eventid"`
		ObjectID string `json:"objectid"`
		Severity string `json:"severity"`
		Clock    string `json:"clock"`
		REventID string `json:"r_eventid"`
	}
	if err := m.call(ctx, "problem.get", map[string]any{"hostids": []string{hostid}, "time_from": from, "recent": true,
		"output": []string{"eventid", "objectid", "severity", "clock", "r_eventid"}, "sortfield": []string{"eventid"}, "sortorder": "DESC", "limit": 100}, &events); err != nil {
		return nil, err
	}
	list := []any{}
	for _, e := range events {
		eid, err1 := strconv.ParseInt(e.EventID, 10, 64)
		tid, err2 := strconv.ParseInt(e.ObjectID, 10, 64)
		sev, err3 := strconv.Atoi(e.Severity)
		if err1 != nil || err2 != nil || err3 != nil || sev < 0 || sev > 5 || unixTime(e.Clock) == "" {
			return nil, errors.New("the monitoring system answer is not valid")
		}
		// The class comes from the catalog and only from the closed table of classes; the
		// item stays in the catalog, where the trigger id finds it.
		class := "unknown"
		if t, ok := m.Triggers[e.ObjectID]; ok && monitorClasses[t.Class] {
			class = t.Class
		}
		state := "open"
		if e.REventID != "" && e.REventID != "0" {
			state = "resolved"
		}
		list = append(list, map[string]any{"eventid": eid, "triggerid": tid, "class": class, "resource": host, "severity": sev, "state": state, "time": unixTime(e.Clock)})
	}
	return structured(map[string]any{"host": host, "problems": list}), nil
}

// maintenance gives the maintenance windows of the host: start and end only.
func (m *Monitor) maintenance(ctx context.Context, hostid string) (*mcp.CallResult, error) {
	var ms []struct {
		ActiveSince string `json:"active_since"`
		ActiveTill  string `json:"active_till"`
	}
	if err := m.call(ctx, "maintenance.get", map[string]any{"hostids": []string{hostid}, "output": []string{"active_since", "active_till"}}, &ms); err != nil {
		return nil, err
	}
	// Ended windows and windows without valid times are left out; the list stays within its
	// schema bound.
	now := time.Now().Unix()
	list := []any{}
	for _, w := range ms {
		till, _ := strconv.ParseInt(w.ActiveTill, 10, 64)
		start, end := unixTime(w.ActiveSince), unixTime(w.ActiveTill)
		if start == "" || end == "" || till < now || len(list) == 100 {
			continue
		}
		list = append(list, map[string]any{"start": start, "end": end})
	}
	return structured(map[string]any{"windows": list}), nil
}

type zitem struct {
	ItemID    string `json:"itemid"`
	ValueType string `json:"value_type"`
}

func (m *Monitor) item(ctx context.Context, hostid, key string) (*zitem, error) {
	var items []zitem
	if err := m.call(ctx, "item.get", map[string]any{"hostids": []string{hostid}, "filter": map[string]any{"key_": key}, "output": []string{"itemid", "value_type"}}, &items); err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, errors.New("item_unknown")
	}
	return &items[0], nil
}

// itemHistory gives the numbers of a closed item field: averages per step from the history,
// or from the trends for a step of an hour. A text or log item is item_not_numeric (MON-6).
func (m *Monitor) itemHistory(ctx context.Context, hostid string, args map[string]any) (*mcp.CallResult, error) {
	field, err := argString(args, "item")
	if err != nil {
		return nil, err
	}
	key, ok := m.Items[field]
	if !ok {
		return nil, errors.New("item_unknown")
	}
	start, err := since(args, "start", time.Hour)
	if err != nil {
		return nil, err
	}
	end := time.Now().Unix()
	if s, _ := args["end"].(string); s != "" {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return nil, errors.New("bad end")
		}
		end = t.Unix()
	}
	step, stepName := int64(300), "5m"
	switch args["step"] {
	case "1m":
		step, stepName = 60, "1m"
	case "1h":
		step, stepName = 3600, "1h"
	}
	it, err := m.item(ctx, hostid, key)
	if err != nil {
		return nil, err
	}
	if it.ValueType != "0" && it.ValueType != "3" {
		return nil, errors.New("item_not_numeric")
	}
	type point struct {
		Clock string `json:"clock"`
		Value string `json:"value"`
		Avg   string `json:"value_avg"`
	}
	// At most the last monitorMaxPoints steps before end are read, newest first, so a cut list
	// still ends at end; a step whose oldest values the limit cut off is dropped below.
	if first := end - end%step - (monitorMaxPoints-1)*step; start < first {
		start = first
	}
	var pts []point
	if step == 3600 {
		err = m.call(ctx, "trend.get", map[string]any{"itemids": []string{it.ItemID}, "time_from": start, "time_till": end, "output": []string{"clock", "value_avg"},
			"sortfield": "clock", "sortorder": "DESC", "limit": monitorMaxPoints}, &pts)
	} else {
		err = m.call(ctx, "history.get", map[string]any{"itemids": []string{it.ItemID}, "history": it.ValueType, "time_from": start, "time_till": end,
			"output": []string{"clock", "value"}, "sortfield": "clock", "sortorder": "DESC", "limit": monitorHistoryLimit}, &pts)
	}
	if err != nil {
		return nil, err
	}
	sums := map[int64][2]float64{}
	for _, p := range pts {
		c, err1 := strconv.ParseInt(p.Clock, 10, 64)
		raw := p.Value
		if raw == "" {
			raw = p.Avg
		}
		v, err2 := strconv.ParseFloat(raw, 64)
		if err1 != nil || err2 != nil || math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, errors.New("item_not_numeric")
		}
		b := c - c%step
		s := sums[b]
		sums[b] = [2]float64{s[0] + v, s[1] + 1}
	}
	buckets := make([]int64, 0, len(sums))
	for b := range sums {
		buckets = append(buckets, b)
	}
	sort.Slice(buckets, func(i, j int) bool { return buckets[i] < buckets[j] })
	if len(pts) >= monitorHistoryLimit && step != 3600 && len(buckets) > 1 {
		buckets = buckets[1:]
	}
	if len(buckets) > monitorMaxPoints {
		buckets = buckets[len(buckets)-monitorMaxPoints:]
	}
	list := []any{}
	for _, b := range buckets {
		s := sums[b]
		list = append(list, map[string]any{"t": time.Unix(b, 0).UTC().Format(time.RFC3339), "v": s[0] / s[1]})
	}
	return structured(map[string]any{"item": field, "step": stepName, "points": list}), nil
}

// itemText gives the latest values of the text or log item of a catalog trigger: text others
// wrote, so the tool's output is untrusted (INJ-13).
func (m *Monitor) itemText(ctx context.Context, hostid string, args map[string]any) (*mcp.CallResult, error) {
	trig, err := argString(args, "trigger")
	if err != nil {
		return nil, err
	}
	t, ok := m.Triggers[trig]
	if !ok || t.Item == "" {
		return nil, errors.New("the trigger is not in the catalog")
	}
	from, err := since(args, "since", time.Hour)
	if err != nil {
		return nil, err
	}
	it, err := m.item(ctx, hostid, t.Item)
	if err != nil {
		return nil, err
	}
	if it.ValueType != "1" && it.ValueType != "2" && it.ValueType != "4" {
		return nil, errors.New("the item is not text")
	}
	var pts []struct {
		Value string `json:"value"`
	}
	if err := m.call(ctx, "history.get", map[string]any{"itemids": []string{it.ItemID}, "history": it.ValueType, "time_from": from,
		"output": []string{"clock", "value"}, "sortfield": "clock", "sortorder": "DESC", "limit": 200}, &pts); err != nil {
		return nil, err
	}
	var b strings.Builder
	for i := len(pts) - 1; i >= 0; i-- {
		b.WriteString(pts[i].Value)
		b.WriteByte('\n')
	}
	return Text(b.String()), nil
}

// comments gives the messages people and bots added to the problems of the host: untrusted
// text (INJ-13).
func (m *Monitor) comments(ctx context.Context, hostid string) (*mcp.CallResult, error) {
	var events []struct {
		EventID      string `json:"eventid"`
		Acknowledges []struct {
			Clock   string `json:"clock"`
			Message string `json:"message"`
		} `json:"acknowledges"`
	}
	if err := m.call(ctx, "problem.get", map[string]any{"hostids": []string{hostid}, "recent": true, "selectAcknowledges": []string{"clock", "message"},
		"output": []string{"eventid"}, "sortfield": []string{"eventid"}, "sortorder": "DESC", "limit": 50}, &events); err != nil {
		return nil, err
	}
	var b strings.Builder
	for _, e := range events {
		for _, a := range e.Acknowledges {
			if a.Message == "" {
				continue
			}
			fmt.Fprintf(&b, "%s %s: %s\n", e.EventID, unixTime(a.Clock), a.Message)
		}
	}
	return Text(b.String()), nil
}
