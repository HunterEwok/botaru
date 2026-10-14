// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// Endpoints are what the runner knows of a test bench: the base addresses of P1, P2 and the
// test-control port, an operator credential per tenant and the test bench key.
type Endpoints struct {
	P1, P2, Test string
	Operators    map[string]string
	TestKey      string
}

// Bench starts fresh test benches: an empty store created as a test bench store and a core over
// it. The runner seeds every bench itself through P2 and the test-control port.
type Bench interface {
	Fresh(ctx context.Context) (*Endpoints, func(), error)
}

// Status is the outcome of one case.
type Status string

const (
	Passed    Status = "passed"
	Failed    Status = "failed"
	Unchecked Status = "unchecked"
	Skipped   Status = "skipped"
)

// Result is the outcome of one case with the first thing it needs (unchecked) or the first
// mismatch (failed).
type Result struct {
	Case   *Case
	Rules  []string
	Status Status
	Detail string
}

// Runner runs cases of a claim against fresh benches.
type Runner struct {
	Root   string
	Rules  map[string]*Rule
	Claim  Claim
	Bench  Bench
	Client *http.Client

	spec  *spec.Spec
	seed  *seedData
	kinds map[string]string
}

// NewRunner loads the specification the runner needs from the skeleton root.
func NewRunner(root string, claim Claim, bench Bench) (*Runner, error) {
	rules, err := LoadRules(root)
	if err != nil {
		return nil, err
	}
	sp, err := spec.Load(root)
	if err != nil {
		return nil, err
	}
	seed, err := loadSeed(root)
	if err != nil {
		return nil, err
	}
	caps, err := sp.Registry("spec/model/capabilities.yaml")
	if err != nil {
		return nil, err
	}
	seed.setClasses(caps.Entries)
	reg, err := sp.Registry("spec/manifests/kinds.yaml")
	if err != nil {
		return nil, err
	}
	kinds := map[string]string{}
	for _, e := range reg.Entries {
		kinds[str(e["id"])] = str(e["scope"])
	}
	return &Runner{Root: root, Rules: rules, Claim: claim, Bench: bench, Client: &http.Client{Timeout: 30 * time.Second},
		spec: sp, seed: seed, kinds: kinds}, nil
}

// Run selects the cases of the claim and runs them; skipped cases are reported as skipped.
func (r *Runner) Run(ctx context.Context, cases []*Case) []Result {
	in, skipped := Select(r.Rules, cases, r.Claim)
	var out []Result
	for _, c := range in {
		res := r.runCase(ctx, c)
		res.Rules = InScopeRules(r.Rules, c, r.Claim)
		out = append(out, res)
	}
	for _, c := range skipped {
		out = append(out, Result{Case: c, Rules: InScopeRules(r.Rules, c, r.Claim), Status: Skipped, Detail: "the case is of a later version"})
	}
	return out
}

func (r *Runner) runCase(ctx context.Context, c *Case) Result {
	if need := r.Needs(c); need != "" {
		return Result{Case: c, Status: Unchecked, Detail: need}
	}
	ep, stop, err := r.Bench.Fresh(ctx)
	if err != nil {
		return Result{Case: c, Status: Failed, Detail: "test bench: " + err.Error()}
	}
	defer stop()
	x := &exec{r: r, ep: ep, c: c, ids: map[string]string{}, granted: map[string][]string{}}
	return x.run(ctx)
}

