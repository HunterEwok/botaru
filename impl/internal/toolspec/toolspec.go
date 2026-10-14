// SPDX-License-Identifier: AGPL-3.0-only

// Package toolspec reads ToolPlugin manifests (tools/<plugin>/tool.yaml) and derives from them
// what the core, the toolhost and the plugins of this implementation must agree on: the
// tool names for rights and for the model, the input schema of a tool, the tools/list
// fingerprint of spec/hashing.md and the checks of program commands (INJ-14).
package toolspec

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"slices"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// Plugin is the spec of a ToolPlugin manifest with its name.
type Plugin struct {
	Name      string `json:"-"`
	Placement string `json:"placement"`
	Network   struct {
		Egress []string `json:"egress"`
	} `json:"network"`
	Secrets []string `json:"secrets"`
	Tools   []Tool   `json:"tools"`
}

// Tool is one tool of a plugin.
type Tool struct {
	Name            string                    `json:"name"`
	Description     string                    `json:"description"`
	Enabled         *bool                     `json:"enabled"`
	Access          string                    `json:"access"`
	Tier            int                       `json:"tier"`
	OutputClass     string                    `json:"output_class"`
	UntrustedOutput *bool                     `json:"untrusted_output"`
	Redact          []string                  `json:"redact"`
	TargetFrom      string                    `json:"target_from"`
	Target          string                    `json:"target"`
	Params          map[string]map[string]any `json:"params"`
	ArgsFrom        string                    `json:"args_from"`
	OutputSchema    map[string]any            `json:"output_schema"`
	OutputLimitKB   int                       `json:"output_limit_kb"`
	TimeoutS        int                       `json:"timeout_s"`
	Commands        map[string]Command        `json:"commands"`
	Billing         string                    `json:"billing"`
	PriceEUR        float64                   `json:"price_eur"`
}

// Command is a named command of the program launch plugin (INJ-14).
type Command struct {
	Argv   []string                  `json:"argv"`
	Params map[string]map[string]any `json:"params"`
}

// Decode reads the spec of a ToolPlugin manifest.
func Decode(name string, spec map[string]any) (Plugin, error) {
	var p Plugin
	b, err := json.Marshal(spec)
	if err != nil {
		return p, err
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("ToolPlugin %s: %w", name, err)
	}
	p.Name = name
	return p, nil
}

// On says whether the tool is turned on (enabled: false turns it off, INJ-1).
func (t Tool) On() bool { return t.Enabled == nil || *t.Enabled }

// Untrusted says whether the output of the tool is untrusted text (INJ-13); a manifest without
// the field counts as untrusted.
func (t Tool) Untrusted() bool { return t.UntrustedOutput == nil || *t.UntrustedOutput }

// TrustedOutput says whether the output of a tool reaches the model without the untrusted mark:
// the tool declares untrusted_output: false and has an output schema without free text, so
// only its structured output, checked against that schema, is shown (INJ-3). A tool that
// declares trusted output without such a schema is treated as untrusted.
func (t Tool) TrustedOutput() bool {
	return !t.Untrusted() && t.OutputSchema != nil && !freeText(t.OutputSchema)
}

