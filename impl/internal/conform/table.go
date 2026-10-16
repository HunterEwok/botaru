// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Decision tables (conformance/runner.md, "Decision table"): a fixture that checks one function
// of the core. The rows of the permission table (C2 authorize) run through the test-control
// operation authorize.eval with the tenant kind of the table, and the tables of pure core
// functions through function.eval: the case's given without tenant is the input and the output
// is compared with expect. The kind and the function are written in the comment at the start of
// the file; a row of the permission table that is not a C2 authorize call (a P1 call, a lint
// row) runs as any other case.

// tableInfo is what the comment at the start of a fixture file says about it.
type tableInfo struct {
	// function is the core function of a function.eval table; "" for the permission table.
	function string
	// authorize marks the permission table.
	authorize bool
	// tenantKind is the kind of the tenant of the table's rows: production or test.
	tenantKind string
}

var functionTableRe = regexp.MustCompile(`Decision table for the core function ([a-z][a-z_.]*)`)

// table reads the comment header of a case's file once and returns its table kind, or nil.
func (r *Runner) table(c *Case) *tableInfo {
	if r.tables == nil {
		r.tables = map[string]*tableInfo{}
	}
	if t, ok := r.tables[c.File]; ok {
		return t
	}
	var t *tableInfo
	if f, err := os.Open(filepath.Join(r.Root, c.File)); err == nil {
		var head []string
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			l := sc.Text()
			if !strings.HasPrefix(l, "#") {
				break
			}
			head = append(head, strings.TrimSpace(strings.TrimPrefix(l, "#")))
		}
		f.Close()
		text := strings.Join(head, " ")
		switch {
		case strings.Contains(text, "Fixture kind: decision table") && strings.Contains(text, "authorize.eval"):
			t = &tableInfo{authorize: true, tenantKind: "test"}
			if strings.Contains(text, "kind: production") {
				t.tenantKind = "production"
			}
		default:
			if m := functionTableRe.FindStringSubmatch(text); m != nil && strings.Contains(text, "function.eval") {
				t = &tableInfo{function: m[1], tenantKind: "test"}
			}
		}
	}
	r.tables[c.File] = t
	return t
}

// tableRow is a decision table case ready to run: for the permission table the case with the
// table context that a C2 row does not read taken out of given, and the budget state of the row.
type tableRow struct {
	info      *tableInfo
	c         *Case
	exhausted bool
}

// tableRow returns the row of a decision table case, or nil when the case is no row the
// decision table path runs (then it runs as any other case), or the first thing it needs.
func (r *Runner) tableRow(c *Case) (*tableRow, string) {
	t := r.table(c)
	if t == nil {
		return nil, ""
	}
	if t.function != "" {
		return &tableRow{info: t, c: c}, r.functionNeeds(c)
	}
	if len(c.Steps) == 0 {
		return nil, ""
	}
	for _, st := range c.Steps {
		call := asMap(st["call"])
		if str(call["port"]) != "C2" || str(call["op"]) != "authorize" {
			return nil, ""
		}
		for k := range st {
			switch k {
			case "call", "by", "note", "expect":
			default:
				return nil, ""
			}
		}
	}
	row := &tableRow{info: t}
	g := map[string]any{}
	for k, v := range c.Given {
		switch k {
		case "system", "roles", "catalog":
			// The table context of the service and script rows: a C2 row reads only the
			// principal, its groups, the holds and the budget.
		case "budget":
			b := asMap(v)
			for f := range b {
				if f != "line" && f != "exhausted" {
					return row, "given budget " + f
				}
			}
			row.exhausted = b["exhausted"] == true
		default:
			g[k] = v
		}
	}
	// A row names in given.people only the people whose groups decide it; the people of the
	// table header that the row does not mention take no part in it.
	named := mentions(&Case{Steps: c.Steps})
	for _, p := range r.seed.people {
		if named[p.ID] {
			named["human:"+p.Name] = true
		}
	}
	var people []any
	for _, p := range asList(g["people"]) {
		pm := asMap(p)
		h := str(pm["principal"])
		if h == "system" {
			// system acts through its own group system-ops only (PRM-10), which the core sets.
			for _, gr := range strs(pm["groups"]) {
				if gr != "system-ops" {
					return row, "given system in group " + gr
				}
			}
			continue
		}
		if named[h] {
			people = append(people, p)
		}
	}
	if people != nil {
		g["people"] = people
	} else {
		delete(g, "people")
	}
	rc := *c
	rc.Given = g
	row.c = &rc
	return row, ""
}

// functionNeeds is what a case of a function.eval table needs: steps only of the mock model's
// text, which is the paragraph the function reads, and checks only of the function's output.
func (r *Runner) functionNeeds(c *Case) string {
	for _, st := range c.Steps {
		for k := range st {
			switch k {
			case "model", "note":
			case "expect":
				if len(asMap(st[k])) > 0 {
					return "step expect in a function table"
				}
			default:
				return "step " + k + " in a function table"
			}
		}
		for k := range asMap(st["model"]) {
			if k != "text" {
				return "model " + k + " in a function table"
			}
		}
	}
	return ""
}

