// SPDX-License-Identifier: AGPL-3.0-only

// Package zabbix is the E5 monitoring adapter for Zabbix 7.0 and later
// (spec/bindings/monitor/zabbix.md, the reference binding of spec/ports/e5-monitor.md): it
// writes alarm episodes and the signed audit head into trapper items of the farm host, ticks,
// passes problems of catalog hosts from a webhook to the core after a re-check through the API,
// sends the script lists of catalog hosts when they change, runs scripts of monitor_script
// deliveries once and adds code notes to problems. Script names, trigger names, tags and texts
// of Zabbix never go to the core.
package zabbix

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// Config is the adapter process of one tenant (TEN-4).
type Config struct {
	Core *p1client.Client
	// Tenant is the tenant of the worker token; Target the target name of the enroll record.
	Tenant, Target string
	// API is the Zabbix JSON-RPC address (.../api_jsonrpc.php), APIToken the token of the
	// adapter's own Zabbix user (MON-4).
	API, APIToken string
	// FarmHost is the farm host of the export items, botaru-core by default.
	FarmHost string
	// Hosts maps a Zabbix host name (monitor_host) to the catalog host id.
	Hosts map[string]string
	// WebhookToken is the token the Zabbix media type sends.
	WebhookToken string
	ScriptsEvery time.Duration
	TickEvery    time.Duration
	// MaintenanceEvery is maintenance_poll_s of the deployment binding: how often the maintenance
	// status of the catalog nodes is read, 60 seconds by default and at most 300 (MON-9).
	MaintenanceEvery time.Duration
	HTTP             *http.Client
	Logger           *slog.Logger
}

// Adapter runs one adapter process.
type Adapter struct {
	cfg Config
	log *slog.Logger
	mu  sync.Mutex
	// scripts is the latest script list by catalog host, and sent the hash the core has.
	scripts map[string][]script
	sent    map[string]string
	hostIDs map[string]string // Zabbix host name -> hostid
	reqID   int
	// inMaintenance is the window id of each node found in maintenance at the last poll.
	inMaintenance map[string]string
}

type script struct {
	ScriptID string   `json:"scriptid"`
	Input    string   `json:"input"`
	Choices  []string `json:"choices,omitempty"`
}

// New returns an adapter.
func New(cfg Config) *Adapter {
	if cfg.FarmHost == "" {
		cfg.FarmHost = "botaru-core"
	}
	if cfg.Target == "" {
		cfg.Target = "zabbix"
	}
	if cfg.ScriptsEvery == 0 {
		cfg.ScriptsEvery = 10 * time.Minute
	}
	if cfg.TickEvery == 0 {
		cfg.TickEvery = time.Minute
	}
	if cfg.MaintenanceEvery <= 0 {
		cfg.MaintenanceEvery = time.Minute
	}
	cfg.MaintenanceEvery = min(cfg.MaintenanceEvery, MaxMaintenanceEvery)
	if cfg.HTTP == nil {
		cfg.HTTP = &http.Client{Timeout: 90 * time.Second}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Adapter{cfg: cfg, log: cfg.Logger, scripts: map[string][]script{}, sent: map[string]string{}, hostIDs: map[string]string{},
		inMaintenance: map[string]string{}}
}

// Hello reports the adapter to the core.
func (a *Adapter) Hello(ctx context.Context) error {
	return a.cfg.Core.Call(ctx, "hello", "", map[string]any{
		"kind": "monitor", "implementation": "botaru-zabbix " + version.Version, "capacity": 1, "protocol_versions": []string{"v1"},
	}, nil)
}

// Run says hello, then serves deliveries, ticks and polls the script lists until ctx ends.
// The webhook handler is served by the caller (Webhook).
func (a *Adapter) Run(ctx context.Context) error {
	for {
		err := a.Hello(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if r := p1client.Reason(err); r == "blocked" || r == "unauthenticated" || r == "version_unsupported" {
			return err
		}
		a.log.Warn("hello failed", "err", err)
		sleep(ctx, 5*time.Second)
	}
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		a.every(ctx, a.cfg.MaintenanceEvery, a.PollMaintenance)
	}()
	go func() {
		defer wg.Done()
		a.every(ctx, a.cfg.TickEvery, a.Tick)
	}()
	go func() {
		defer wg.Done()
		a.every(ctx, a.cfg.ScriptsEvery, a.SyncScripts)
	}()
	for ctx.Err() == nil {
		if _, err := a.ServeOne(ctx, 30); err != nil && ctx.Err() == nil {
			a.log.Warn("serving work", "err", err)
			sleep(ctx, 3*time.Second)
		}
	}
	wg.Wait()
	return ctx.Err()
}

func (a *Adapter) every(ctx context.Context, d time.Duration, fn func(context.Context) error) {
	for ctx.Err() == nil {
		if err := fn(ctx); err != nil && ctx.Err() == nil {
			a.log.Warn("periodic step", "err", err)
		}
		sleep(ctx, d)
	}
}

func sleep(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}

// APIError is a refusal of the Zabbix API: the request reached Zabbix and was not carried out.
type APIError struct {
	Code    int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("zabbix api %d: %s", e.Code, e.Message) }

// call calls one Zabbix API method with the adapter's token.
func (a *Adapter) call(ctx context.Context, method string, params, out any) error {
	a.mu.Lock()
	a.reqID++
	id := a.reqID
	a.mu.Unlock()
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params, "id": id})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.API, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json-rpc")
	req.Header.Set("Authorization", "Bearer "+a.cfg.APIToken)
	resp, err := a.cfg.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("zabbix %s: request failed", method)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"error"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return fmt.Errorf("zabbix %s: unreadable answer (status %d)", method, resp.StatusCode)
	}
	if r.Error != nil {
		return &APIError{Code: r.Error.Code, Message: r.Error.Message}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(r.Result, out)
}