// Needs returns the first thing a case needs that the runner cannot do yet, or "" when the
// runner can run it. What the core itself lacks shows at run time (unimplemented).
func (r *Runner) Needs(c *Case) string {
	keys := make([]string, 0, len(c.Given))
	for k := range c.Given {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch k {
		case "since":
		case "tenant":
			if t, ok := c.Given[k].(string); !ok || t != "demo" {
				return "given tenant other than demo"
			}
		case "people":
			for _, p := range asList(c.Given[k]) {
				pm := asMap(p)
				if _, ok := pm["caps"]; ok {
					return "given people with individual caps"
				}
				if t := str(pm["tenant"]); t != "" && t != "demo" {
					return "given people of another tenant"
				}
				if s := str(pm["state"]); s != "" && s != "active" {
					return "given people state " + s
				}
				if !strings.HasPrefix(str(pm["principal"]), "human:") {
					return "given people that are not persons"
				}
			}
		case "manifest", "manifests":
		case "holds":
			for _, h := range asList(c.Given[k]) {
				hm := asMap(h)
				for f := range hm {
					switch f {
					case "id", "scope", "effects", "reason", "until", "tenant":
					case "author_kind":
						if str(hm[f]) != "operator" {
							return "given hold by " + str(hm[f])
						}
					case "author":
						if _, ok := hm["author_kind"]; !ok {
							return "given hold by a person"
						}
					default:
						return "given hold " + f
					}
				}
			}
		default:
			return "given " + k
		}
	}
	for _, st := range c.Steps {
		if n := r.stepNeeds(st); n != "" {
			return n
		}
		if n := factNeeds(asMap(st["expect"])); n != "" {
			return n
		}
	}
	return factNeeds(c.Expect)
}

var runnerOps = map[string]bool{
	"P2.apply": true, "P2.principal.add": true, "P2.principal.off": true, "P2.principal.on": true,
	"P2.principal.set_identity": true, "P2.principal.offboard": true, "P2.grant": true, "P2.ungrant": true,
	"P2.audit.verify": true, "P2.health": true, "P2.hold.set": true, "P2.hold.release": true, "P2.hold.list": true,
}

func (r *Runner) stepNeeds(st map[string]any) string {
	for k := range st {
		switch k {
		case "call", "stand", "lint", "expect", "note", "at", "by":
		default:
			if !isVerb(k) {
				return "step modifier " + k
			}
		}
	}
	switch {
	case st["call"] != nil:
		call := asMap(st["call"])
		op := str(call["port"]) + "." + str(call["op"])
		if !runnerOps[op] {
			return "call " + op
		}
		if by := str(st["by"]); by != "" && by != "operator" {
			return "call " + op + " by " + by
		}
	case st["stand"] != nil:
		s := asMap(st["stand"])
		switch op := str(s["op"]); op {
		case "clock.set":
		case "clock.advance":
			if _, err := parseDuration(str(s["by"])); err != nil {
				return "stand clock.advance by a named period"
			}
		case "loop.tick":
			for k := range s {
				if k != "op" && k != "loop" && k != "copies" && k != "concurrent" {
					return "stand loop.tick " + k
				}
			}
		default:
			return "stand " + op
		}
	case st["lint"] != nil:
		for k := range asMap(st["lint"]) {
			if k != "tenant_yaml" {
				return "lint " + k
			}
		}
		// A tenant_yaml row without kind: test lints a production tenant, and a test bench
		// refuses every production tenant (APR-9), so an apply would not reach the row's check.
		if str(asMap(asMap(st["lint"])["tenant_yaml"])["kind"]) != "test" {
			return "lint of a production tenant on a test bench"
		}
	default:
		for _, v := range []string{"model", "occur", "check"} {
			if _, ok := st[v]; ok {
				return "step " + v
			}
		}
		return "step without a verb"
	}
	if at := st["at"]; at != nil {
		if _, err := parseAt(str(at)); err != nil {
			return "step at " + str(at)
		}
	}
	return ""
}

func isVerb(k string) bool {
	switch k {
	case "call", "stand", "model", "lint", "occur", "check":
		return true
	}
	return false
}

// checkedFacts are the expectation keys the runner compares.
var checkedFacts = map[string]bool{"result": true, "reason": true, "stage": true, "error": true, "note": true, "field": true}

func factNeeds(e map[string]any) string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !checkedFacts[k] {
			return "fact " + k
		}
	}
	return ""
}

