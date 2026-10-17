// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The manifest set a lint reads, its findings and the accessors over decoded YAML.

// lintObj is one object of the set with the index of its manifest.
type lintObj struct {
	i                  int
	kind, tenant, name string
	spec               map[string]any
}

// lintSet indexes the objects of a set by kind.
type lintSet struct {
	byKind map[string][]lintObj
}

func newLintSet(docs []manifest.Doc, index []int) *lintSet {
	s := &lintSet{byKind: map[string][]lintObj{}}
	for j, d := range docs {
		spec, _ := d.Value["spec"].(map[string]any)
		if spec == nil {
			spec = map[string]any{}
		}
		tenant := d.Tenant
		if d.Kind == "Tenant" {
			tenant = d.Name
		}
		s.byKind[d.Kind] = append(s.byKind[d.Kind], lintObj{i: index[j], kind: d.Kind, tenant: tenant, name: d.Name, spec: spec})
	}
	return s
}

func (s *lintSet) each(kind string) []lintObj { return s.byKind[kind] }

// get finds an object of a kind by name, in a tenant or global.
func (s *lintSet) get(kind, name, tenant string) map[string]any {
	for _, o := range s.byKind[kind] {
		if o.name == name && (tenant == "" || o.tenant == tenant || o.tenant == "") {
			return o.spec
		}
	}
	return nil
}

// lintFindings collects the errors of the set lint, one per code and object.
type lintFindings struct {
	out  []ApplyError
	seen map[string]bool
}

// refuses says whether the findings already hold an error of the object.
func (f *lintFindings) refuses(o lintObj) bool {
	for _, e := range f.out {
		if e.Kind == o.kind && e.Name == o.name && (o.kind == "Tenant" || e.Tenant == o.tenant) {
			return true
		}
	}
	return false
}

// addDetail is add with a detail text on the error.
func (f *lintFindings) addDetail(o lintObj, code, path, detail string) {
	n := len(f.out)
	f.add(o, code, path)
	if len(f.out) > n {
		f.out[len(f.out)-1].Detail = detail
	}
}

func (f *lintFindings) add(o lintObj, code, path string) {
	k := fmt.Sprintf("%d/%s/%s", o.i, code, path)
	if o.i < 0 {
		k = fmt.Sprintf("store/%s/%s/%s/%s/%s", o.kind, o.tenant, o.name, code, path)
	}
	if f.seen == nil {
		f.seen = map[string]bool{}
	}
	if f.seen[k] {
		return
	}
	f.seen[k] = true
	e := ApplyError{Kind: o.kind, Name: o.name, Code: code, Path: fmt.Sprintf("/manifests/%d/spec%s", o.i, path)}
	if o.i < 0 {
		// An object the store keeps from an earlier apply: the set does not hold it.
		e.Path = "/store/spec" + path
	}
	if o.kind != "Tenant" {
		e.Tenant = o.tenant
	}
	f.out = append(f.out, e)
}

// Accessors over decoded YAML.

func mstr(m map[string]any, k string) string { s, _ := m[k].(string); return s }
func mmap(m map[string]any, k string) map[string]any {
	v, _ := m[k].(map[string]any)
	if v == nil {
		return map[string]any{}
	}
	return v
}
func mlist(m map[string]any, k string) []any { v, _ := m[k].([]any); return v }
func mstrs(m map[string]any, k string) []string {
	var out []string
	for _, v := range mlist(m, k) {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func mnum(m map[string]any, k string) (float64, bool) {
	switch v := m[k].(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	}
	return 0, false
}
func truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case []any:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case int:
		return x != 0
	case float64:
		return x != 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	}
	return true
}

// durSeconds reads a duration of the schemas (15m) in seconds, or -1.
func durSeconds(v any) int64 {
	s, _ := v.(string)
	d, ok := parseDuration(s)
	if !ok {
		return -1
	}
	return int64(d.Seconds())
}

// walkNodes calls fn for every object node under v.
func walkNodes(v any, fn func(map[string]any)) {
	switch x := v.(type) {
	case map[string]any:
		fn(x)
		for _, e := range x {
			walkNodes(e, fn)
		}
	case []any:
		for _, e := range x {
			walkNodes(e, fn)
		}
	}
}

func hasKey(v any, key string) bool {
	found := false
	walkNodes(v, func(m map[string]any) {
		if _, ok := m[key]; ok {
			found = true
		}
	})
	return found
}

// allStrings calls fn for every string of v, the keys of objects included.
func allStrings(v any, fn func(string)) {
	switch x := v.(type) {
	case string:
		fn(x)
	case map[string]any:
		for k, e := range x {
			fn(k)
			allStrings(e, fn)
		}
	case []any:
		for _, e := range x {
			allStrings(e, fn)
		}
	}
}

func sortedKinds(S *lintSet) []string { return slices.Sorted(mapKeys(S.byKind)) }

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// mapSlice applies fn to every element of in.
func mapSlice[T, U any](in []T, fn func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, fn(v))
	}
	return out
}