// push writes one value into a trapper item of the farm host (history.push).
func (a *Adapter) push(ctx context.Context, key, value string) error {
	var out struct {
		Response string `json:"response"`
		Data     []struct {
			Error string `json:"error"`
		} `json:"data"`
	}
	if err := a.call(ctx, "history.push", []any{map[string]any{"host": a.cfg.FarmHost, "key": key, "value": value}}, &out); err != nil {
		return err
	}
	for _, d := range out.Data {
		if d.Error != "" {
			return &APIError{Message: d.Error}
		}
	}
	return nil
}

// Tick writes the adapter liveness number (MON-2).
func (a *Adapter) Tick(ctx context.Context) error {
	return a.push(ctx, "botaru.adapter.alive["+a.cfg.Tenant+"]", "1")
}

// ServeOne claims one work item and serves it. It reports whether there was one.
func (a *Adapter) ServeOne(ctx context.Context, waitS int) (bool, error) {
	item, err := a.cfg.Core.Claim(ctx, []string{"delivery", "erasure"}, waitS)
	if err != nil || item == nil {
		return false, err
	}
	if item.Kind == "erasure" {
		return true, a.erase(ctx, item)
	}
	var p struct {
		Delivery struct {
			Kind string         `json:"kind"`
			Body map[string]any `json:"body"`
		} `json:"delivery"`
	}
	if err := json.Unmarshal(item.Payload, &p); err != nil {
		return true, a.fail(ctx, item, "input_schema_invalid", false)
	}
	body := p.Delivery.Body
	switch p.Delivery.Kind {
	case "monitor_alarm":
		sev, _ := body["severity"].(string)
		if err := a.push(ctx, "botaru.alarm["+sev+"]", alarmValue(body)); err != nil {
			return true, a.apiFail(ctx, item, err)
		}
		return true, a.complete(ctx, item, map[string]any{"external_id": "botaru.alarm"})
	case "monitor_audit_head":
		v := fmt.Sprintf("epoch=%v seq=%v hash=%v sig=%v key=%v", num(body["epoch"]), num(body["seq"]), body["hash"], body["sig"], body["key_id"])
		if err := a.push(ctx, "botaru.audit.head["+a.cfg.Tenant+"]", v); err != nil {
			return true, a.apiFail(ctx, item, err)
		}
		return true, a.complete(ctx, item, map[string]any{"external_id": "botaru.audit.head"})
	case "monitor_script":
		return true, a.runScript(ctx, item, body)
	case "monitor_note":
		return true, a.note(ctx, item, body)
	}
	return true, a.fail(ctx, item, "input_schema_invalid", false)
}