// Tool returns a tool of the plugin by name.
func (p Plugin) Tool(name string) (Tool, bool) {
	for _, t := range p.Tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// RightName is the name of a tool in rights and roles: <plugin>/<tool>.
func RightName(plugin, tool string) string { return plugin + "/" + tool }

// ModelName is the name of a tool for the model: <plugin>_<tool>.
func ModelName(plugin, tool string) string { return plugin + "_" + tool }

// AllParams returns the parameters of a tool: its own, then those of its commands, and for a
// tool with commands but no command parameter, the parameter command with the command names.
func (t Tool) AllParams() map[string]map[string]any {
	out := map[string]map[string]any{}
	for k, v := range t.Params {
		out[k] = v
	}
	if len(t.Commands) > 0 {
		if _, ok := out["command"]; !ok {
			names := make([]any, 0, len(t.Commands))
			for _, n := range sortedKeys(t.Commands) {
				names = append(names, n)
			}
			out["command"] = map[string]any{"enum": names}
		}
		for _, n := range sortedKeys(t.Commands) {
			for k, v := range t.Commands[n].Params {
				if _, ok := out[k]; !ok {
					out[k] = v
				}
			}
		}
	}
	return out
}

// Required lists the parameters a call must give: the tool's own parameters without a
// default; a parameter of a command is required only by its command, which the plugin checks.
func (t Tool) Required() []string {
	var req []string
	for k, v := range t.Params {
		if _, ok := v["default"]; !ok {
			req = append(req, k)
		}
	}
	if len(t.Commands) > 0 {
		if _, ok := t.Params["command"]; !ok {
			req = append(req, "command")
		}
	}
	sort.Strings(req)
	return req
}

// InputSchema is the input schema of a tool in the manifest form: the parameters as declared,
// enum_from included, in a closed object. The core turns enum_from into an enum of the tenant's
// catalog for the model; the plugin's live tools/list carries this form.
func (t Tool) InputSchema() map[string]any {
	props := map[string]any{}
	for k, v := range t.AllParams() {
		props[k] = clone(v)
	}
	s := map[string]any{"type": "object", "additionalProperties": false, "properties": props}
	if req := t.Required(); len(req) > 0 {
		r := make([]any, len(req))
		for i, x := range req {
			r[i] = x
		}
		s["required"] = r
	}
	return s
}

// LiveTools is the tools/list answer that a plugin of this implementation gives for its
// manifest: the turned-on tools in name order, each with name, description, input schema and
// the read-only annotation from access (INJ-3).
func (p Plugin) LiveTools() []any {
	var out []any
	names := make([]string, 0, len(p.Tools))
	for _, t := range p.Tools {
		if t.On() {
			names = append(names, t.Name)
		}
	}
	sort.Strings(names)
	for _, n := range names {
		t, _ := p.Tool(n)
		e := map[string]any{"name": t.Name, "inputSchema": t.InputSchema(),
			"annotations": map[string]any{"readOnlyHint": t.Access != "write"}}
		if t.Description != "" {
			e["description"] = t.Description
		}
		out = append(out, e)
	}
	return out
}

// Fingerprint is the tools_fingerprint of spec/hashing.md: the array of tools in ascending
// order of name, each with only name, description, inputSchema and annotations.
func Fingerprint(tools []any) (string, error) {
	type named struct {
		name string
		v    map[string]any
	}
	var list []named
	for _, x := range tools {
		m, ok := x.(map[string]any)
		if !ok {
			return "", fmt.Errorf("a tool is not an object")
		}
		e := map[string]any{}
		for _, k := range []string{"name", "description", "inputSchema", "annotations"} {
			if v, ok := m[k]; ok {
				e[k] = v
			}
		}
		n, _ := m["name"].(string)
		list = append(list, named{n, e})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name < list[j].name })
	arr := make([]any, len(list))
	for i, x := range list {
		arr[i] = x.v
	}
	return jcs.Hash(normalize(arr))
}

// CombinedFingerprint is the fingerprint a toolhost reports in hello: P1 hello carries one
// tools_fingerprint, so a toolhost with several plugins reports the hash of the array of the
// plugin fingerprints in ascending order of the plugin name; with one plugin it is that
// plugin's fingerprint.
func CombinedFingerprint(byPlugin map[string]string) (string, error) {
	names := sortedKeys(byPlugin)
	if len(names) == 1 {
		return byPlugin[names[0]], nil
	}
	arr := make([]any, len(names))
	for i, n := range names {
		arr[i] = byPlugin[n]
	}
	return jcs.Hash(arr)
}

// Issue is a check failure of a plugin manifest with a lint code.
type Issue struct {
	Code string
	Tool string
	Path string
}

func (i Issue) String() string { return i.Code + " " + i.Tool + " " + i.Path }

// Check runs the checks of a plugin manifest that the schemas cannot express: a call target
// (TEN-3a), the redaction of untrusted output on a machine (INJ-4) and the program commands
// (INJ-14). forbidden is the set of program base names of spec/model/forbidden-programs.yaml.
func Check(p Plugin, forbidden map[string]bool) []Issue {
	var out []Issue
	for _, t := range p.Tools {
		if !t.On() {
			continue
		}
		switch {
		case (t.TargetFrom == "") == (t.Target != "any"):
			// Exactly one of target_from and target: any (TEN-3a).
			out = append(out, Issue{Code: "tool_target_missing", Tool: t.Name})
		case t.TargetFrom != "" && t.ArgsFrom == "":
			// With args_from the parameters are the action's, checked with the action.
			// The target is a machine: the parameter takes a catalog list the core maps to one.
			if s, ok := t.AllParams()[t.TargetFrom]; !ok || !strings.HasPrefix(asString(s["enum_from"]), "catalog.") {
				out = append(out, Issue{Code: "tool_target_unknown", Tool: t.Name, Path: t.TargetFrom})
			}
		}
		// A parameter that is a free string is only for a sandbox plugin (INJ-1, SBX-1).
		if p.Placement != "sandbox" {
			for _, n := range sortedKeys(t.AllParams()) {
				if freeParam(t.AllParams()[n]) {
					out = append(out, Issue{Code: "sandbox_placement", Tool: t.Name, Path: n})
				}
			}
		}
		// Output shown as untrusted, also when trusted output lacks a closed schema, is masked
		// on the machine (INJ-4).
		if p.Placement == "host" && !t.TrustedOutput() && t.Access != "write" {
			for _, k := range []string{"ip", "email", "secret"} {
				if !slices.Contains(t.Redact, k) {
					out = append(out, Issue{Code: "redact_required", Tool: t.Name, Path: k})
					break
				}
			}
		}
		// Output declared trusted holds only numbers, booleans and closed values (INJ-3).
		if !t.Untrusted() && t.OutputSchema != nil && freeText(t.OutputSchema) {
			out = append(out, Issue{Code: "untrusted_output_false_text", Tool: t.Name})
		}
		// The model sees one schema per parameter name, so the tool and all its commands must
		// agree on it; otherwise a value allowed for one command reaches another.
		seen := map[string]map[string]any{}
		for k, v := range t.Params {
			seen[k] = v
		}
		for _, n := range sortedKeys(t.Commands) {
			for _, k := range sortedKeys(t.Commands[n].Params) {
				v := t.Commands[n].Params[k]
				if prev, ok := seen[k]; ok && !reflect.DeepEqual(prev, v) {
					out = append(out, Issue{Code: "cmd_param_conflict", Tool: t.Name, Path: n})
				} else if !ok {
					seen[k] = v
				}
			}
			out = append(out, checkCommand(t, n, t.Commands[n], forbidden)...)
		}
	}
	return out
}

