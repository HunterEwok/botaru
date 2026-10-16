// SPDX-License-Identifier: AGPL-3.0-only

package conform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	// kindSchemas are the spec schemas of the manifest kinds (kinds.yaml).
	kindSchemas map[string]string
	// seedTypes are the example task types the seed groups grant (seedData.typeNames).
	seedTypes []map[string]any
	// tables are the decision table kinds of the fixture files read so far (table.go).
	tables map[string]*tableInfo
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
	kinds, kindSchemas := map[string]string{}, map[string]string{}
	for _, e := range reg.Entries {
		kinds[str(e["id"])] = str(e["scope"])
		kindSchemas[str(e["id"])] = str(e["schema"])
	}
	seedTypes, err := loadExampleTypes(root, sp, seed.typeNames())
	if err != nil {
		return nil, err
	}
	return &Runner{Root: root, Rules: rules, Claim: claim, Bench: bench, Client: &http.Client{Timeout: 30 * time.Second},
		spec: sp, seed: seed, kinds: kinds, kindSchemas: kindSchemas, seedTypes: seedTypes}, nil
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
	if row, need := r.tableRow(c); row != nil {
		if need == "" && row.info.function == "" {
			need = r.givenNeeds(row.c)
		}
		if need != "" {
			return Result{Case: c, Status: Unchecked, Detail: need}
		}
		return r.runTable(ctx, row)
	}
	if pc := r.plainRow(c); pc != nil {
		res := r.runCase(ctx, pc)
		res.Case = c
		return res
	}
	if need := r.Needs(c); need != "" {
		return Result{Case: c, Status: Unchecked, Detail: need}
	}
	ep, stop, err := r.Bench.Fresh(ctx)
	if err != nil {
		return Result{Case: c, Status: Failed, Detail: "test bench: " + err.Error()}
	}
	defer stop()
	x := newExec(r, ep, c)
	defer func() {
		for _, f := range x.cleanup {
			f()
		}
	}()
	return x.run(ctx)
}

// Needs returns the first thing a case needs that the runner cannot do yet, or "" when the
// runner can run it. What the core itself lacks shows at run time (unimplemented).
func (r *Runner) Needs(c *Case) string {
	if n := r.givenNeeds(c); n != "" {
		return n
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

// givenNeeds returns the first key of given the runner cannot seed, or "".
func (r *Runner) givenNeeds(c *Case) string {
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
		case "catalog":
			if n := catalogNeeds(c.Given[k]); n != "" {
				return n
			}
		case "lint":
			// The name of the lint family the case's manifests check (governance/charter): a
			// label without data.
			if _, ok := c.Given[k].(string); !ok {
				return "given lint " + fmt.Sprint(c.Given[k])
			}
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
			if strings.HasSuffix(k, "_yaml") {
				// A fragment in given is the row's lint when the case has no steps.
				if len(c.Steps) > 0 {
					return "given " + k + " with steps"
				}
				if n := lintNeeds(map[string]any{k: c.Given[k]}); n != "" {
					return n
				}
				continue
			}
			return "given " + k
		}
	}
	return ""
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
			if n := standNeeds(s); n != "" {
				return n
			}
		}
	case st["lint"] != nil:
		if n := lintNeeds(asMap(st["lint"])); n != "" {
			return n
		}
	case st["occur"] != nil:
		if n := occurNeeds(st["occur"]); n != "" {
			return n
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
	if a, ok := e["apply"]; ok {
		if n := factNeeds(applyFact(map[string]any{"apply": a})); n != "" {
			return "fact apply." + strings.TrimPrefix(n, "fact ")
		}
		if applyFact(map[string]any{"apply": a}) == nil {
			return "fact apply"
		}
		e = withoutApply(e)
	}
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if k == "alarm" {
			if n := alarmNeeds(e[k]); n != "" {
				return n
			}
			continue
		}
		if !checkedFacts[k] {
			return "fact " + k
		}
	}
	return ""
}

func newExec(r *Runner, ep *Endpoints, c *Case) *exec {
	return &exec{r: r, ep: ep, c: c, ids: map[string]string{}, identities: map[string]string{}, granted: map[string][]string{}}
}

