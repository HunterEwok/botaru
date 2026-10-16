// SPDX-License-Identifier: AGPL-3.0-only

package spec

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
)

// Profiles are the conformance profiles this implementation claims (MAN-8). A manifest field
// of a feature profile outside this list is refused at apply (MAN-11).
var Profiles = []string{"core"}

// Ahead lists, by manifest kind, the fields of a later skeleton version that this build already
// implements, as JSON pointers into the manifest where "*" stands for any one segment (a list
// index or a map key). apply accepts such a field although the claimed version is earlier; the
// rest of that version stays refused (MAN-11). Kinds of a later version are listed under the
// kind with the pointer "/kind". The list shrinks when the claimed version reaches them.
var Ahead = map[string][]string{
	// File limits (ART-4) and the sink of a person's copy (SEC-10, LCY-2).
	"Tenant": {"/spec/limits/max_file_bytes", "/spec/limits/max_task_files_bytes", "/spec/privacy/export_sink"},
	// The one recipient of an export sink (SEC-10).
	"Sink": {"/spec/export_only/recipient_ref"},
	// The tool output cache (PRM-12).
	"ToolPlugin": {"/spec/tools/*/cache_s"},
	// The model exposure of a dataset (PRV-4).
	"Catalog": {"/spec/resources/*/model_exposure", "/spec/resources/*/kind"},
	// The eval report index of the quality gate (PRV-3).
	"EvalReports": {"/kind"},
}