func checkCommand(t Tool, name string, c Command, forbidden map[string]bool) []Issue {
	var out []Issue
	if len(c.Argv) == 0 {
		return []Issue{{Code: "cmd_argv_path", Tool: t.Name, Path: name}}
	}
	prog := c.Argv[0]
	if !path.IsAbs(prog) || strings.Contains(prog, "{") {
		out = append(out, Issue{Code: "cmd_argv_path", Tool: t.Name, Path: name})
	}
	if forbidden[path.Base(prog)] {
		out = append(out, Issue{Code: "cmd_forbidden_program", Tool: t.Name, Path: name})
	}
	sep := slices.Index(c.Argv, "--")
	for i, a := range c.Argv[1:] {
		if !strings.Contains(a, "{") {
			continue
		}
		// A parameter is a whole element after the -- separator.
		if !IsPlaceholder(a) || sep < 0 || i+1 < sep {
			out = append(out, Issue{Code: "cmd_param_position", Tool: t.Name, Path: name})
			continue
		}
		param, _, _ := strings.Cut(strings.Trim(a, "{}"), ".")
		if _, ok := c.Params[param]; !ok {
			if _, ok := t.Params[param]; !ok {
				out = append(out, Issue{Code: "cmd_param_position", Tool: t.Name, Path: name})
			}
		}
	}
	return out
}

// freeParam says whether a parameter schema is a string without enum, enum_from, const or
// format: the sandbox_param form of the manifest schema.
func freeParam(s map[string]any) bool {
	if s["type"] != "string" {
		return false
	}
	for _, k := range []string{"enum", "enum_from", "const", "x-botaru-format"} {
		if _, ok := s[k]; ok {
			return false
		}
	}
	return true
}

// proseFormats are the formats of spec/model/formats.yaml whose values are prose, not a
// closed shape: a string of such a format is free text (INJ-3).
var proseFormats = map[string]bool{"search_query": true}

// freeText says whether a schema has a string without enum, enum_from, const or a non-prose
// format anywhere in it, as ci/check_manifests.py does; a string of any length counts.
func freeText(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if x["type"] == "string" || isStringType(x["type"]) {
			closed := false
			for _, k := range []string{"enum", "enum_from", "const"} {
				if _, ok := x[k]; ok {
					closed = true
				}
			}
			if f, ok := x["x-botaru-format"].(string); ok && !proseFormats[f] {
				closed = true
			}
			if !closed {
				return true
			}
		}
		for _, e := range x {
			if freeText(e) {
				return true
			}
		}
	case []any:
		for _, e := range x {
			if freeText(e) {
				return true
			}
		}
	}
	return false
}

func isStringType(t any) bool {
	l, _ := t.([]any)
	for _, e := range l {
		if e == "string" {
			return true
		}
	}
	return false
}

// IsPlaceholder says whether an argv element is a whole parameter reference: {param} or
// {param.field}.
func IsPlaceholder(a string) bool {
	if len(a) < 3 || a[0] != '{' || a[len(a)-1] != '}' {
		return false
	}
	inner := a[1 : len(a)-1]
	for _, r := range inner {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '.') {
			return false
		}
	}
	return !strings.HasPrefix(inner, ".") && !strings.HasSuffix(inner, ".")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func clone(v map[string]any) map[string]any {
	b, _ := json.Marshal(v)
	var out map[string]any
	_ = json.Unmarshal(b, &out)
	return out
}

// normalize turns a value into plain JSON values (numbers as float64 or json.Number from a
// round trip), so that a schema decoded from YAML and one decoded from JSON hash the same.
func normalize(v any) any {
	b, err := json.Marshal(v)
	if err != nil {
		return v
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return v
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// FreeText says whether a schema has free text anywhere in it (INJ-3); see freeText.
func FreeText(v any) bool { return freeText(v) }