// exec is one case on one bench.
type exec struct {
	r   *Runner
	ep  *Endpoints
	c   *Case
	ids map[string]string // training handle -> principal id
	// identities are the channel identities of the seeded people by handle.
	identities map[string]string
	// granted are the groups the seed gave each person.
	granted map[string][]string
	start   time.Time
	now     time.Time
	// types are example task types the seed applies besides the hello-bot set: a row of the
	// permission table names them.
	types []map[string]any
	// chanToken is the P1 token of the runner's channel worker once enrolled (channel.go), and
	// updates counts the activities it submitted.
	chanToken string
	updates   int
	// base is the manifest set the seed applied (seedBase).
	base []map[string]any
	// catalog is the spec of the given catalog; mon is the monitoring system of the bench once
	// started (monitor.go); cleanup stops what the case started.
	catalog map[string]any
	mon     *bench5
	cleanup []func()
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
	// Violations are the places where a manifest the core refused by its kind schema breaks
	// that schema (manifestRefusal).
	Violations []violation
	// Missing are the required fields a refused manifest lacks, as dotted paths of the manifest
	// (spec.title, metadata.name): a fixture manifest without them is refused for them.
	Missing []string
	// Alts are the other errors of a refused apply: an expectation matches the set when it
	// matches one of its errors.
	Alts []*outcome
	// Touched are the indexes in the applied set of the objects a lint fragment changed; nil
	// when the outcome is not of a fragment.
	Touched map[int]bool
}

// violation is one place a manifest breaks its kind schema: the pointer in the apply body and
// the schema's message.
type violation struct{ Path, Message string }

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
	if o, err := x.seedCatalog(ctx); err != nil {
		return res(Failed, "given catalog: %v", err)
	} else if o != nil && o.Result != "ok" {
		return res(Failed, "given catalog was not applied: %s %s%s", o.Error, o.Path, o.why())
	}
	// applied is the outcome of the given manifests or fragments, which expect.apply names.
	var last, applied *outcome
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
			if n := incomplete(nil, o); n != "" {
				return res(Unchecked, "%s", n)
			}
			return res(Failed, "given manifests were not applied: %s %s", o.Error, o.Path)
		}
		last, applied = o, o
	}
	if err := x.seedHolds(ctx); err != nil {
		if u, ok := err.(errUnchecked); ok {
			return res(Unchecked, "%s", string(u))
		}
		return res(Failed, "given holds: %v", err)
	}
	if frags := x.givenFragments(); len(frags) > 0 {
		o, err := x.lint(ctx, frags)
		if err != nil {
			if u, ok := err.(errUnchecked); ok {
				return res(Unchecked, "%s", string(u))
			}
			return res(Failed, "given fragments: %v", err)
		}
		if o.Unimplemented != "" {
			return res(Unchecked, "core: %s", o.Unimplemented)
		}
		last, applied = o, o
	}
	for i, st := range x.c.Steps {
		o, err := x.step(ctx, st)
		if err != nil {
			if u, ok := err.(errUnchecked); ok {
				return res(Unchecked, "step %d: %s", i+1, string(u))
			}
			return res(Failed, "step %d: %v", i+1, err)
		}
		if o.Unimplemented != "" {
			return res(Unchecked, "core: %s", o.Unimplemented)
		}
		if e := asMap(st["expect"]); len(e) > 0 {
			if n := incomplete(e, o); n != "" {
				return res(Unchecked, "step %d: %s", i+1, n)
			}
			if d := x.compare(e, o); d != "" {
				return res(Failed, "step %d: %s", i+1, d)
			}
		}
		last = o
	}
	if e := applyFact(x.c.Expect); e != nil {
		// expect.apply is the outcome of applying the given manifests (or fragments).
		if applied == nil {
			return res(Failed, "an apply expectation without manifests")
		}
		if n := incomplete(e, applied); n != "" {
			return res(Unchecked, "%s", n)
		}
		if d := compare(e, applied); d != "" {
			return res(Failed, "apply: %s", d)
		}
	}
	if e := withoutApply(x.c.Expect); len(e) > 0 {
		if last == nil {
			return res(Failed, "an expectation without an action")
		}
		if n := incomplete(e, last); n != "" {
			return res(Unchecked, "%s", n)
		}
		if d := x.compare(e, last); d != "" {
			return res(Failed, "%s", d)
		}
	}
	return res(Passed, "")
}