// alarmValue is the item value of an alarm episode: key=value pairs in a fixed order, the
// values after the tags (MON-1).
func alarmValue(body map[string]any) string {
	var parts []string
	for _, k := range []string{"seq", "state", "tenant", "alarm", "subject", "severity", "scope", "origin", "key"} {
		if v, ok := body[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, num(v)))
		}
	}
	if vals, ok := body["values"].(map[string]any); ok {
		keys := make([]string, 0, len(vals))
		for k := range vals {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			parts = append(parts, fmt.Sprintf("value_%s=%v", k, num(vals[k])))
		}
	}
	return strings.Join(parts, " ")
}

// num prints a JSON number without an exponent.
func num(v any) any {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return int64(f)
	}
	return v
}

func (a *Adapter) complete(ctx context.Context, item *p1client.WorkItem, receipt map[string]any) error {
	return a.cfg.Core.Call(ctx, "complete", item.Lease(), map[string]any{"receipt": receipt}, nil)
}

func (a *Adapter) fail(ctx context.Context, item *p1client.WorkItem, reason string, retryable bool) error {
	return a.cfg.Core.Call(ctx, "fail", item.Lease(), map[string]any{"reason": reason, "retryable": retryable}, nil)
}

// apiFail hands in a failure of the monitoring system: always retryable for an export, which
// never dies by attempts (MON-11).
func (a *Adapter) apiFail(ctx context.Context, item *p1client.WorkItem, err error) error {
	a.log.Warn("zabbix refused or did not answer", "err", err)
	return a.fail(ctx, item, "provider_unavailable", true)
}

// runScript runs a script of a monitor_script delivery once: the host must be a catalog host
// of this tenant and the scriptid in its latest list (MON-10).
func (a *Adapter) runScript(ctx context.Context, item *p1client.WorkItem, body map[string]any) error {
	resource, _ := body["resource"].(string)
	node, _ := body["monitor_host"].(string)
	scriptid, _ := body["scriptid"].(string)
	input, _ := body["input"].(string)
	if a.cfg.Hosts[node] != resource || resource == "" {
		return a.fail(ctx, item, "forbidden", false)
	}
	a.mu.Lock()
	list, known := a.scripts[resource]
	a.mu.Unlock()
	if !known {
		// After a restart the list is read before the first run, bounded well inside the lease
		// so the run's outcome is still handed in (MON-10); the periodic sync started by Run
		// usually fills it first.
		wctx, cancel := context.WithTimeout(ctx, scriptsWarmTimeout)
		err := a.SyncScripts(wctx)
		cancel()
		if err != nil {
			return a.fail(ctx, item, "provider_unavailable", false)
		}
		a.mu.Lock()
		list = a.scripts[resource]
		a.mu.Unlock()
	}
	var s *script
	for i := range list {
		if list[i].ScriptID == scriptid {
			s = &list[i]
		}
	}
	if s == nil || s.Input == "none" && input != "" || s.Input == "list" && !contains(s.Choices, input) {
		return a.fail(ctx, item, "script_unknown", false)
	}
	hostid, err := a.hostID(ctx, node)
	if err != nil {
		return a.fail(ctx, item, "provider_unavailable", false)
	}
	params := map[string]any{"scriptid": scriptid, "hostid": hostid}
	if input != "" {
		params["manualinput"] = input
	}
	var out struct {
		Response string `json:"response"`
		Value    string `json:"value"`
	}
	if err := a.call(ctx, "script.execute", params, &out); err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			// Zabbix refused the call: the script did not run.
			return a.fail(ctx, item, "provider_unavailable", false)
		}
		// Nobody knows whether it ran (APR-7).
		return a.fail(ctx, item, "exec_unknown", false)
	}
	// Zabbix gives no exit code; a failed run hands in 1 so the core reads it as failed.
	receipt := map[string]any{"external_id": "script:" + scriptid, "output": cut(out.Value, 4096)}
	if out.Response != "success" {
		receipt["exit_code"] = 1
	}
	return a.complete(ctx, item, receipt)
}