// exec is one case on one bench.
type exec struct {
	r   *Runner
	ep  *Endpoints
	c   *Case
	ids map[string]string // training handle -> principal id
	// granted are the groups the seed gave each person.
	granted map[string][]string
	start   time.Time
	now     time.Time
}

// outcome is what one action gave, in the terms of the expectations.
type outcome struct {
	Result string // ok, rejected, denied
	Reason string
	Stage  string
	Error  string
	Path   string
	Detail string
	// Unimplemented is set when the core answered that this build lacks the operation.
	Unimplemented string
}

func (x *exec) run(ctx context.Context) Result {
	res := func(s Status, f string, a ...any) Result {
		return Result{Case: x.c, Status: s, Detail: fmt.Sprintf(f, a...)}
	}
	if err := x.seedBase(ctx); err != nil {
		if u, ok := err.(errUnchecked); ok {
			return res(Unchecked, "%s", string(u))
		}
		return res(Failed, "seed: %v", err)
	}
	if err := x.seedPeople(ctx); err != nil {
		if u, ok := err.(errUnchecked); ok {
			return res(Unchecked, "%s", string(u))
		}
		return res(Failed, "given people: %v", err)
	}
	var last *outcome
	if ms := x.givenManifests(); len(ms) > 0 {
		// Fields and kinds of a later version than the claim go before apply (MAN-11).
		if _, err := testbench.DropUnsupported(x.r.spec, ms); err != nil {
			return res(Unchecked, "given manifests: %v", err)
		}
		o, err := x.apply(ctx, ms, false)
		if err != nil {
			return res(Failed, "given manifests: %v", err)
		}
		if o.Unimplemented != "" {
			return res(Unchecked, "core: %s", o.Unimplemented)
		}
		if len(x.c.Steps) > 0 && o.Result != "ok" {
			return res(Failed, "given manifests were not applied: %s %s", o.Error, o.Path)
		}
		last = o
	}
	if err := x.seedHolds(ctx); err != nil {
		if u, ok := err.(errUnchecked); ok {
			return res(Unchecked, "%s", string(u))
		}
		return res(Failed, "given holds: %v", err)
	}
	for i, st := range x.c.Steps {
		o, err := x.step(ctx, st)
		if err != nil {
			return res(Failed, "step %d: %v", i+1, err)
		}
		if o.Unimplemented != "" {
			return res(Unchecked, "core: %s", o.Unimplemented)
		}
		if e := asMap(st["expect"]); len(e) > 0 {
			if d := compare(e, o); d != "" {
				return res(Failed, "step %d: %s", i+1, d)
			}
		}
		last = o
	}
	if len(x.c.Expect) > 0 {
		if last == nil {
			return res(Failed, "an expectation without an action")
		}
		if d := compare(x.c.Expect, last); d != "" {
			return res(Failed, "%s", d)
		}
	}
	return res(Passed, "")
}

// compare checks an outcome against an expectation and returns the first mismatch.
func compare(e map[string]any, o *outcome) string {
	if v, ok := e["result"]; ok && str(v) != o.Result {
		return fmt.Sprintf("result %s, expected %s%s", o.Result, str(v), o.why())
	}
	if v, ok := e["reason"]; ok {
		if _, has := e["result"]; !has && o.Result == "ok" {
			return fmt.Sprintf("result ok, expected a refusal with %s", str(v))
		}
		if str(v) != o.Reason && str(v) != o.Error {
			return fmt.Sprintf("reason %q, expected %s%s", o.Reason, str(v), o.why())
		}
	}
	if v, ok := e["stage"]; ok && !stageMatches(str(v), o) {
		return fmt.Sprintf("stage %q, expected %s%s", o.Stage, str(v), o.why())
	}
	if v, ok := e["error"]; ok && str(v) != o.Error {
		return fmt.Sprintf("error %q, expected %s%s", o.Error, str(v), o.why())
	}
	if v, ok := e["field"]; ok {
		f := str(v)
		if o.Path != "/"+f && !strings.HasSuffix(o.Path, "/"+f) && !strings.Contains(o.Path, "/"+f+"/") {
			return fmt.Sprintf("field at %q, expected %s%s", o.Path, f, o.why())
		}
	}
	return ""
}