var manifestPtr = regexp.MustCompile(` at (/manifests/([0-9]+)(/\S*)?)$`)

// manifestRefusal reads an input_schema_invalid refusal of P2 apply that points into a manifest:
// the manifest fails the schema of its kind, which is the lint error schema_invalid of MAN-1
// (ci/lint_codes.py), refused at the request schema. The wire names one place of the oneOf of
// the wrapper schema, often a kind branch that does not apply, so the runner reads where the
// refused manifest breaks the spec schema of its kind from the skeleton schema and keeps every
// place, which the field fact is compared with.
func (x *exec) manifestRefusal(o *outcome, body map[string]any) {
	if o.Reason != "input_schema_invalid" {
		return
	}
	m := manifestPtr.FindStringSubmatch(o.Detail)
	if m == nil {
		return
	}
	n, _ := strconv.Atoi(m[2])
	list := asList(body["manifests"])
	if n >= len(list) {
		return
	}
	o.Error, o.Path = "schema_invalid", m[1]
	mf := asMap(list[n])
	sch := x.r.kindSchemas[str(mf["kind"])]
	if sch == "" {
		return
	}
	sp := asMap(mf["spec"])
	if sp == nil {
		sp = map[string]any{}
	}
	if str(asMap(mf["metadata"])["name"]) == "" {
		o.Missing = append(o.Missing, "metadata.name")
	}
	var ve *spec.ValidationError
	if errors.As(x.r.spec.Validate(sch, sp), &ve) {
		for _, v := range ve.Violations {
			o.Violations = append(o.Violations, violation{Path: "/manifests/" + m[2] + "/spec" + v.Path, Message: v.Message})
			o.Missing = append(o.Missing, missingProps(sp, v)...)
		}
		o.Path = o.Violations[0].Path
	}
}

var listedRe = regexp.MustCompile(`^&\{\[([a-z0-9_ ]+)\]\}$`)

// missingProps are the properties a schema violation lists that the object at its place lacks:
// the required properties of a manifest that does not have them (a property it lists that the
// object has is an unexpected one).
func missingProps(sp map[string]any, v spec.Violation) []string {
	mm := listedRe.FindStringSubmatch(v.Message)
	if mm == nil {
		return nil
	}
	var obj any = sp
	for _, seg := range strings.Split(strings.TrimPrefix(v.Path, "/"), "/") {
		if seg == "" {
			continue
		}
		switch t := obj.(type) {
		case map[string]any:
			obj = t[seg]
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i >= len(t) {
				return nil
			}
			obj = t[i]
		default:
			return nil
		}
	}
	om, ok := obj.(map[string]any)
	if !ok {
		return nil
	}
	prefix := "spec" + strings.ReplaceAll(v.Path, "/", ".")
	var out []string
	for _, p := range strings.Fields(mm[1]) {
		if _, has := om[p]; !has {
			out = append(out, prefix+"."+p)
		}
	}
	return out
}

// manifestIndex is the index of the manifest a refusal points at (/manifests/N/...), or -1.
var manifestIndex = regexp.MustCompile(`^/manifests/([0-9]+)(/|$)`)

// vague is the reason a refusal does not check an expected refusal: the expectation names no
// error code, reason or field, so a refusal for any reason would match; or the errors that
// match it lie outside the objects the row's fragments change.
func vague(e map[string]any, o *outcome) string {
	if o == nil || o.Result == "ok" || str(e["result"]) != "rejected" {
		return ""
	}
	_, hasErr := e["error"]
	_, hasReason := e["reason"]
	_, hasField := e["field"]
	if !hasErr && !hasReason && !hasField {
		return "the expectation names no error code, reason or field, so a refusal for any reason would match (" + o.errors() + ")"
	}
	if o.Touched == nil {
		return ""
	}
	var outside []string
	for _, oo := range append([]*outcome{o}, o.Alts...) {
		if compareOne(withoutKeys(e, "note"), oo) != "" {
			continue
		}
		m := manifestIndex.FindStringSubmatch(oo.Path)
		if m != nil {
			if i, _ := strconv.Atoi(m[1]); oo.Touched[i] {
				return ""
			}
		}
		outside = append(outside, oo.Error+" at "+oo.Path)
	}
	if len(outside) > 0 {
		return "the matching refusal " + strings.Join(outside, ", ") + " lies outside the objects the fragment changes"
	}
	return ""
}