func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// note adds the code note service_control_done to a problem: event.acknowledge with action 4
// only, never another bit (MON-8, MON-4).
func (a *Adapter) note(ctx context.Context, item *p1client.WorkItem, body map[string]any) error {
	eventid, _ := body["eventid"].(string)
	bot, _ := body["bot_id"].(string)
	fields, _ := body["fields"].(map[string]any)
	var parts []string
	for _, k := range []string{"service", "script", "verb", "result", "grant_id", "approval_id"} {
		if v, ok := fields[k]; ok {
			parts = append(parts, fmt.Sprintf("%s=%v", k, v))
		}
	}
	msg := fmt.Sprintf("[botaru %s] service_control_done %s", bot, strings.Join(parts, " "))
	if err := a.call(ctx, "event.acknowledge", map[string]any{"eventids": []string{eventid}, "action": 4, "message": msg}, nil); err != nil {
		return a.apiFail(ctx, item, err)
	}
	return a.complete(ctx, item, map[string]any{"external_id": "ack:" + eventid})
}

// erase answers an erasure step: a note carries no data of people, so a subject scope is
// not_applicable; Zabbix 7.0 cannot delete a message, so a tenant or id scope fails and a
// Zabbix administrator removes the notes by hand (RET-7).
func (a *Adapter) erase(ctx context.Context, item *p1client.WorkItem) error {
	var p struct {
		Scope struct {
			Subject string   `json:"subject"`
			IDs     []string `json:"ids"`
		} `json:"scope"`
	}
	_ = json.Unmarshal(item.Payload, &p)
	outcome := map[string]any{"outcome": "failed"}
	if p.Scope.Subject != "" && len(p.Scope.IDs) == 0 {
		outcome = map[string]any{"outcome": "not_applicable", "basis": "kind_not_held"}
	}
	return a.cfg.Core.Call(ctx, "complete", item.Lease(), map[string]any{"erasure": outcome}, nil)
}

// hostID is the Zabbix hostid of a catalog host.
func (a *Adapter) hostID(ctx context.Context, node string) (string, error) {
	a.mu.Lock()
	id, ok := a.hostIDs[node]
	a.mu.Unlock()
	if ok {
		return id, nil
	}
	if err := a.loadHostIDs(ctx); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if id, ok := a.hostIDs[node]; ok {
		return id, nil
	}
	return "", fmt.Errorf("host %s is not visible to the adapter user", node)
}

func (a *Adapter) loadHostIDs(ctx context.Context) error {
	nodes := make([]string, 0, len(a.cfg.Hosts))
	for n := range a.cfg.Hosts {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	var hosts []struct {
		HostID string `json:"hostid"`
		Host   string `json:"host"`
	}
	if err := a.call(ctx, "host.get", map[string]any{"output": []string{"hostid", "host"}, "filter": map[string]any{"host": nodes}}, &hosts); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, h := range hosts {
		if _, ok := a.cfg.Hosts[h.Host]; ok {
			a.hostIDs[h.Host] = h.HostID
		}
	}
	return nil
}

// choiceForm is the form of a choice of a script input list in ext.monitor.scripts.v1.
var choiceForm = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)

// scriptsWarmTimeout bounds the script-list read a run makes on a cold cache: far below the
// delivery lease, so the outcome is handed in before the lease ends.
const scriptsWarmTimeout = 20 * time.Second

