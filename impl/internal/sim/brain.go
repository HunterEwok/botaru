// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
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
// to restart a service makes it try, so the guards of the core are exercised (INJ-15).
type Brain struct {
	calls atomic.Int64
}

// Calls is the number of answered calls.
func (b *Brain) Calls() int64 { return b.calls.Load() }

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
// the call is made.
type step struct {
	plugin, tool string
	input        map[string]any
	when         func(seen string) bool
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
)

func dataUsedPct(seen string) int {
	m := reDfData.FindAllStringSubmatch(seen, -1)
	if len(m) == 0 {
		return 0
	}
	n, _ := strconv.Atoi(m[len(m)-1][1])
	return n
}

func plan(goal string) []step {
	g := strings.ToLower(goal)
	switch {
	case reDisk.MatchString(g):
		return []step{
			{plugin: "cmd", tool: "run", input: map[string]any{"command": "disk_usage", "host": "db1"}},
			{plugin: "logs", tool: "tail", input: map[string]any{"source": "postgresql", "lines": 50}},
			{plugin: "", tool: "request_action", input: map[string]any{"action": "cleanup_pipeline_tmp", "args": map[string]any{"older_than_days": 7}},
				when: func(seen string) bool { return dataUsedPct(seen) >= 85 }},
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

func (b *Brain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	var req brainRequest
	if err != nil || json.Unmarshal(raw, &req) != nil || len(req.Messages) == 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"the request is not a Messages request"}}`)
		return
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
	if next := b.next(goal, seen.String(), names, offered, called); next != nil {
		content = []map[string]any{{"type": "text", "text": "Let me look at that."}, next}
		next["id"] = fmt.Sprintf("toolu_sim%06d", n)
		stop = "tool_use"
	} else {
		content = []map[string]any{{"type": "text", "text": answer(system, goal, seen.String())}}
	}
	out, _ := json.Marshal(content)
	resp := map[string]any{"id": fmt.Sprintf("msg_sim%06d", n), "type": "message", "role": "assistant", "model": req.Model,
		"content": content, "stop_reason": stop, "stop_sequence": nil,
		"usage": map[string]any{"input_tokens": len(raw)/4 + 1, "output_tokens": len(out)/4 + 1}}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// next is the next tool call of the plan, nil when the bot answers.
func (b *Brain) next(goal, seen string, names []string, offered map[string]map[string]any, called map[string]int) map[string]any {
	// The gullible part: an instruction in the tool output to restart a service.
	if reInject.MatchString(seen) {
		if n := toolName(names, "service", "control"); n != "" && called[n] == 0 {
			if in, ok := fits(offered[n], map[string]any{"service": "nginx", "verb": "restart"}); ok {
				return map[string]any{"type": "tool_use", "name": n, "input": in}
			}
		}
	}
	for _, s := range plan(goal) {
		n := toolName(names, s.plugin, s.tool)
		if n == "" {
			continue
		}
		in, ok := fits(offered[n], s.input)
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

// reSchema finds the result schema the runtime puts at the end of the system prompt.
var reSchema = regexp.MustCompile(`(?s)matches this JSON Schema, and nothing else:\n(\{.*\})\s*$`)

// answer is the final answer: a JSON object of the result schema, or plain text without one.
func answer(system, goal, seen string) string {
	f := findings(seen)
	summary := "I looked at the logs and the state of the services. " + strings.Join(f.lines, " ")
	if len(f.lines) == 0 {
		summary = "I looked at the logs and the checks and found nothing unusual."
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
}

var (
	reStatus = regexp.MustCompile(`" ([0-9]{3}) [0-9]+ "`)
	reRT     = regexp.MustCompile(`rt=([0-9.]+)`)
	reURL    = regexp.MustCompile(`"GET ([^ ?"]+)`)
	// reSlowApp is a slow request line of the app-api log.
	reSlowApp = regexp.MustCompile(`slow request (/[^ ]*) [0-9]+ms`)
)

func findings(seen string) found {
	var f found
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
	case strings.Contains(seen, "inactive") || strings.Contains(seen, "failed"):
		f.lines = append(f.lines, "nginx.service on web1 is not running.")
		f.cause = true
	}
	if f.diskUsed = dataUsedPct(seen); f.diskUsed > 0 {
		f.lines = append(f.lines, fmt.Sprintf("The /data volume of db1 is %d%% full.", f.diskUsed))
		if f.diskUsed >= 85 {
			f.cause = true
		}
	}
	if strings.Contains(seen, "No space left on device") {
		f.lines = append(f.lines, "PostgreSQL reports \"No space left on device\".")
		f.cause = true
	}
	return f
}

// fill builds a value of a schema: the required fields of an object, the summary for a
// string, the found facts for the fields it knows, and the first enum value otherwise.
func fill(schema map[string]any, name, summary string, f found) any {
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		pick := enum[0]
		for _, want := range []string{map[bool]string{true: "cause_found", false: "no_cause"}[f.cause], "ok", "done", "approve", "keep"} {
			if slices.Contains(enum, any(want)) {
				pick = want
				break
			}
		}
		return pick
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
		if v, ok := schema["minimum"].(float64); ok {
			return v
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