func withoutKeys(e map[string]any, keys ...string) map[string]any {
	out := map[string]any{}
	for k, v := range e {
		if !contains(keys, k) {
			out[k] = v
		}
	}
	return out
}

// incomplete is the reason a refusal does not check an expectation: the refused fixture manifest
// lacks required fields other than the one the expectation names, so the core refused it for
// them. The case is unchecked: the fixture, not the core, is wrong.
func incomplete(e map[string]any, o *outcome) string {
	if n := vague(e, o); n != "" {
		return n
	}
	if o == nil || len(o.Missing) == 0 || o.Result == "ok" {
		return ""
	}
	f := str(e["field"])
	var other []string
	for _, m := range o.Missing {
		if f != "" && (fieldAt("/manifests/0/"+strings.ReplaceAll(m, ".", "/"), "", f)) {
			continue
		}
		other = append(other, m)
	}
	if len(other) == 0 {
		return ""
	}
	return "the given manifest lacks the required " + strings.Join(other, ", ") + ", so its refusal does not check the case"
}

// applyFact is the expectation of the fact apply: apply: ok, or a mapping of expectation fields.
func applyFact(e map[string]any) map[string]any {
	switch v := e["apply"].(type) {
	case string:
		return map[string]any{"result": v}
	case map[string]any:
		return v
	}
	return nil
}

// withoutApply is an expectation without the fact apply.
func withoutApply(e map[string]any) map[string]any {
	if _, ok := e["apply"]; !ok {
		return e
	}
	out := shallowCopy(e)
	delete(out, "apply")
	return out
}

// compare checks the facts the runner reads from the bench (the alarm the monitoring system
// got) and then the outcome.
func (x *exec) compare(e map[string]any, o *outcome) string {
	if a, ok := e["alarm"]; ok {
		// The alarm fact is read from the monitoring system, not from the outcome.
		if d := x.alarmFact(asMap(a)); d != "" {
			return d
		}
		rest := map[string]any{}
		for k, v := range e {
			if k != "alarm" {
				rest[k] = v
			}
		}
		if len(rest) == 0 {
			return ""
		}
		e = rest
	}
	return compare(e, o)
}

// compare checks an outcome against an expectation and returns the first mismatch; a refused
// apply matches when one of its errors does.
func compare(e map[string]any, o *outcome) string {
	d := compareOne(e, o)
	for _, a := range o.Alts {
		if d == "" {
			break
		}
		if compareOne(e, a) == "" {
			d = ""
		}
	}
	return d
}

func compareOne(e map[string]any, o *outcome) string {
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
		if len(o.Violations) > 0 {
			// A manifest refused by its kind schema: every place it breaks the schema is the
			// field, so a fixture manifest broken elsewhere as well does not check the field.
			for _, vi := range o.Violations {
				if !fieldAt(vi.Path, vi.Message, f) {
					return fmt.Sprintf("the manifest breaks its schema at %s (%s) besides the field %s", vi.Path, vi.Message, f)
				}
			}
		} else if !fieldAt(o.Path, "", f) {
			return fmt.Sprintf("field at %q, expected %s%s", o.Path, f, o.why())
		}
	}
	return ""
}

