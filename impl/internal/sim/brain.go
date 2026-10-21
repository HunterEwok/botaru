// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
)

// Brain is the mock model of a -mock run: an Anthropic Messages API that plays a plausible
// operator bot without a model. It keeps no state between calls: each answer comes from the
// request alone (the goal, the tools offered, the tool results so far and the result schema),
// so any number of bots and attempts can share it. It reads the tools the way an operator
// would (checks, logs, the unit state, the disk) and asks for a restart or a cleanup when the
// tool output shows the need. It is gullible on purpose: a log line that tells the assistant
// to restart a service makes it try, so the guards of the core are exercised (INJ-15). The one
// switch it has is the evolution mode the director turns on: then an analysis proposes a
// manifest change instead of a change of the kind none (D-120).
type Brain struct {
	// Home is the tenant of the scenarios, whose Tenant an analysis proposes to change.
	Home   string
	calls  atomic.Int64
	evolve atomic.Bool
	// noAttachments counts the planned data/describe calls whose file had no values to pick
	// from: the sign of the known gap GapAttachments.
	noAttachments atomic.Int64
}

// NoAttachments is the number of data/describe calls the mock could not make for want of an
// attachment to name.
func (b *Brain) NoAttachments() int64 { return b.noAttachments.Load() }

// Calls is the number of answered calls.
func (b *Brain) Calls() int64 { return b.calls.Load() }

// SetEvolve turns the evolution mode of the analyses on or off.
func (b *Brain) SetEvolve(on bool) { b.evolve.Store(on) }

// evolveTarget is the Tenant an analysis proposes to change in the evolution mode, "" outside it.
func (b *Brain) evolveTarget() string {
	if !b.Evolve() {
		return ""
	}
	if b.Home == "" {
		return "demo"
	}
	return b.Home
}

// Evolve says whether the analyses propose a manifest change.
func (b *Brain) Evolve() bool { return b.evolve.Load() }