// SyncScripts reads the scripts Zabbix gives the adapter user on each catalog host and sends
// the list of a host to the core when its hash changed (MON-10). The script name is dropped.
func (a *Adapter) SyncScripts(ctx context.Context) error {
	if err := a.loadHostIDs(ctx); err != nil {
		return err
	}
	a.mu.Lock()
	byID := map[string]string{}
	var ids []string
	for node, id := range a.hostIDs {
		byID[id] = node
		ids = append(ids, id)
	}
	a.mu.Unlock()
	sort.Strings(ids)
	if len(ids) == 0 {
		return nil
	}
	var firstErr error
	var res map[string][]struct {
		ScriptID      string `json:"scriptid"`
		Scope         string `json:"scope"`
		ManualInput   string `json:"manualinput"`
		ValidatorType string `json:"manualinput_validator_type"`
		Validator     string `json:"manualinput_validator"`
	}
	// Zabbix 7.0 takes one host object per host, with an optional manualinput.
	hosts := make([]map[string]string, 0, len(ids))
	for _, id := range ids {
		hosts = append(hosts, map[string]string{"hostid": id})
	}
	if err := a.call(ctx, "script.getscriptsbyhosts", hosts, &res); err != nil {
		return err
	}
	for _, hostid := range ids {
		resource := a.cfg.Hosts[byID[hostid]]
		list := []script{}
		for _, s := range res[hostid] {
			if s.Scope != "" && s.Scope != "2" {
				continue
			}
			switch {
			case s.ManualInput == "" || s.ManualInput == "0":
				list = append(list, script{ScriptID: s.ScriptID, Input: "none"})
			case s.ValidatorType == "1":
				var choices []string
				ok := true
				for _, c := range strings.Split(s.Validator, ",") {
					if c = strings.TrimSpace(c); c != "" {
						choices = append(choices, c)
						ok = ok && len(c) <= 64 && choiceForm.MatchString(c)
					}
				}
				// A list the core would refuse leaves out this script only, not the host.
				if ok && len(choices) <= 32 {
					list = append(list, script{ScriptID: s.ScriptID, Input: "list", Choices: choices})
				}
			}
		}
		sort.Slice(list, func(i, j int) bool { return list[i].ScriptID < list[j].ScriptID })
		if len(list) > 64 {
			a.log.Warn("a host has more than 64 scripts for the adapter user; the list is cut by scriptid", "resource", resource, "scripts", len(list))
			list = list[:64]
		}
		raw, _ := json.Marshal(list)
		sum := sha256.Sum256(raw)
		hash := "sha256:" + hex.EncodeToString(sum[:16])
		a.mu.Lock()
		a.scripts[resource] = list
		same := a.sent[resource] == hash
		a.mu.Unlock()
		if same {
			continue
		}
		if _, err := a.signal(ctx, "ext.monitor.scripts.v1", "scripts:"+resource+":"+hash[7:23],
			map[string]any{"resource": resource, "hash": hash, "scripts": list}); err != nil {
			// One refused list does not stop the lists of the other hosts.
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		a.mu.Lock()
		a.sent[resource] = hash
		a.mu.Unlock()
	}
	return firstErr
}

// signal submits an external event to the core as an activity of kind signal.
func (a *Adapter) signal(ctx context.Context, typ, update string, data map[string]any) (map[string]any, error) {
	var out map[string]any
	err := a.cfg.Core.Call(ctx, "submit_activity", "", map[string]any{
		"kind": "signal", "channel": a.cfg.Target, "update_id": update,
		"sender": map[string]any{"identity": "hook:monitor", "verified": true},
		"chat":   map[string]any{"kind": "direct", "external_id": "monitor"},
		"event":  map[string]any{"type": typ, "data": data},
	}, &out)
	return out, err
}

// webhookBody is what the Zabbix media type sends: {EVENT.ID}, {TRIGGER.ID}, {HOST.HOST},
// {EVENT.NSEVERITY}, {EVENT.VALUE}. Nothing else is read.
type webhookBody struct {
	EventID   string `json:"eventid"`
	TriggerID string `json:"triggerid"`
	Host      string `json:"host"`
	Value     string `json:"value"`
}

// Webhook is the handler of the problem webhook (MON-5): the token, a catalog host, a re-check
// of the problem through the API, then only closed fields to the core.
func (a *Adapter) Webhook() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if a.cfg.WebhookToken == "" || subtle.ConstantTimeCompare([]byte(got), []byte(a.cfg.WebhookToken)) != 1 {
			http.Error(w, "unauthenticated", http.StatusUnauthorized)
			return
		}
		var b webhookBody
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&b); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		status, err := a.problem(r.Context(), b)
		if err != nil {
			a.log.Warn("problem webhook", "err", err)
		}
		w.WriteHeader(status)
	})
}