// runTable runs a decision table row on a fresh bench.
func (r *Runner) runTable(ctx context.Context, row *tableRow) Result {
	c := row.c
	ep, stop, err := r.Bench.Fresh(ctx)
	if err != nil {
		return Result{Case: c, Status: Failed, Detail: "test bench: " + err.Error()}
	}
	defer stop()
	x := &exec{r: r, ep: ep, c: c, ids: map[string]string{}, granted: map[string][]string{}}
	res := func(s Status, f string, a ...any) Result {
		return Result{Case: c, Status: s, Detail: fmt.Sprintf(f, a...)}
	}
	if row.info.function != "" {
		return x.runFunction(ctx, row.info.function, res)
	}
	for _, seed := range []func(context.Context) error{x.seedBase, x.seedPeople, x.seedHolds} {
		if err := seed(ctx); err != nil {
			if u, ok := err.(errUnchecked); ok {
				return res(Unchecked, "%s", string(u))
			}
			return res(Failed, "seed: %v", err)
		}
	}
	var last *outcome
	for i, st := range c.Steps {
		req := x.expand(asMap(asMap(st["call"])["request"]))
		if row.exhausted {
			cx := asMap(req["context"])
			if cx == nil {
				cx = map[string]any{}
				req["context"] = cx
			}
			cx["budget"] = map[string]any{"exhausted": true}
		}
		status, resp, err := x.post(ctx, x.ep.Test+"/v1/test/authorize.eval", x.ep.TestKey, map[string]any{"request": req, "tenant_kind": row.info.tenantKind})
		if err != nil {
			return res(Failed, "step %d: %v", i+1, err)
		}
		o := problemOutcome(status, resp)
		if o.Unimplemented != "" {
			return res(Unchecked, "core: %s", o.Unimplemented)
		}
		if status == 200 {
			o = &outcome{Result: "ok", Stage: str(resp["stage"])}
			if str(resp["decision"]) == "deny" {
				o = &outcome{Result: "denied", Reason: str(resp["reason"]), Stage: str(resp["stage"]), Detail: strings.Join(strs(resp["needs"]), " ")}
			}
		}
		if e := asMap(st["expect"]); len(e) > 0 {
			if d := compare(e, o); d != "" {
				return res(Failed, "step %d: %s", i+1, d)
			}
		}
		last = o
	}
	if len(c.Expect) > 0 {
		if n := factNeeds(c.Expect); n != "" {
			return res(Unchecked, "%s", n)
		}
		if d := compare(c.Expect, last); d != "" {
			return res(Failed, "%s", d)
		}
	}
	return res(Passed, "")
}

// runFunction calls function.eval with the case's given without tenant and the model's text, and
// compares every expected fact with the output of the same name; a fact the output lacks is
// unchecked.
func (x *exec) runFunction(ctx context.Context, function string, res func(Status, string, ...any) Result) Result {
	in := map[string]any{}
	for k, v := range x.c.Given {
		if k != "tenant" {
			in[k] = v
		}
	}
	for _, st := range x.c.Steps {
		if t := str(asMap(st["model"])["text"]); t != "" {
			in["text"] = t
		}
	}
	status, resp, err := x.post(ctx, x.ep.Test+"/v1/test/function.eval", x.ep.TestKey, map[string]any{"function": function, "input": in})
	if err != nil {
		return res(Failed, "function.eval: %v", err)
	}
	if status != 200 {
		o := problemOutcome(status, resp)
		if o.Unimplemented != "" {
			return res(Unchecked, "core: %s", o.Unimplemented)
		}
		return res(Failed, "function.eval: %s%s", o.Reason, o.why())
	}
	out := asMap(resp["output"])
	keys := make([]string, 0, len(x.c.Expect))
	for k := range x.c.Expect {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "note" {
			continue
		}
		got, ok := out[k]
		if !ok {
			return res(Unchecked, "fact %s", k)
		}
		if want := x.c.Expect[k]; fmt.Sprint(normal(want)) != fmt.Sprint(normal(got)) {
			return res(Failed, "%s %v, expected %v", k, got, want)
		}
	}
	return res(Passed, "")
}

// normal makes numbers of YAML and JSON compare equal: 2 and 2.0 print the same.
func normal(v any) any {
	switch t := v.(type) {
	case int:
		return float64(t)
	case int64:
		return float64(t)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = normal(e)
		}
		return out
	}
	return v
}

// standNeeds checks a stand step of the test bench operations beyond the clock and the loops
// (conformance/runner.md, "Test bench"): what the runner cannot build the body of yet.
func standNeeds(s map[string]any) string {
	op := str(s["op"])
	only := func(fields ...string) string {
		for k := range s {
			if k != "op" && !contains(fields, k) {
				return "stand " + op + " " + k
			}
		}
		return ""
	}
	switch op {
	case "store.snapshot", "store.restore":
		return only("label")
	case "store.fault":
		return only("point", "count")
	case "process.stop":
		// A worker of the bench is a process the runner would have to run itself; the runner
		// runs none yet, so only the core is stopped.
		if p := str(s["process"]); p != "core" {
			return "stand process.stop of " + p
		}
		return only("process", "signal")
	}
	return "stand " + op
}

// standMore runs the stand steps of standNeeds.
func (x *exec) standMore(ctx context.Context, s map[string]any) (*outcome, error) {
	op := str(s["op"])
	switch op {
	case "store.snapshot", "store.restore":
		return x.test(ctx, op, map[string]any{"label": str(s["label"])})
	case "store.fault":
		body := map[string]any{"point": str(s["point"])}
		if n, ok := s["count"].(int); ok {
			body["count"] = n
		}
		return x.test(ctx, op, body)
	case "process.stop":
		// signal: 9 means kill, without the field graceful (runner.md).
		mode := "graceful"
		if fmt.Sprint(s["signal"]) == "9" {
			mode = "kill"
		}
		return x.test(ctx, op, map[string]any{"process": map[string]any{"kind": "core"}, "mode": mode})
	}
	return nil, fmt.Errorf("stand %s", op)
}