// ahead says whether the field at ptr of a manifest of kind is in Ahead.
func ahead(kind, ptr string) bool {
	if ptr == "" || ptr == "/spec" {
		// A later kind carries the version mark on the whole document or its spec.
		ptr = "/kind"
	}
	segs := strings.Split(ptr, "/")
	for _, pat := range Ahead[kind] {
		ps := strings.Split(pat, "/")
		if len(ps) != len(segs) {
			continue
		}
		ok := true
		for i := range ps {
			if ps[i] != "*" && ps[i] != segs[i] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// VersionLater reports whether the skeleton version a (v0.2) is later than b (v0.1). A value
// that is not of the form vMAJOR.MINOR counts as later, so an unknown mark is never accepted.
func VersionLater(a, b string) bool {
	am, an, okA := parseVersion(a)
	bm, bn, okB := parseVersion(b)
	if !okA || !okB {
		return true
	}
	return am > bm || am == bm && an > bn
}

func parseVersion(v string) (int, int, bool) {
	major, minor, ok := strings.Cut(strings.TrimPrefix(v, "v"), ".")
	if !ok {
		return 0, 0, false
	}
	minor, _, _ = strings.Cut(minor, ".")
	a, err1 := strconv.Atoi(major)
	b, err2 := strconv.Atoi(minor)
	return a, b, err1 == nil && err2 == nil
}

// LaterFields walks a value along a schema (by short name or $id) and returns the JSON
// pointers of the fields that the value holds and that the schema marks with an
// x-botaru-version later than version (MAN-11). The walk follows $ref, properties,
// patternProperties, additionalProperties, items and prefixItems, and the branches of allOf,
// anyOf, oneOf, if, then and else; a branch whose const or enum of a present field does not
// fit the value is skipped, so a oneOf by kind looks only at the branch of the value's kind.
func (s *Spec) LaterFields(name string, v any, version string) []string {
	why := s.laterFields(name, v, version)
	out := make([]string, 0, len(why))
	for p := range why {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// laterFields is LaterFields with, for each pointer, why the value there cannot simply be
// left out: "" for a whole optional field, else the reason.
func (s *Spec) laterFields(name string, v any, version string) map[string]string {
	id := name
	if x, ok := s.ids[name]; ok {
		id = x
	}
	root, ok := s.raw[id]
	if !ok {
		return nil
	}
	w := &walker{s: s, version: version, why: map[string]string{}}
	if m, ok := v.(map[string]any); ok && name == "manifest:manifest" {
		w.kind, _ = m["kind"].(string)
	}
	w.walk(root, id, v, "", 0, "it is the whole manifest")
	return w.why
}

type walker struct {
	s       *Spec
	version string
	why     map[string]string
	// kind is the manifest kind whose Ahead fields the walk accepts; empty outside manifests.
	kind string
}

func (w *walker) add(ptr, why string) {
	if _, seen := w.why[ptr]; !seen {
		w.why[ptr] = why
	}
}

// Reasons why a later value cannot be left out of a manifest.
const (
	whyRequired = "it is a required field"
	whyElement  = "it is an element of a list"
	whyVariant  = "it is a value or variant that gives the field its meaning"
)

// resolve returns the schema a $ref names and the $id of its document.
func (w *walker) resolve(ref, doc string) (any, string) {
	base, frag, _ := strings.Cut(ref, "#")
	if base == "" {
		base = doc
	}
	node, ok := w.s.raw[base]
	if !ok {
		return nil, base
	}
	if frag == "" {
		return node, base
	}
	for _, tok := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		m, ok := node.(map[string]any)
		if !ok {
			return nil, base
		}
		node = m[tok]
	}
	return node, base
}

// walk visits a schema node at a position of the value. why says whether the position is a
// whole optional field ("") and, if not, why it cannot be left out.
func (w *walker) walk(node any, doc string, v any, ptr string, depth int, why string) {
	m, ok := node.(map[string]any)
	if !ok || depth > 64 {
		return
	}
	if xv, ok := m["x-botaru-version"].(string); ok && VersionLater(xv, w.version) && !(w.kind != "" && ahead(w.kind, ptr)) {
		if why == "" && w.meaningful(m, v) {
			why = whyVariant
		}
		w.add(ptr, why)
		return
	}
	if ref, ok := m["$ref"].(string); ok {
		if target, tdoc := w.resolve(ref, doc); target != nil {
			w.walk(target, tdoc, v, ptr, depth+1, why)
		}
	}
	for _, k := range []string{"allOf", "anyOf", "oneOf"} {
		branchWhy := why
		if k != "allOf" {
			branchWhy = whyVariant
		}
		list, _ := m[k].([]any)
		for _, b := range list {
			if w.fits(b, doc, v, depth) {
				w.walk(b, doc, v, ptr, depth+1, branchWhy)
			}
		}
	}
	for _, k := range []string{"then", "else"} {
		if b, ok := m[k]; ok {
			w.walk(b, doc, v, ptr, depth+1, whyVariant)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		props, _ := m["properties"].(map[string]any)
		pats, _ := m["patternProperties"].(map[string]any)
		req, _ := m["required"].([]any)
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			child := ptr + "/" + escape(k)
			childWhy := ""
			if slices.Contains(req, any(k)) {
				childWhy = whyRequired
			}
			if p, ok := props[k]; ok {
				w.walk(p, doc, x[k], child, depth+1, childWhy)
				continue
			}
			matched := false
			for pat, p := range pats {
				if re, err := regexp.Compile(pat); err == nil && re.MatchString(k) {
					matched = true
					w.walk(p, doc, x[k], child, depth+1, childWhy)
				}
			}
			if ap, ok := m["additionalProperties"].(map[string]any); ok && !matched {
				w.walk(ap, doc, x[k], child, depth+1, childWhy)
			}
		}
	case []any:
		prefix, _ := m["prefixItems"].([]any)
		for i, e := range x {
			child := ptr + "/" + strconv.Itoa(i)
			if i < len(prefix) {
				w.walk(prefix[i], doc, e, child, depth+1, whyElement)
			} else if it, ok := m["items"]; ok {
				w.walk(it, doc, e, child, depth+1, whyElement)
			}
		}
	}
}

// meaningful says whether a later field holds a value chosen from a closed list of its own
// (enum or const on the field) other than the schema default: leaving it out would change what
// the manifest says, as with a channel direction. A field that only refers to a shared value
// type, such as a locale, picks a presentation and stays a plain optional field.
func (w *walker) meaningful(m map[string]any, v any) bool {
	_, hasEnum := m["enum"]
	_, hasConst := m["const"]
	if !hasEnum && !hasConst {
		return false
	}
	def, hasDef := m["default"]
	return !hasDef || !sameScalar(def, v)
}

// fits says whether a branch can hold the value: no present field contradicts a const or an
// enum of the branch, and no required field with a const is missing.
func (w *walker) fits(branch any, doc string, v any, depth int) bool {
	m, ok := branch.(map[string]any)
	for i := 0; ok && depth+i < 64; i++ {
		ref, has := m["$ref"].(string)
		if !has {
			break
		}
		t, _ := w.resolve(ref, doc)
		m, ok = t.(map[string]any)
	}
	if !ok {
		return true
	}
	x, isObj := v.(map[string]any)
	if !isObj {
		return true
	}
	props, _ := m["properties"].(map[string]any)
	req, _ := m["required"].([]any)
	for k, p := range props {
		pm, _ := p.(map[string]any)
		val, present := x[k]
		if c, has := pm["const"]; has {
			if !present {
				for _, r := range req {
					if r == k {
						return false
					}
				}
				continue
			}
			if !sameScalar(c, val) {
				return false
			}
		}
		if e, has := pm["enum"].([]any); has && present {
			found := false
			for _, c := range e {
				if sameScalar(c, val) {
					found = true
				}
			}
			if !found {
				return false
			}
		}
	}
	return true
}

func sameScalar(a, b any) bool {
	switch a.(type) {
	case map[string]any, []any:
		return false
	}
	switch b.(type) {
	case map[string]any, []any:
		return false
	}
	return numText(a) == numText(b)
}

func numText(v any) string {
	switch x := v.(type) {
	case string:
		return "s:" + x
	case nil:
		return "null"
	case bool:
		return strconv.FormatBool(x)
	default:
		f, err := strconv.ParseFloat(strings.TrimSpace(toString(x)), 64)
		if err != nil {
			return toString(x)
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
}

func toString(v any) string {
	switch x := v.(type) {
	case interface{ String() string }:
		return x.String()
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'g', -1, 64)
	}
	return ""
}

// profileField is a manifest value of a feature profile or a later version that the schemas do
// not mark with x-botaru-version: the profile (or version) it needs, and whether it is a value
// that gives its field the meaning (an enum value such as a resource kind or a channel
// direction), which must never be left out, rather than a whole optional field.
type profileField struct {
	need  string
	value bool
}

// profileFields are the manifest values of the feature profiles of MAN-11 that the schemas do
// not mark with x-botaru-version.
func profileFields(kind string, v map[string]any) map[string]profileField {
	out := map[string]profileField{}
	sp, _ := v["spec"].(map[string]any)
	if sp == nil {
		return out
	}
	tier2 := func(x any) bool { n, ok := x.(interface{ String() string }); return ok && n.String() == "2" }
	switch kind {
	case "Catalog":
		res, _ := sp["resources"].([]any)
		for i, r := range res {
			rm, _ := r.(map[string]any)
			switch rm["kind"] {
			case "corpus":
				out["/spec/resources/"+strconv.Itoa(i)+"/kind"] = profileField{need: "knowledge", value: true}
			case "dataset":
				// A dataset resource is of v0.2 too (catalog schema, resources.kind).
				out["/spec/resources/"+strconv.Itoa(i)+"/kind"] = profileField{need: "v0.2", value: true}
			}
		}
	case "Role":
		if _, ok := sp["skills"]; ok {
			out["/spec/skills"] = profileField{need: "knowledge", value: false}
		}
	case "Channel":
		if sp["direction"] == "in" {
			out["/spec/direction"] = profileField{need: "ingress", value: true}
		}
		if _, ok := sp["ingress"]; ok {
			out["/spec/ingress"] = profileField{need: "ingress", value: false}
		}
	case "ToolPlugin":
		if sp["placement"] == "sandbox" {
			out["/spec/placement"] = profileField{need: "sandbox", value: true}
		}
		tools, _ := sp["tools"].([]any)
		for i, t := range tools {
			tm, _ := t.(map[string]any)
			if tier2(tm["tier"]) {
				out["/spec/tools/"+strconv.Itoa(i)+"/tier"] = profileField{need: "sandbox", value: true}
			}
			if cr, ok := tm["credential"].(map[string]any); ok {
				if _, ok := cr["per_principal"]; ok {
					out["/spec/tools/"+strconv.Itoa(i)+"/credential"] = profileField{need: "connections", value: true}
				}
			}
		}
	case "TaskType":
		if tier2(sp["tier"]) {
			out["/spec/tier"] = profileField{need: "sandbox", value: true}
		}
	case "Tenant":
		if r, ok := sp["retention"].(map[string]any); ok {
			if _, ok := r["journals"]; ok {
				out["/spec/retention/journals"] = profileField{need: "debug", value: false}
			}
		}
		if oa, ok := sp["operator_access"].(map[string]any); ok {
			if _, ok := oa["debug_journal"]; ok {
				out["/spec/operator_access/debug_journal"] = profileField{need: "debug", value: false}
			}
		}
	}
	return out
}

// Unsupported returns the JSON pointers in a manifest that a core of this implementation
// refuses with version_unsupported (MAN-11): the whole kind ("/kind") when its since in
// spec/manifests/kinds.yaml is later than the claimed skeleton version, the fields the schemas
// mark with a later x-botaru-version, and the fields of a feature profile the core does not
// claim. A field inside a listed field is not listed again.
func (s *Spec) Unsupported(v map[string]any) ([]string, error) {
	why, err := s.unsupported(v)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(why))
	for p := range why {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, nil
}

// Droppable says whether a pointer that Unsupported lists can be left out of the manifest
// without changing the meaning of what stays: only a whole optional field can. It returns nil
// then, and otherwise an error that says why. A value such as a catalog resource kind, a
// channel direction or a tier is never dropped: the manifest would then say something else.
func (s *Spec) Droppable(v map[string]any, ptr string) error {
	why, err := s.unsupported(v)
	if err != nil {
		return err
	}
	reason, listed := why[ptr]
	switch {
	case !listed:
		return fmt.Errorf("%s is not a field this skeleton version refuses, so it is not left out", ptr)
	case reason != "":
		return fmt.Errorf("%s cannot be left out: %s", ptr, reason)
	}
	return nil
}

func (s *Spec) unsupported(v map[string]any) (map[string]string, error) {
	n, err := ijson.Normalize(v)
	if err != nil {
		return nil, err
	}
	nv, _ := n.(map[string]any)
	kind, _ := nv["kind"].(string)
	reg, err := s.Registry("spec/manifests/kinds.yaml")
	if err != nil {
		return nil, err
	}
	if e, ok := reg.Entry(kind); ok {
		if since, _ := e["since"].(string); since != "" && VersionLater(since, Version) && !ahead(kind, "/kind") {
			return map[string]string{"/kind": "the kind is of a later version; the whole manifest is refused"}, nil
		}
	}
	out := s.laterFields("manifest:manifest", nv, Version)
	if out == nil {
		out = map[string]string{}
	}
	for p, f := range profileFields(kind, nv) {
		if f.need == "v0.2" && (!VersionLater(f.need, Version) || ahead(kind, p)) || f.need != "v0.2" && slices.Contains(Profiles, f.need) {
			continue
		}
		covered := false
		for q := range out {
			if q == p || strings.HasPrefix(p, q+"/") {
				covered = true
			}
		}
		if covered {
			if f.value && out[p] == "" {
				out[p] = whyVariant
			}
			continue
		}
		out[p] = ""
		if f.value {
			out[p] = whyVariant
		}
	}
	return out, nil
}

func escape(k string) string {
	return strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1")
}