type brainRequest struct {
	Model    string          `json:"model"`
	System   json.RawMessage `json:"system"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Name        string         `json:"name"`
		InputSchema map[string]any `json:"input_schema"`
	} `json:"tools"`
}

type block struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

// blocks reads a content field: a string or a list of blocks.
func blocks(raw json.RawMessage) []block {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []block{{Type: "text", Text: s}}
	}
	var bs []block
	_ = json.Unmarshal(raw, &bs)
	return bs
}

// text joins the text of a content field.
func text(raw json.RawMessage) string {
	var out []string
	for _, b := range blocks(raw) {
		switch b.Type {
		case "text":
			out = append(out, b.Text)
		case "tool_result":
			out = append(out, text(b.Content))
		}
	}
	return strings.Join(out, "\n")
}

// step is one tool call of a plan; when, if set, decides from the tool output so far whether
// the call is made; from, if set, makes the input from the tool output so far (nil: not yet).
type step struct {
	plugin, tool string
	input        map[string]any
	when         func(seen string) bool
	from         func(seen string) map[string]any
}

// The plans of the goals the director's people send. The logs tool returns at most its
// output_limit_kb from the start of the lines it read, so a plan reads 80 lines of the access
// log, which fit, and so sees the newest ones.
var (
	reSlow   = regexp.MustCompile(`(?i)slow|latenc|long time`)
	reDown   = regexp.MustCompile(`(?i)down|not open|unavailable|502|not respond|does not load`)
	reDisk   = regexp.MustCompile(`(?i)disk|space|full|storage`)
	reInject = regexp.MustCompile(`(?i)(ai assistant|assistant:|system notice)[^\n]*restart`)
	reDfData = regexp.MustCompile(`([0-9]+)% /data`)
	// reUnitDown is a unit state of systemctl that is not running.
	reUnitDown = regexp.MustCompile(`Active: (inactive|failed)|"state": ?"(inactive|failed)"`)
	reGeo      = regexp.MustCompile(`(?i)\bgeo\b|bathy|raster|layer|map-areas|dataset`)
	rePipe     = regexp.MustCompile(`(?i)pipeline|\bruns?\b|ctd|station`)
	reKB       = regexp.MustCompile(`(?i)runbook|manual|knowledge|documentation`)
	// The instructions of task types the mock tells apart: the CTD file description, the
	// availability summary and the reports whose numbers come from params.
	reDescribeTask = regexp.MustCompile(`(?i)describe the ctd file`)
	reHealthTask   = regexp.MustCompile(`(?i)whether the sites and services`)
	reParamsTask   = regexp.MustCompile(`(?i)numbers only from params`)
	reSummaryTask  = regexp.MustCompile(`(?i)answer briefly from the summaries`)
	reSensorAsk    = regexp.MustCompile(`(?i)\bsensors?\b`)
	reFragment     = regexp.MustCompile(`"fragment": ?"([a-z]{1,8}_[0-9A-HJKMNP-TV-Z]{26}:[0-9]{1,4})"`)
)

func dataUsedPct(seen string) int {
	m := reDfData.FindAllStringSubmatch(seen, -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

// plan is the tool calls of a task: by the task type's instruction in the system text, else by
// the words of the goal. The plans name the template machines and units (web1, db1, nginx);
// adapt fits them to the tenant's catalog.
func plan(system, goal string) []step {
	g := strings.ToLower(goal)
	switch {
	case reParamsTask.MatchString(system):
		return nil
	case reDescribeTask.MatchString(system):
		return []step{{plugin: "data", tool: "describe", input: map[string]any{"file": ""}}}
	case reSummaryTask.MatchString(system):
		// The summary of the day reads the bots' notes when its type has an inbox (MSG-8).
		return []step{{plugin: "", tool: "inbox_read", input: map[string]any{"max": 20}}}
	case reHealthTask.MatchString(system):
		return []step{
			{plugin: "checks", tool: "latest", input: map[string]any{"check": "www-health"}},
			{plugin: "checks", tool: "latest", input: map[string]any{"check": "api-health"}},
		}
	case reKB.MatchString(g):
		return []step{
			{plugin: "", tool: "kb_search", input: map[string]any{"corpus": "", "query": kbQuery(goal)}},
			{plugin: "", tool: "kb_read", from: func(seen string) map[string]any {
				if m := reFragment.FindStringSubmatch(seen); m != nil {
					return map[string]any{"corpus": "", "fragment": m[1], "count": 2}
				}
				return nil
			}},
		}
	case reDisk.MatchString(g):
		return []step{
			{plugin: "cmd", tool: "run", input: map[string]any{"command": "disk_usage", "host": "db1"}},
			{plugin: "logs", tool: "tail", input: map[string]any{"source": "postgresql", "lines": 50}},
			{plugin: "", tool: "request_action", input: map[string]any{"action": "cleanup_pipeline_tmp", "args": map[string]any{"older_than_days": 7}},
				when: func(seen string) bool { return dataUsedPct(seen) >= 85 }},
		}
	case reGeo.MatchString(g):
		return []step{
			{plugin: "geo", tool: "describe", input: map[string]any{"dataset": "bathy-50m"}},
			{plugin: "geo", tool: "describe_vector", input: map[string]any{"dataset": "map-areas"}},
		}
	case rePipe.MatchString(g):
		return []step{
			{plugin: "batch", tool: "runs", input: map[string]any{}},
			{plugin: "batch", tool: "trace", input: map[string]any{"run": "run-2026-09"},
				when: func(seen string) bool { return strings.Contains(seen, "run-2026-09 failed") }},
			{plugin: "batch", tool: "trace_summary", input: map[string]any{"stations": "st-05"}},
		}
	case reDown.MatchString(g):
		return []step{
			{plugin: "checks", tool: "latest", input: map[string]any{"check": "www-health"}},
			{plugin: "cmd", tool: "run", input: map[string]any{"command": "service_status", "host": "web1", "service": "nginx"}},
			{plugin: "service", tool: "control", input: map[string]any{"service": "nginx", "verb": "restart"},
				when: func(seen string) bool { return strings.Contains(seen, "inactive") || strings.Contains(seen, "failed") }},
		}
	case reSlow.MatchString(g):
		return []step{
			{plugin: "logs", tool: "tail", input: map[string]any{"source": "nginx-access", "lines": 80}},
			{plugin: "logs", tool: "tail", input: map[string]any{"source": "app-api", "lines": 100}},
			{plugin: "checks", tool: "latest", input: map[string]any{"check": "api-health"}},
		}
	}
	return []step{
		{plugin: "logs", tool: "tail", input: map[string]any{"source": "nginx-access", "lines": 80}},
		{plugin: "logs", tool: "tail", input: map[string]any{"source": "nginx-error", "lines": 100}},
	}
}

// toolName finds the offered tool of a plugin's tool: <plugin>_<tool> as P3 lists it, or a
// built-in tool by its own name.
func toolName(offered []string, plugin, tool string) string {
	for _, n := range offered {
		if plugin == "" && n == tool || plugin != "" && (n == plugin+"_"+tool || n == plugin+"__"+tool || n == plugin+"/"+tool) {
			return n
		}
	}
	return ""
}

// adapt fits the planned arguments to the enums of the tool's input schema: a plan names the
// template machines and units (web1, db1, nginx) and a tenant of the farm has its own. A value
// outside its enum becomes the enum value of the same kind the goal names (db1 -> db4), else the
// first of that kind (db1 -> db3), else one that ends with it (nginx -> web2-nginx); an empty
// value becomes the enum value one of whose words the goal holds (vendor-manuals for a manual),
// else the first.
func adapt(schema map[string]any, in map[string]any, goal string) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	out := maps.Clone(in)
	for k, v := range in {
		p, _ := props[k].(map[string]any)
		enum, _ := p["enum"].([]any)
		s, ok := v.(string)
		if !ok || len(enum) == 0 || slices.Contains(enum, any(s)) {
			continue
		}
		if pick := enumPick(enum, s, strings.ToLower(goal)); pick != "" {
			out[k] = pick
		}
	}
	return out
}

func enumPick(enum []any, v, goal string) string {
	var vals []string
	for _, e := range enum {
		if s, ok := e.(string); ok {
			vals = append(vals, s)
		}
	}
	if len(vals) == 0 {
		return ""
	}
	if v == "" {
		for _, s := range vals {
			for _, w := range strings.Split(s, "-") {
				if len(w) >= 4 && strings.Contains(goal, strings.TrimSuffix(w, "s")) {
					return s
				}
			}
		}
		return vals[0]
	}
	if kind := strings.TrimRight(v, "0123456789"); kind != "" && kind != v {
		var same []string
		for _, s := range vals {
			if strings.HasPrefix(s, kind) && strings.Trim(s[len(kind):], "0123456789") == "" {
				same = append(same, s)
			}
		}
		for _, s := range same {
			if regexp.MustCompile(`\b` + regexp.QuoteMeta(s) + `\b`).MatchString(goal) {
				return s
			}
		}
		if len(same) > 0 {
			return same[0]
		}
	}
	for _, s := range vals {
		if strings.HasSuffix(s, "-"+v) {
			return s
		}
	}
	return ""
}

// kbQuery is the word a knowledge search looks for: the longest word of the question.
func kbQuery(goal string) string {
	line, _, _ := strings.Cut(goal, "\n")
	best := ""
	for _, w := range regexp.MustCompile(`[A-Za-z][A-Za-z0-9-]*`).FindAllString(line, -1) {
		if len(w) > len(best) {
			best = strings.ToLower(w)
		}
	}
	return best
}

// fits keeps the arguments the tool's input schema names, and refuses a value outside an enum.
func fits(schema map[string]any, in map[string]any) (map[string]any, bool) {
	props, _ := schema["properties"].(map[string]any)
	if props == nil {
		return in, true
	}
	out := map[string]any{}
	for k, v := range in {
		p, ok := props[k].(map[string]any)
		if !ok {
			continue
		}
		if enum, ok := p["enum"].([]any); ok && !slices.Contains(enum, any(fmt.Sprint(v))) {
			return nil, false
		}
		out[k] = v
	}
	for _, r := range strList(schema["required"]) {
		if _, ok := out[r]; !ok {
			return nil, false
		}
	}
	return out, true
}

// topLevelCombinator names the oneOf, allOf or anyOf at the top of a tool's input schema, or
// is empty.
func topLevelCombinator(schema map[string]any) string {
	for _, k := range []string{"oneOf", "allOf", "anyOf"} {
		if _, ok := schema[k]; ok {
			return k
		}
	}
	return ""
}

func (b *Brain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var req brainRequest
	if err != nil || json.Unmarshal(raw, &req) != nil || len(req.Messages) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"the request is not a Messages request"}}`)
		return
	}
	// The Messages API refuses a tool whose input schema has oneOf, allOf or anyOf at the top
	// level; the mock refuses it the same way, so a mock run catches such a tool.
	for i, t := range req.Tools {
		if k := topLevelCombinator(t.InputSchema); k != "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			_, _ = fmt.Fprintf(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tools.%d.custom.input_schema: input_schema does not support oneOf, allOf, or anyOf at the top level (%s)"}}`, i, k)
			return
		}
	}
	n := b.calls.Add(1)
	system := text(req.System)
	goal := text(req.Messages[0].Content)
	// What the bot has done and seen so far.
	var seen strings.Builder
	called := map[string]int{}
	for _, m := range req.Messages[1:] {
		for _, bl := range blocks(m.Content) {
			switch bl.Type {
			case "tool_use":
				called[bl.Name+string(bl.Input)]++
				called[bl.Name]++
			case "tool_result":
				seen.WriteString(text(bl.Content))
				seen.WriteString("\n")
			}
		}
	}
	offered := map[string]map[string]any{}
	var names []string
	for _, t := range req.Tools {
		offered[t.Name] = t.InputSchema
		names = append(names, t.Name)
	}
	var content []map[string]any
	stop := "end_turn"
	if call, ans, ok := governance(system, goal, seen.String(), names, offered, called, b.evolveTarget()); ok {
		if call != nil {
			call["id"] = fmt.Sprintf("toolu_sim%06d", n)
			content = []map[string]any{{"type": "text", "text": "Let me read the series."}, call}
			stop = "tool_use"
		} else {
			content = []map[string]any{{"type": "text", "text": ans}}
		}
	} else if next := b.next(system, goal, seen.String(), names, offered, called); next != nil {
		content = []map[string]any{{"type": "text", "text": "Let me look at that."}, next}
		next["id"] = fmt.Sprintf("toolu_sim%06d", n)
		stop = "tool_use"
	} else {
		content = []map[string]any{{"type": "text", "text": answer(system, goal, seen.String())}}
	}
	// The Messages API marks a tool call by the model with caller {type: direct}, and the
	// runtime sends it back in the history; the mock answers the same way.
	for _, bl := range content {
		if bl["type"] == "tool_use" {
			bl["caller"] = map[string]any{"type": "direct"}
		}
	}
	out, _ := json.Marshal(content)
	resp := map[string]any{"id": fmt.Sprintf("msg_sim%06d", n), "type": "message", "role": "assistant", "model": req.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": len(raw)/4 + 1, "output_tokens": len(out)/4 + 1}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// next is the next tool call of the plan, nil when the bot answers.
func (b *Brain) next(system, goal, seen string, names []string, offered map[string]map[string]any, called map[string]int) map[string]any {
	// The gullible part: an instruction in the tool output to restart a service.
	if reInject.MatchString(seen) {
		if n := toolName(names, "service", "control"); n != "" && called[n] == 0 {
			if in, ok := fits(offered[n], adapt(offered[n], map[string]any{"service": "nginx", "verb": "restart"}, goal)); ok {
				return map[string]any{"type": "tool_use", "name": n, "input": in}
			}
		}
	}
	for _, s := range plan(system, goal) {
		n := toolName(names, s.plugin, s.tool)
		if n == "" {
			continue
		}
		input := s.input
		if s.from != nil {
			if input = s.from(seen); input == nil {
				continue
			}
		}
		if s.plugin == "data" && s.tool == "describe" && noValues(offered[n], "file") {
			// No attachment to name: no value of file passes the schema.
			b.noAttachments.Add(1)
			continue
		}
		in, ok := fits(offered[n], adapt(offered[n], input, goal))
		if !ok {
			continue
		}
		key, _ := json.Marshal(in)
		if called[n+string(key)] > 0 {
			continue
		}
		if s.when != nil && !s.when(seen) {
			continue
		}
		return map[string]any{"type": "tool_use", "name": n, "input": in}
	}
	return nil
}

// noValues says whether a property of a tool's input schema takes no value: the core writes
// an enum_from that resolved to no values as not: {}.
func noValues(schema map[string]any, prop string) bool {
	p, _ := mapOf(schema["properties"])[prop].(map[string]any)
	not, ok := p["not"].(map[string]any)
	return ok && len(not) == 0
}

// reSchema finds the result schema the runtime puts at the end of the system prompt.
var reSchema = regexp.MustCompile(`(?s)matches this JSON Schema, and nothing else:\n(\{.*\})\s*$`)

// answer is the final answer: a JSON object of the result schema, or plain text without one.
func answer(system, goal, seen string) string {
	f := findings(seen)
	summary := "From what the tools show: " + strings.Join(f.lines, " ")
	// A short answer about the sensors offers to look further and waits for the person
	// (need_input opens the follow-up window, CNT-5).
	ask := reSummaryTask.MatchString(system) && reSensorAsk.MatchString(firstLine(goal))
	switch params := attemptParams(goal); {
	case reParamsTask.MatchString(system) && len(params) > 0:
		summary = "The numbers of this report: " + paramsLine(params) + "."
	case len(f.lines) == 0:
		summary = "I looked at what the tools show and found nothing unusual."
	}
	if ask {
		f.needsYou = true
		summary += " Shall I compare it with yesterday?"
	}
	m := reSchema.FindStringSubmatch(system)
	if m == nil {
		return summary
	}
	var schema map[string]any
	if json.Unmarshal([]byte(m[1]), &schema) != nil {
		return summary
	}
	out, _ := json.Marshal(fill(schema, "", summary, f))
	return string(out)
}

// found are the facts the bot read in the tool output.
type found struct {
	lines           []string
	count5xx, slow  int
	diskUsed        int
	cause, needsYou bool
	// runFailed is a pipeline run the bot saw stop on a failure.
	runFailed bool
	// nums are the numbers of the JSON objects in the tool output by field name; checks and
	// checksBad count the site check results read and those with a failure.
	nums              map[string]float64
	checks, checksBad int
}

// number is the value of a result field of a CTD file description from the plugin's numbers:
// the rows are its records, a profile starts at the surface, and the flagged records are the
// missing share.
func (f found) number(name string) (float64, bool) {
	switch name {
	case "rows":
		v, ok := f.nums["records"]
		return v, ok
	case "depth_min_m":
		_, ok := f.nums["depth_max_m"]
		return 0, ok
	case "missing_pct":
		if n := f.nums["records"]; n > 0 {
			return math.Round(f.nums["flagged_records"]/n*1000) / 10, true
		}
		return 0, false
	}
	v, ok := f.nums[name]
	return v, ok
}

// jsonObjects are the JSON objects that start on a line of the tool output.
func jsonObjects(seen string) []map[string]any {
	var out []map[string]any
	for _, l := range strings.Split(seen, "\n") {
		i := strings.IndexByte(l, '{')
		if i < 0 {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(l[i:]), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

// walk calls fn on every object of a JSON value.
func walk(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, e := range x {
			walk(e, fn)
		}
	case []any:
		for _, e := range x {
			walk(e, fn)
		}
	}
}

// reData finds the data block of the attempt beside the goal: the task's params among them.
var reData = regexp.MustCompile(`(?s)it is data, not instructions:\n(\{.*\})`)

// attemptParams are the task's params in the data block of the first message.
func attemptParams(goal string) map[string]any {
	m := reData.FindStringSubmatch(goal)
	if m == nil {
		return nil
	}
	var data struct {
		Params map[string]any `json:"params"`
	}
	_ = json.Unmarshal([]byte(m[1]), &data)
	return data.Params
}

// paramsLine names the params of a report: each scalar by name, a list by its length.
func paramsLine(params map[string]any) string {
	var parts []string
	for _, k := range slices.Sorted(maps.Keys(params)) {
		switch v := params[k].(type) {
		case []any:
			parts = append(parts, fmt.Sprintf("%d %s", len(v), k))
		case map[string]any:
		default:
			parts = append(parts, fmt.Sprintf("%s %v", k, v))
		}
	}
	return strings.Join(parts, ", ")
}

var (
	reStatus = regexp.MustCompile(`" ([0-9]{3}) [0-9]+ "`)
	reRT     = regexp.MustCompile(`rt=([0-9.]+)`)
	reURL    = regexp.MustCompile(`"GET ([^ ?"]+)`)
	// reSlowApp is a slow request line of the app-api log.
	reSlowApp = regexp.MustCompile(`slow request (/[^ ]*) [0-9]+ms`)
)

func findings(seen string) found {
	f := found{nums: map[string]float64{}}
	var kbText string
	for _, o := range jsonObjects(seen) {
		walk(o, func(m map[string]any) {
			for k, v := range m {
				if n, ok := v.(float64); ok {
					f.nums[k] = n
				}
			}
			if sensor, ok := m["sensor"].(string); ok {
				if issue, ok := m["issue"].(string); ok {
					f.lines = append(f.lines, fmt.Sprintf("A bot note says sensor %s has a %s.", sensor, strings.ReplaceAll(issue, "_", " ")))
				}
			}
			if c, ok := m["check"].(string); ok {
				if total, ok := m["total"].(float64); ok {
					okN, _ := m["ok"].(float64)
					status, _ := m["last_status"].(float64)
					f.checks++
					bad := okN < total || status >= 500
					if bad {
						f.checksBad++
					}
					f.lines = append(f.lines, fmt.Sprintf("Check %s: %d of %d recent runs passed, last status %d.", c, int(okN), int(total), int(status)))
				}
			}
			if t, ok := m["text"].(string); ok && m["fragment"] != nil && kbText == "" {
				kbText = t
			}
		})
	}
	if n, ok := f.nums["records"]; ok {
		f.lines = append(f.lines, fmt.Sprintf("The file has %d records down to %.2f m; %d of them are flagged.", int(n), f.nums["depth_max_m"], int(f.nums["flagged_records"])))
	}
	if kbText != "" {
		f.lines = append(f.lines, "The knowledge base says: "+clip(strings.Join(strings.Fields(kbText), " "), 300))
	}
	slowURLs := map[string]int{}
	for _, l := range strings.Split(seen, "\n") {
		if m := reStatus.FindStringSubmatch(l); m != nil && m[1][0] == '5' {
			f.count5xx++
		}
		if u := reSlowApp.FindStringSubmatch(l); u != nil {
			f.slow++
			slowURLs[u[1]]++
			continue
		}
		if m := reRT.FindStringSubmatch(l); m != nil {
			if v, _ := strconv.ParseFloat(m[1], 64); v >= 2 {
				f.slow++
				if u := reURL.FindStringSubmatch(l); u != nil {
					slowURLs[u[1]]++
				}
			}
		}
	}
	if f.slow > 0 {
		top := slices.SortedFunc(maps.Keys(slowURLs), func(a, b string) int { return slowURLs[b] - slowURLs[a] })
		if len(top) > 3 {
			top = top[:3]
		}
		f.lines = append(f.lines, fmt.Sprintf("%d slow requests (over 2 s) in the access log, mostly %s; app-api logs slow requests at the same time.", f.slow, strings.Join(top, ", ")))
		f.cause = true
	}
	if f.count5xx > 0 {
		f.lines = append(f.lines, fmt.Sprintf("%d responses with a 5xx status in the lines I read.", f.count5xx))
	}
	switch {
	case strings.Contains(seen, "service_restarted") || strings.Contains(seen, `"state":"active"`) && strings.Contains(seen, "restart"):
		f.lines = append(f.lines, "nginx was restarted and is active again.")
		f.cause = true
	case strings.Contains(seen, "approval_required") || strings.Contains(seen, "pending"):
		f.lines = append(f.lines, "The action needs a person's approval; I asked for it.")
		f.cause, f.needsYou = true, true
	case reUnitDown.MatchString(seen):
		f.lines = append(f.lines, "nginx.service is not running.")
		f.cause = true
	}
	if f.diskUsed = dataUsedPct(seen); f.diskUsed > 0 {
		f.lines = append(f.lines, fmt.Sprintf("The /data volume is %d%% full.", f.diskUsed))
		if f.diskUsed >= 85 {
			f.cause = true
		}
	}
	if strings.Contains(seen, "st-05 failed") {
		f.lines = append(f.lines, "Run run-2026-09 stopped at the QC stage: station st-05 has a salinity spike above 40 psu at 31 m.")
		f.cause, f.runFailed = true, true
	}
	if strings.Contains(seen, "GTiff") {
		f.lines = append(f.lines, "bathy-50m is a GTiff raster of 4000 by 3000 cells in EPSG:3857 with depths down to 312.4 m.")
	}
	if strings.Contains(seen, `"featureCount"`) {
		f.lines = append(f.lines, "map-areas is a polygon layer of 37 areas with the fields name, area_km2 and note.")
	}
	if strings.Contains(seen, "No space left on device") {
		f.lines = append(f.lines, "PostgreSQL reports \"No space left on device\".")
		f.cause = true
	}
	return f
}

// enumObserved is the value of an enum that something the bot saw points to: the verdict of
// the site checks it read or the state of a run it saw fail; unlike enumFact, no default.
func enumObserved(enum []any, f found) (any, bool) {
	var wants []string
	if f.runFailed {
		wants = append(wants, "failed")
	}
	if f.checks > 0 {
		wants = append(wants, map[bool]string{true: "degraded", false: "up"}[f.checksBad > 0])
	}
	for _, want := range wants {
		if slices.Contains(enum, any(want)) {
			return want, true
		}
	}
	return nil, false
}

// enumFact is the value of an enum the found facts point to, if any.
func enumFact(enum []any, f found) (any, bool) {
	wants := []string{map[bool]string{true: "cause_found", false: "no_cause"}[f.cause], "ok", "done", "approve", "keep"}
	if f.checks > 0 {
		wants = append([]string{map[bool]string{true: "degraded", false: "up"}[f.checksBad > 0]}, wants...)
	}
	if f.runFailed {
		wants = append([]string{"failed"}, wants...)
	}
	for _, want := range wants {
		if slices.Contains(enum, any(want)) {
			return want, true
		}
	}
	return nil, false
}

// fill builds a value of a schema: the required fields of an object, the summary for a
// string, the found facts for the fields it knows, and the first enum value otherwise.
func fill(schema map[string]any, name, summary string, f found) any {
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		if pick, ok := enumFact(enum, f); ok {
			return pick
		}
		return enum[0]
	}
	if c, ok := schema["const"]; ok {
		return c
	}
	typ, _ := schema["type"].(string)
	if l, ok := schema["type"].([]any); ok && len(l) > 0 {
		typ = fmt.Sprint(l[0])
	}
	switch typ {
	case "object":
		props, _ := schema["properties"].(map[string]any)
		out := map[string]any{}
		for _, r := range strList(schema["required"]) {
			if p, ok := props[r].(map[string]any); ok {
				out[r] = fill(p, r, summary, f)
			}
		}
		// An optional field the code reads is filled only with a value the facts back: an enum
		// value such as the state of a run the bot saw fail, or need_input when the bot asks back.
		for k, v := range props {
			p, _ := v.(map[string]any)
			if _, done := out[k]; done || p["x-botaru-read-by"] != "code" {
				continue
			}
			if enum, _ := p["enum"].([]any); len(enum) > 0 {
				if pick, ok := enumObserved(enum, f); ok {
					out[k] = pick
				}
			} else if k == "need_input" && p["type"] == "boolean" && f.needsYou {
				out[k] = true
			}
		}
		if p, ok := props["facts"].(map[string]any); ok {
			facts := map[string]any{}
			fp, _ := p["properties"].(map[string]any)
			if _, ok := fp["count_5xx"]; ok && f.count5xx > 0 {
				facts["count_5xx"] = f.count5xx
			}
			if _, ok := fp["count_slow_requests"]; ok && f.slow > 0 {
				facts["count_slow_requests"] = f.slow
			}
			if _, ok := fp["disk_used_pct"]; ok && f.diskUsed > 0 {
				facts["disk_used_pct"] = f.diskUsed
			}
			out["facts"] = facts
		}
		return out
	case "array":
		return []any{}
	case "boolean":
		return name == "need_input" && f.needsYou
	case "integer", "number":
		lo, hasLo := schema["minimum"].(float64)
		if v, ok := f.number(name); ok {
			if hi, ok := schema["maximum"].(float64); ok {
				v = math.Min(v, hi)
			}
			if hasLo {
				v = math.Max(v, lo)
			}
			if typ == "integer" {
				return int64(math.Round(v))
			}
			return v
		}
		if hasLo {
			return lo
		}
		return 0
	case "string":
		s := summary
		if ml, ok := schema["maxLength"].(float64); ok && len(s) > int(ml) {
			s = s[:int(ml)]
		}
		return s
	}
	return nil
}