// fieldAt tells whether a refusal at a pointer names a field. A field is written as a name
// (max_tokens) or as a dotted path from the manifest (spec.insight.merge_window); a manifest
// field without spec. or metadata. is a spec field. A schema names a missing or an unexpected
// property at the object that holds it, with the property in its message.
func fieldAt(path, msg, f string) bool {
	fp := "/" + strings.ReplaceAll(f, ".", "/")
	if strings.HasPrefix(path, "/manifests/") && !strings.HasPrefix(f, "spec.") && !strings.HasPrefix(f, "metadata.") {
		fp = "/spec" + fp
	}
	if strings.HasSuffix(path, fp) || strings.Contains(path, fp+"/") || (!strings.HasPrefix(path, "/manifests/") && strings.HasSuffix(path, "/"+f)) {
		return true
	}
	i := strings.LastIndex(fp, "/")
	parent, last := fp[:i], fp[i+1:]
	return msg != "" && strings.HasSuffix(path, parent) && regexp.MustCompile(`\b`+regexp.QuoteMeta(last)+`\b`).MatchString(msg)
}

// stageMatches compares the stage of a refusal. The wire does not tell the manifest lint from
// applying the set: both come back as errors of P2 apply, so either stage matches an apply error.
//
// A manifest that fails the schema of its kind is refused by the manifest's schema and by the
// lint of apply alike (MAN-1, the lint code schema_invalid), so it matches the three stages.
func stageMatches(want string, o *outcome) bool {
	if o.Error == "schema_invalid" && (want == "lint" || want == "apply" || want == "schema") {
		return true
	}
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
	// The seed's other tenants, farm and demo2, as kind: test (seed.yaml, "Initial state").
	more, err := manifest.ReadFiles([]string{filepath.Join(x.r.Root, "examples/tenants/farm/tenant.yaml"), filepath.Join(x.r.Root, "examples/tenants/demo2/tenant.yaml")})
	if err != nil {
		return err
	}
	for _, d := range more {
		asMap(d.Value["spec"])["kind"] = "test"
		ms = append(ms, d.Value)
	}
	if _, err := testbench.DropUnsupported(x.r.spec, ms); err != nil {
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
	ms = append(ms, withTypes(x.r.seedTypes, x.types)...)
	x.base = ms
	o, err := x.apply(ctx, ms, false)
	if err != nil {
		return err
	}
	if o.Result != "ok" {
		if len(x.types) > 0 {
			return errUnchecked(fmt.Sprintf("given type %s: the seed does not apply it (%s %s)", metaNames(x.types), o.Error, o.Path))
		}
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
		x.identities["human:"+p.Name] = p.Identity
		if err := x.addPerson(ctx, p.ID, p.Identity); err != nil {
			return err
		}
		for _, g := range p.Groups {
			if listed := x.givenGroups("human:" + p.Name); listed != nil && !listed[g] {
				continue
			}
			o, err := x.seedGrant(ctx, p.ID, p.Identity, g)
			if err != nil {
				return err
			}
			if o.Result != "ok" {
				return fmt.Errorf("grant %s %s: %s%s", p.Name, g, o.Reason, o.why())
			}
			x.granted["human:"+p.Name] = append(x.granted["human:"+p.Name], g)
		}
	}
	return nil
}

// withTypes adds the task types of more to base, each name once.
func withTypes(base, more []map[string]any) []map[string]any {
	out := append([]map[string]any{}, base...)
	have := map[string]bool{}
	for _, m := range base {
		have[str(asMap(m["metadata"])["name"])] = true
	}
	for _, m := range more {
		if n := str(asMap(m["metadata"])["name"]); !have[n] {
			have[n] = true
			out = append(out, m)
		}
	}
	return out
}

// givenFragments are the lint fragments of given (keys <kind>_yaml) of a case without steps.
func (x *exec) givenFragments() map[string]any {
	out := map[string]any{}
	for k, v := range x.c.Given {
		if strings.HasSuffix(k, "_yaml") {
			out[k] = v
		}
	}
	return out
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
		body := map[string]any{"scope": x.holdScope(asMap(hm["scope"])), "effects": hm["effects"], "reason": reason, "until": hm["until"]}
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

// holdScope completes a hold scope of a fixture for hold.set: a scope of a tenant's object
// without tenant is of the case's tenant, and a tenant scope written with the tenant as its name
// ({kind: tenant, name: demo}) names it in tenant, where the hold schema has it.
func (x *exec) holdScope(sc map[string]any) map[string]any {
	out := shallowCopy(sc)
	switch k := str(out["kind"]); {
	case k == "farm" || k == "seat":
	case k == "tenant" && out["tenant"] == nil && out["name"] != nil:
		out["tenant"] = out["name"]
		delete(out, "name")
	case out["tenant"] == nil:
		out["tenant"] = x.tenant()
	}
	return out
}

// tenant is the case's tenant (given.tenant), demo by default.
func (x *exec) tenant() string {
	if t, ok := x.c.Given["tenant"].(string); ok && t != "" {
		return t
	}
	return "demo"
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
			o, err := x.seedGrant(ctx, id, x.identities[handle], g)
			if err != nil {
				return err
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
		out = append(out, x.onBase(m))
	}
	return out
}

// onBase completes a given manifest of an object the seed already applied (the demo tenant, the
// hello role): the fixture names the fields that matter, and they are merged into the seeded
// object (deepMerge), so the object stays whole.
func (x *exec) onBase(m map[string]any) map[string]any {
	md := asMap(m["metadata"])
	for _, b := range x.base {
		bmd := asMap(b["metadata"])
		if str(b["kind"]) != str(m["kind"]) || str(bmd["name"]) != str(md["name"]) || str(bmd["tenant"]) != str(md["tenant"]) {
			continue
		}
		out := deepCopy(b)
		// The given spec is merged into the seeded one at every depth of maps; a list replaces
		// the seeded list (the fixture writes the whole list it means).
		deepMerge(asMap(out["spec"]), deepCopy(asMap(m["spec"])))
		if str(m["kind"]) == "Tenant" {
			asMap(out["spec"])["kind"] = "test"
		}
		return out
	}
	return m
}

// deepMerge merges src into dst: maps merge key by key at every depth, other values replace.
func deepMerge(dst, src map[string]any) {
	for k, v := range src {
		if sm, ok := v.(map[string]any); ok {
			if dm, ok := dst[k].(map[string]any); ok {
				deepMerge(dm, sm)
				continue
			}
		}
		dst[k] = v
	}
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
	case st["occur"] != nil:
		return x.occur(ctx, st["occur"])
	}
	return nil, fmt.Errorf("no verb")
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
		return x.standMore(ctx, s)
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
		o := problemOutcome(status, resp)
		x.manifestRefusal(o, body)
		return o, nil
	}
	errs := asList(resp["errors"])
	if len(errs) == 0 {
		return &outcome{Result: "ok"}, nil
	}
	var o *outcome
	for _, raw := range errs {
		e := asMap(raw)
		one := &outcome{Result: "rejected", Error: str(e["code"]), Path: str(e["path"]), Stage: "apply", Detail: str(e["message"])}
		if o == nil {
			o = one
		} else {
			o.Alts = append(o.Alts, one)
		}
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

// seedGrant grants a seed group. A privileged group stays pending until the person's
// verification code is checked (PRM-13): the runner plays /me in the person's direct chat through
// its channel, reads the code from the reply and repeats the grant with it, as an operator does
// with the code the person reads out. The membership is never written around the core.
func (x *exec) seedGrant(ctx context.Context, subject, identity, g string) (*outcome, error) {
	req := map[string]any{"op": "grant", "subject": subject, "group": g}
	status, resp, err := x.post(ctx, x.ep.P2+"/v1/p2/grant", x.ep.Operators["demo"], req)
	if err != nil {
		return nil, err
	}
	o := problemOutcome(status, resp)
	if o.Result != "ok" || str(resp["state"]) != "pending" {
		return o, nil
	}
	code, err := x.verificationCode(ctx, identity)
	if err != nil {
		return nil, err
	}
	req["code"] = code
	status, resp, err = x.post(ctx, x.ep.P2+"/v1/p2/grant", x.ep.Operators["demo"], req)
	if err != nil {
		return nil, err
	}
	o = problemOutcome(status, resp)
	if o.Result == "ok" && str(resp["state"]) != "active" {
		return nil, fmt.Errorf("grant %s with the verification code: %s", g, str(resp["state"]))
	}
	return o, nil
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
	return x.postWith(ctx, url, token, nil, body)
}

func (x *exec) postWith(ctx context.Context, url, token string, headers map[string]string, body map[string]any) (int, map[string]any, error) {
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
	for k, v := range headers {
		req.Header.Set(k, v)
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