// stageMatches compares the stage of a refusal. The wire does not tell the manifest lint from
// applying the set: both come back as errors of P2 apply, so either stage matches an apply error.
func stageMatches(want string, o *outcome) bool {
	switch want {
	case "lint", "apply":
		return o.Stage == "lint" || o.Stage == "apply"
	}
	return want == o.Stage
}

func (o *outcome) why() string {
	var parts []string
	for _, p := range []struct{ k, v string }{{"reason", o.Reason}, {"error", o.Error}, {"path", o.Path}, {"detail", o.Detail}} {
		if p.v != "" {
			parts = append(parts, p.k+" "+p.v)
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// seedBase brings a fresh bench to the seed of evals/fixtures/seed.yaml as far as the base set
// reaches: the hello-bot manifests with tenant demo as kind: test, the groups of the seed and
// its people with their identities and groups (conformance/runner.md, "Initial state").
func (x *exec) seedBase(ctx context.Context) error {
	ms, err := testbench.HelloBotManifests(x.r.Root)
	if err != nil {
		return err
	}
	ms = append(ms, x.r.seed.groupManifests("demo")...)
	// The seed groups may request the example actions; apply refuses a permission on an action
	// outside the set (APR-5, cap_unknown_action), so the actions come along.
	acts, err := testbench.ExampleActions(x.r.Root)
	if err != nil {
		return err
	}
	ms = append(ms, acts...)
	o, err := x.apply(ctx, ms, false)
	if err != nil {
		return err
	}
	if o.Result != "ok" {
		return fmt.Errorf("the base set was not applied: %s %s", o.Error, o.Path)
	}
	// Only the people the case names are seeded: a grant of a privileged group needs the
	// verification code flow (PRM-13), and a case that names nobody should not depend on it.
	named := mentions(x.c)
	for _, p := range x.r.seed.people {
		if p.Tenant != "demo" || p.ID == "" || p.Kind != "human" || !named["human:"+p.Name] {
			continue
		}
		x.ids["human:"+p.Name] = p.ID
		if err := x.addPerson(ctx, p.ID, p.Identity); err != nil {
			return err
		}
		for _, g := range p.Groups {
			if listed := x.givenGroups("human:" + p.Name); listed != nil && !listed[g] {
				continue
			}
			o, err := x.p2(ctx, "demo", "grant", map[string]any{"op": "grant", "subject": p.ID, "group": g})
			if err != nil {
				return err
			}
			if o.Reason == "privileged_group" {
				return errUnchecked("core: grant of the privileged group " + g + " (PRM-13)")
			}
			if o.Result != "ok" {
				return fmt.Errorf("grant %s %s: %s%s", p.Name, g, o.Reason, o.why())
			}
			x.granted["human:"+p.Name] = append(x.granted["human:"+p.Name], g)
		}
	}
	return nil
}

// givenGroups returns the groups given.people lists for a person, or nil when it does not
// list the person.
func (x *exec) givenGroups(handle string) map[string]bool {
	for _, p := range asList(x.c.Given["people"]) {
		pm := asMap(p)
		if str(pm["principal"]) == handle {
			out := map[string]bool{}
			for _, g := range strs(pm["groups"]) {
				out[g] = true
			}
			return out
		}
	}
	return nil
}

// seedHolds sets the holds of given.holds through P2 hold.set by the operator of farm; a
// fixture id of a hold becomes a handle for the requests of the steps.
func (x *exec) seedHolds(ctx context.Context) error {
	for _, h := range asList(x.c.Given["holds"]) {
		hm := asMap(h)
		reason := str(hm["reason"])
		if reason == "" {
			reason = "manual"
		}
		body := map[string]any{"scope": hm["scope"], "effects": hm["effects"], "reason": reason, "until": hm["until"]}
		status, resp, err := x.post(ctx, x.ep.P2+"/v1/p2/hold.set", x.ep.Operators["farm"], body)
		if err != nil {
			return err
		}
		if status != 200 {
			o := problemOutcome(status, resp)
			if o.Unimplemented != "" {
				return errUnchecked("core: " + o.Unimplemented)
			}
			if hm["until"] != nil && o.Reason == "input_schema_invalid" {
				return errUnchecked("given hold with an until before the bench clock")
			}
			return fmt.Errorf("hold.set: %s%s", o.Reason, o.why())
		}
		if id := str(hm["id"]); id != "" {
			x.ids[id] = str(resp["id"])
		}
	}
	return nil
}

// errUnchecked marks a seed step the core cannot do yet: the case is unchecked, not failed.
type errUnchecked string

func (e errUnchecked) Error() string { return string(e) }

// mentions collects the strings of a case that name a principal by handle.
func mentions(c *Case) map[string]bool {
	out := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case string:
			if strings.HasPrefix(t, "human:") {
				out[t] = true
			}
		}
	}
	walk(c.Given)
	for _, st := range c.Steps {
		walk(map[string]any(st))
	}
	return out
}

func (x *exec) addPerson(ctx context.Context, id, identity string) error {
	req := map[string]any{"op": "principal.add", "subject": id, "kind": "human"}
	if identity != "" {
		req["identity"] = identity
	}
	return x.p2ok(ctx, "demo", "principal.add", req)
}

// seedPeople applies given.people: a listed person gets exactly the listed groups.
func (x *exec) seedPeople(ctx context.Context) error {
	for _, p := range asList(x.c.Given["people"]) {
		pm := asMap(p)
		handle := str(pm["principal"])
		id, known := x.ids[handle]
		if !known {
			if strings.HasPrefix(handle, "human:p_") {
				id = handle
			} else {
				id = newPrincipalID()
			}
			x.ids[handle] = id
			if err := x.addPerson(ctx, id, ""); err != nil {
				return err
			}
		}
		have := x.granted[handle]
		for _, g := range strs(pm["groups"]) {
			if contains(have, g) {
				continue
			}
			o, err := x.p2(ctx, "demo", "grant", map[string]any{"op": "grant", "subject": id, "group": g})
			if err != nil {
				return err
			}
			if o.Reason == "privileged_group" {
				return errUnchecked("core: grant of the privileged group " + g + " (PRM-13)")
			}
			if o.Result != "ok" {
				return fmt.Errorf("grant %s %s: %s%s", handle, g, o.Reason, o.why())
			}
		}
	}
	return nil
}

// givenManifests completes the manifests of given: apiVersion and, for a tenant-scoped kind,
// metadata.tenant of the case's tenant.
func (x *exec) givenManifests() []map[string]any {
	var raw []any
	if m := x.c.Given["manifest"]; m != nil {
		raw = append(raw, m)
	}
	raw = append(raw, asList(x.c.Given["manifests"])...)
	var out []map[string]any
	for _, v := range raw {
		m := deepCopy(asMap(v))
		if m == nil {
			continue
		}
		if _, ok := m["apiVersion"]; !ok {
			m["apiVersion"] = "botaru/v1alpha1"
		}
		md := asMap(m["metadata"])
		if md == nil {
			md = map[string]any{}
			m["metadata"] = md
		}
		// On a test bench every tenant is a test tenant (conformance/runner.md, "Initial state").
		if str(m["kind"]) == "Tenant" {
			if sp := asMap(m["spec"]); sp != nil {
				sp["kind"] = "test"
			}
		}
		if x.r.kinds[str(m["kind"])] == "tenant" {
			if _, ok := md["tenant"]; !ok {
				md["tenant"] = "demo"
			}
		}
		out = append(out, m)
	}
	return out
}

func (x *exec) step(ctx context.Context, st map[string]any) (*outcome, error) {
	if at := str(st["at"]); at != "" {
		if err := x.at(ctx, at); err != nil {
			return nil, err
		}
	}
	switch {
	case st["call"] != nil:
		call := asMap(st["call"])
		op := str(call["op"])
		req := x.expand(asMap(call["request"]))
		if op == "apply" {
			return x.applyBody(ctx, req)
		}
		if strings.HasPrefix(op, "hold.") {
			// Holds go with the operator credential of farm, which reaches every tenant and
			// the scopes farm and seat (PRM-14).
			return x.p2(ctx, "farm", op, req)
		}
		return x.p2(ctx, "demo", op, req)
	case st["stand"] != nil:
		return x.stand(ctx, asMap(st["stand"]))
	case st["lint"] != nil:
		return x.lint(ctx, asMap(st["lint"]))
	}
	return nil, fmt.Errorf("no verb")
}

// lint applies manifest fragments on top of the seed with dry_run.
func (x *exec) lint(ctx context.Context, l map[string]any) (*outcome, error) {
	ms, err := manifest.ReadFiles([]string{filepath.Join(x.r.Root, "examples/tenants/demo/tenant.yaml")})
	if err != nil {
		return nil, err
	}
	// Needs lets through only rows of kind: test; the fragment sets it.
	t := ms[0].Value
	sp := asMap(t["spec"])
	for k, v := range asMap(l["tenant_yaml"]) {
		sp[k] = v
	}
	set := []map[string]any{t}
	if _, err := testbench.DropUnsupported(x.r.spec, set); err != nil {
		return nil, err
	}
	return x.apply(ctx, set, true)
}

func (x *exec) stand(ctx context.Context, s map[string]any) (*outcome, error) {
	switch op := str(s["op"]); op {
	case "clock.set":
		return x.test(ctx, op, map[string]any{"op": op, "at": str(s["at"])})
	case "clock.advance":
		d, err := parseDuration(str(s["by"]))
		if err != nil {
			return nil, err
		}
		return x.test(ctx, op, map[string]any{"op": op, "by_s": int64(d / time.Second)})
	case "loop.tick":
		body := map[string]any{"loop": str(s["loop"])}
		if n, ok := s["copies"].(int); ok {
			body["copies"] = n
		}
		return x.test(ctx, op, body)
	default:
		return nil, fmt.Errorf("stand %s", op)
	}
}

// at sets the store clock before a step: a UTC instant, or an offset from the case start.
func (x *exec) at(ctx context.Context, at string) error {
	if d, err := parseDuration(at); err == nil {
		if x.start.IsZero() {
			// The case starts at the store clock of the first timed step: a shift by zero
			// reads it.
			if _, err := x.test(ctx, "clock.advance", map[string]any{"op": "clock.advance", "by_s": 0}); err != nil {
				return err
			}
		}
		target := x.start.Add(d)
		if !target.After(x.now) {
			return nil
		}
		_, err := x.test(ctx, "clock.advance", map[string]any{"op": "clock.advance", "by_s": int64(target.Sub(x.now) / time.Second)})
		return err
	}
	_, err := x.test(ctx, "clock.set", map[string]any{"op": "clock.set", "at": at})
	return err
}

func (x *exec) test(ctx context.Context, op string, body map[string]any) (*outcome, error) {
	status, resp, err := x.post(ctx, x.ep.Test+"/v1/test/"+op, x.ep.TestKey, body)
	if err != nil {
		return nil, err
	}
	o := problemOutcome(status, resp)
	if o.Result == "ok" {
		if t, err := time.Parse(time.RFC3339Nano, str(resp["now"])); err == nil {
			if x.start.IsZero() {
				x.start = t
			}
			x.now = t
		}
	} else if o.Unimplemented == "" {
		return nil, fmt.Errorf("test-control %s: %s %s", op, o.Reason, str(resp["detail"]))
	}
	return o, nil
}

// apply sends a manifest set to P2 apply.
func (x *exec) apply(ctx context.Context, ms []map[string]any, dryRun bool) (*outcome, error) {
	list := make([]any, len(ms))
	for i, m := range ms {
		list[i] = m
	}
	body := map[string]any{"commit": strings.Repeat("c", 40), "manifests": list}
	if dryRun {
		body["dry_run"] = true
	}
	return x.applyBody(ctx, body)
}

func (x *exec) applyBody(ctx context.Context, body map[string]any) (*outcome, error) {
	status, resp, err := x.post(ctx, x.ep.P2+"/v1/p2/apply", x.ep.Operators["demo"], body)
	if err != nil {
		return nil, err
	}
	if status != 200 {
		return problemOutcome(status, resp), nil
	}
	errs := asList(resp["errors"])
	if len(errs) == 0 {
		return &outcome{Result: "ok"}, nil
	}
	e := asMap(errs[0])
	o := &outcome{Result: "rejected", Error: str(e["code"]), Path: str(e["path"]), Stage: "apply"}
	if o.Error == "schema_invalid" {
		o.Stage = "schema"
	}
	return o, nil
}

func (x *exec) p2(ctx context.Context, tenant, op string, body map[string]any) (*outcome, error) {
	status, resp, err := x.post(ctx, x.ep.P2+"/v1/p2/"+op, x.ep.Operators[tenant], body)
	if err != nil {
		return nil, err
	}
	return problemOutcome(status, resp), nil
}

func (x *exec) p2ok(ctx context.Context, tenant, op string, body map[string]any) error {
	o, err := x.p2(ctx, tenant, op, body)
	if err != nil {
		return err
	}
	if o.Result != "ok" {
		return fmt.Errorf("%s: %s%s", op, o.Reason, o.why())
	}
	return nil
}

// problemOutcome reads an HTTP answer: 200 is ok, a 403 refusal is a C2 denial, any other
// problem details a rejection.
func problemOutcome(status int, resp map[string]any) *outcome {
	if status == 200 {
		return &outcome{Result: "ok"}
	}
	o := &outcome{Result: "rejected", Reason: str(resp["reason"]), Detail: str(resp["detail"])}
	if status == 403 {
		o.Result = "denied"
	}
	if o.Reason == "input_schema_invalid" {
		o.Stage = "schema"
	}
	if d := str(resp["detail"]); status == 404 && (strings.Contains(d, "not implemented in this build") || strings.Contains(d, "this build has no")) {
		o.Unimplemented = d
	}
	return o
}

func (x *exec) post(ctx context.Context, url, token string, body map[string]any) (int, map[string]any, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return 0, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := x.r.Client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, nil, err
	}
	var out map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &out); err != nil {
			return 0, nil, fmt.Errorf("%s: %d, not JSON", url, resp.StatusCode)
		}
	}
	return resp.StatusCode, out, nil
}