// problem re-checks a webhook problem and passes it on. It returns the HTTP status.
func (a *Adapter) problem(ctx context.Context, b webhookBody) (int, error) {
	resource, ok := a.cfg.Hosts[b.Host]
	if !ok || !digits(b.EventID) || !digits(b.TriggerID) {
		// A host outside the catalog does not enter the farm.
		return http.StatusAccepted, nil
	}
	state := "open"
	if b.Value == "0" {
		state = "resolved"
	}
	var events []struct {
		EventID  string `json:"eventid"`
		ObjectID string `json:"objectid"`
		Severity string `json:"severity"`
		REventID string `json:"r_eventid"`
		Hosts    []struct {
			Host string `json:"host"`
		} `json:"hosts"`
	}
	if err := a.call(ctx, "event.get", map[string]any{"eventids": []string{b.EventID}, "selectHosts": []string{"host"},
		"output": []string{"eventid", "objectid", "severity", "r_eventid"}}, &events); err != nil {
		return http.StatusBadGateway, err
	}
	if len(events) != 1 || events[0].ObjectID != b.TriggerID {
		return http.StatusAccepted, nil
	}
	e := events[0]
	onHost := false
	for _, h := range e.Hosts {
		onHost = onHost || h.Host == b.Host
	}
	open := e.REventID == "" || e.REventID == "0"
	if !onHost || open != (state == "open") {
		return http.StatusAccepted, nil
	}
	sev := 0
	fmt.Sscan(e.Severity, &sev)
	if _, err := a.signal(ctx, "ext.monitor.problem.v1", b.EventID+":"+state, map[string]any{
		"eventid": b.EventID, "triggerid": b.TriggerID, "resource": resource, "severity": sev, "state": state,
	}); err != nil {
		return http.StatusServiceUnavailable, err
	}
	return http.StatusAccepted, nil
}

func digits(s string) bool {
	if s == "" || len(s) > 20 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// MaxMaintenanceEvery is the longest maintenance poll interval (MON-9).
const MaxMaintenanceEvery = 300 * time.Second

// PollMaintenance reads the maintenance status of the catalog nodes and sends one
// ext.monitor.maintenance.v1 per node in maintenance with state active and until = the poll
// time plus two poll intervals, and one with state ended at the first poll that finds a node
// out of the maintenance it was in (MON-9). It does not read the time periods of a window:
// the end of a window is found by the polls, not computed (D-100).
func (a *Adapter) PollMaintenance(ctx context.Context) error {
	nodes := make([]string, 0, len(a.cfg.Hosts))
	for n := range a.cfg.Hosts {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)
	if len(nodes) == 0 {
		return nil
	}
	var hosts []struct {
		Host          string `json:"host"`
		Status        string `json:"maintenance_status"`
		MaintenanceID string `json:"maintenanceid"`
	}
	if err := a.call(ctx, "host.get", map[string]any{"output": []string{"hostid", "host", "maintenance_status", "maintenanceid"},
		"filter": map[string]any{"host": nodes}}, &hosts); err != nil {
		return err
	}
	now := time.Now().UTC()
	until := now.Add(2 * a.cfg.MaintenanceEvery).Format(time.RFC3339)
	found := map[string]string{}
	for _, h := range hosts {
		if _, ok := a.cfg.Hosts[h.Host]; ok && h.Status == "1" && digits(h.MaintenanceID) && len(h.MaintenanceID) <= 20 {
			found[h.Host] = h.MaintenanceID
		}
	}
	var firstErr error
	for _, node := range nodes {
		resource := a.cfg.Hosts[node]
		a.mu.Lock()
		prev := a.inMaintenance[node]
		a.mu.Unlock()
		mid, active := found[node]
		var data map[string]any
		switch {
		case active:
			data = map[string]any{"maintenanceid": mid, "resource": resource, "state": "active", "until": until}
		case prev != "":
			data = map[string]any{"maintenanceid": prev, "resource": resource, "state": "ended"}
		default:
			continue
		}
		// An ended window of another id comes before the active one of the new id.
		if active && prev != "" && prev != mid {
			if _, err := a.signal(ctx, "ext.monitor.maintenance.v1", "maint:"+resource+":"+prev+":ended:"+now.Format("20060102T150405"),
				map[string]any{"maintenanceid": prev, "resource": resource, "state": "ended"}); err != nil {
				firstErr = cmp.Or(firstErr, err)
				continue
			}
		}
		if _, err := a.signal(ctx, "ext.monitor.maintenance.v1", "maint:"+resource+":"+data["maintenanceid"].(string)+":"+data["state"].(string)+":"+now.Format("20060102T150405"), data); err != nil {
			firstErr = cmp.Or(firstErr, err)
			continue
		}
		a.mu.Lock()
		if active {
			a.inMaintenance[node] = mid
		} else {
			delete(a.inMaintenance, node)
		}
		a.mu.Unlock()
	}
	return firstErr
}
