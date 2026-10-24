// SPDX-License-Identifier: AGPL-3.0-only

package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/dlclark/regexp2"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// The local check of an answer against the full result schema of the claim payload, before the
// result goes to complete: the core refuses a result that does not match (OUT-1) and the
// attempt is spent, so the runtime first gives the model one turn to fix what it can see.

// maxDetail bounds the validation detail the repair turn quotes.
const maxDetail = 1500

type re2 struct{ re *regexp2.Regexp }

func (r re2) MatchString(s string) bool { ok, err := r.re.MatchString(s); return err == nil && ok }
func (r re2) String() string            { return r.re.String() }

// compileSchema compiles a result schema with the regular expressions of ECMAScript, as the
// core does; nil when it does not compile, and then the runtime does not check locally.
func compileSchema(schema map[string]any) *jsonschema.Schema {
	if schema == nil {
		return nil
	}
	doc, err := toJSONValue(schema)
	if err != nil {
		return nil
	}
	c := jsonschema.NewCompiler()
	c.DefaultDraft(jsonschema.Draft2020)
	c.UseRegexpEngine(func(expr string) (jsonschema.Regexp, error) {
		re, err := regexp2.Compile(expr, regexp2.ECMAScript)
		if err != nil {
			return nil, err
		}
		// A bound against a runaway match of a pattern a manifest or a plugin wrote, as in
		// spec.Load; a match past it fails.
		re.MatchTimeout = time.Second
		return re2{re}, nil
	})
	if err := c.AddResource("urn:botaru:agent:result", doc); err != nil {
		return nil
	}
	s, err := c.Compile("urn:botaru:agent:result")
	if err != nil {
		return nil
	}
	return s
}

// toJSONValue turns a decoded value into the form the validator reads (numbers as json.Number).
func toJSONValue(v any) (any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return jsonschema.UnmarshalJSON(bytes.NewReader(raw))
}

// checkResult validates a result against a compiled schema; it returns "" when the result
// matches, else the paths and reasons of the failures, one per line.
func checkResult(s *jsonschema.Schema, result map[string]any) string {
	if s == nil {
		return ""
	}
	v, err := toJSONValue(result)
	if err != nil {
		return "the answer is not a JSON object"
	}
	err = s.Validate(v)
	if err == nil {
		return ""
	}
	var ve *jsonschema.ValidationError
	if !errors.As(err, &ve) {
		return err.Error()
	}
	var lines []string
	for _, u := range ve.BasicOutput().Errors {
		if u.Error == nil {
			continue
		}
		msg := u.Error.String()
		if msg == "validation failed" {
			continue
		}
		loc := u.InstanceLocation
		if loc == "" {
			loc = "/"
		}
		lines = append(lines, "at "+loc+": "+msg)
	}
	if len(lines) == 0 {
		lines = []string{"the answer does not match the schema"}
	}
	d := strings.Join(lines, "; ")
	if len(d) > maxDetail {
		d = d[:maxDetail] + "..."
	}
	return d
}

// stripNullOptionals removes, in place, the keys whose value is null from the objects of a
// result when the schema of that object does not require them and their own schema does not
// take null, or when the object does not declare the key and takes no other keys: a model
// often writes null for a fact it does not know, and the schema means such a field to be left
// out.
func stripNullOptionals(obj any, schema map[string]any) {
	if schema == nil {
		return
	}
	switch x := obj.(type) {
	case map[string]any:
		props, _ := schema["properties"].(map[string]any)
		req, _ := schema["required"].([]any)
		for k, v := range x {
			ps, _ := props[k].(map[string]any)
			if v == nil {
				// A key the object does not declare and does not take is dropped as well.
				undeclared := ps == nil && schema["additionalProperties"] == false
				if !slices.Contains(req, any(k)) && (undeclared || ps != nil && !allowsNull(ps)) {
					delete(x, k)
				}
				continue
			}
			if ps != nil {
				stripNullOptionals(v, ps)
			}
		}
	case []any:
		items, _ := schema["items"].(map[string]any)
		for _, v := range x {
			stripNullOptionals(v, items)
		}
	}
}

// allowsNull says whether a property schema takes null: by its type, enum or const, or a
// branch of anyOf or oneOf that does, or a schema with none of these.
func allowsNull(s map[string]any) bool {
	// A schema that names no type, values or branches takes null like anything else.
	if _, typed := s["type"]; !typed && s["enum"] == nil && s["const"] == nil && s["anyOf"] == nil && s["oneOf"] == nil && s["$ref"] == nil {
		return true
	}
	switch t := s["type"].(type) {
	case string:
		if t == "null" {
			return true
		}
	case []any:
		if slices.Contains(t, any("null")) {
			return true
		}
	}
	if e, ok := s["enum"].([]any); ok && slices.Contains(e, nil) {
		return true
	}
	if c, ok := s["const"]; ok && c == nil {
		return true
	}
	for _, k := range []string{"anyOf", "oneOf"} {
		for _, b := range asMaps(s[k]) {
			if allowsNull(b) {
				return true
			}
		}
	}
	return false
}

func asMaps(v any) []map[string]any {
	l, _ := v.([]any)
	var out []map[string]any
	for _, e := range l {
		if m, ok := e.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}