// expand replaces training handles human:<handle> in a request body by the principal ids of
// the bench.
func (x *exec) expand(m map[string]any) map[string]any {
	out, _ := x.expandValue(deepCopy(m)).(map[string]any)
	return out
}

func (x *exec) expandValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			t[k] = x.expandValue(e)
		}
	case []any:
		for i, e := range t {
			t[i] = x.expandValue(e)
		}
	case string:
		if id, ok := x.ids[t]; ok {
			return id
		}
	}
	return v
}

var durRe = regexp.MustCompile(`^(\d+)(s|m|h|d)$`)

// parseDuration reads a fixture duration: 90s, 15m, 2h, 7d.
func parseDuration(s string) (time.Duration, error) {
	m := durRe.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("not a duration: %q", s)
	}
	n, _ := strconv.Atoi(m[1])
	unit := map[string]time.Duration{"s": time.Second, "m": time.Minute, "h": time.Hour, "d": 24 * time.Hour}[m[2]]
	return time.Duration(n) * unit, nil
}

func parseAt(s string) (time.Time, error) {
	if d, err := parseDuration(s); err == nil {
		return time.Time{}.Add(d), nil
	}
	return time.Parse(time.RFC3339, s)
}

func deepCopy(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	b, _ := json.Marshal(m)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
