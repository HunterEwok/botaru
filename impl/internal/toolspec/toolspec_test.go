// SPDX-License-Identifier: AGPL-3.0-only

package toolspec

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

func readPlugin(t *testing.T, name string) Plugin {
	t.Helper()
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/tools", name, "tool.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	p, err := Decode(docs[0].Name, docs[0].Value["spec"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCmdManifest(t *testing.T) {
	p := readPlugin(t, "cmd")
	if is := Check(p, map[string]bool{"sh": true}); len(is) > 0 {
		t.Fatalf("the example fails the checks: %v", is)
	}
	run, _ := p.Tool("run")
	all := run.AllParams()
	if all["service"]["enum_from"] != "catalog.services" || all["command"] == nil {
		t.Fatalf("params %v", all)
	}
	if req := run.Required(); !slices.Equal(req, []string{"command", "host"}) {
		t.Fatalf("required %v", req)
	}
	if run.Untrusted() != true || RightName("cmd", "run") != "cmd/run" || ModelName("cmd", "run") != "cmd_run" {
		t.Fatal("names or trust")
	}
	// The fingerprint does not depend on the order of tools or of keys.
	a, err := Fingerprint(p.LiveTools())
	if err != nil {
		t.Fatal(err)
	}
	var round []any
	b, _ := json.Marshal(p.LiveTools())
	_ = json.Unmarshal(b, &round)
	if c, _ := Fingerprint(round); c != a {
		t.Fatal("the fingerprint changed over JSON")
	}
	one, _ := CombinedFingerprint(map[string]string{"cmd": a})
	two, _ := CombinedFingerprint(map[string]string{"cmd": a, "logs": "x"})
	if one != a || two == a {
		t.Fatal("combined fingerprint")
	}
}

func TestCheck(t *testing.T) {
	tool := func(argv ...string) Plugin {
		return Plugin{Name: "cmd", Placement: "host", Tools: []Tool{{
			Name: "run", Access: "read", TargetFrom: "host", Redact: []string{"ip", "email", "secret"},
			Params:   map[string]map[string]any{"host": {"enum_from": "catalog.hosts"}},
			Commands: map[string]Command{"c": {Argv: argv, Params: map[string]map[string]any{"unit": {"enum_from": "catalog.services"}}}},
		}}}
	}
	for _, c := range []struct {
		argv []string
		code string
	}{
		{[]string{"/usr/bin/df", "-h"}, ""},
		{[]string{"/usr/bin/systemctl", "status", "--", "{unit}"}, ""},
		{[]string{"df"}, "cmd_argv_path"},
		{[]string{"/bin/sh", "-c", "x"}, "cmd_forbidden_program"},
		{[]string{"/usr/bin/systemctl", "status", "{unit}"}, "cmd_param_position"},
		{[]string{"/usr/bin/systemctl", "--", "--unit={unit}"}, "cmd_param_position"},
		{[]string{"/usr/bin/systemctl", "--", "{other}"}, "cmd_param_position"},
	} {
		is := Check(tool(c.argv...), map[string]bool{"sh": true})
		if c.code == "" && len(is) > 0 || c.code != "" && (len(is) == 0 || is[0].Code != c.code) {
			t.Errorf("%v: %v, want %q", c.argv, is, c.code)
		}
	}
	p := tool("/usr/bin/df")
	p.Tools[0].Redact = []string{"secret"}
	p.Tools[0].TargetFrom = ""
	is := Check(p, nil)
	if len(is) != 2 || is[0].Code != "tool_target_missing" || is[1].Code != "redact_required" {
		t.Fatalf("issues %v", is)
	}
	f := false
	trusted := Plugin{Name: "checks", Placement: "host", Tools: []Tool{{Name: "latest", Access: "read", Target: "any", UntrustedOutput: &f,
		OutputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"check": map[string]any{"enum_from": "catalog.checks"}, "ok": map[string]any{"type": "integer"}}}}}}
	if is := Check(trusted, nil); len(is) > 0 {
		t.Fatalf("closed trusted output: %v", is)
	}
	if !trusted.Tools[0].TrustedOutput() {
		t.Fatal("closed output declared trusted is not trusted")
	}
	noSchema := trusted.Tools[0]
	noSchema.OutputSchema = nil
	if noSchema.TrustedOutput() {
		t.Fatal("trusted output without a schema")
	}
	trusted.Tools[0].OutputSchema["properties"].(map[string]any)["note"] = map[string]any{"type": "string", "maxLength": 50}
	if is := Check(trusted, nil); len(is) != 2 || is[1].Code != "untrusted_output_false_text" {
		t.Fatalf("free text declared trusted: %v", is)
	}
	// A prose format is free text, not a closed shape.
	delete(trusted.Tools[0].OutputSchema["properties"].(map[string]any), "note")
	trusted.Tools[0].OutputSchema["properties"].(map[string]any)["query"] = map[string]any{"type": "string", "x-botaru-format": "search_query"}
	if is := Check(trusted, nil); len(is) != 2 || is[1].Code != "untrusted_output_false_text" || trusted.Tools[0].TrustedOutput() {
		t.Fatalf("a search_query declared trusted: %v", is)
	}
	if is := Check(readPlugin(t, "checks"), nil); len(is) > 0 {
		t.Fatalf("the checks example: %v", is)
	}
	// Both target modes, and a free string on a host plugin, are refused.
	both := tool("/usr/bin/df")
	both.Tools[0].Target = "any"
	if is := Check(both, nil); len(is) != 1 || is[0].Code != "tool_target_missing" {
		t.Fatalf("both target modes: %v", is)
	}
	// The target parameter takes a catalog list.
	plain := tool("/usr/bin/df")
	plain.Tools[0].Params["host"] = map[string]any{"enum": []any{"web1"}}
	if is := Check(plain, nil); len(is) != 1 || is[0].Code != "tool_target_unknown" {
		t.Fatalf("a target without a catalog list: %v", is)
	}
	free := tool("/usr/bin/df")
	free.Tools[0].Params["path"] = map[string]any{"type": "string", "maxLength": 1000}
	if is := Check(free, nil); len(is) != 1 || is[0].Code != "sandbox_placement" || is[0].Path != "path" {
		t.Fatalf("a free string on a host plugin: %v", is)
	}
	// A parameter name has one schema across the tool and its commands; the same schema twice
	// is fine.
	shared := tool("/usr/bin/systemctl", "status", "--", "{unit}")
	shared.Tools[0].OutputClass = "raw_logs" // the journal is text from outside (INJ-14)
	shared.Tools[0].Commands["d"] = Command{Argv: []string{"/usr/bin/journalctl", "--", "{unit}"},
		Params: map[string]map[string]any{"unit": {"enum_from": "catalog.services"}}}
	if is := Check(shared, nil); len(is) > 0 {
		t.Fatalf("one schema shared by two commands: %v", is)
	}
	shared.Tools[0].Commands["d"].Params["unit"] = map[string]any{"enum": []any{"sshd.service"}}
	if is := Check(shared, nil); len(is) != 1 || is[0].Code != "cmd_param_conflict" || is[0].Path != "d" {
		t.Fatalf("two schemas for one parameter: %v", is)
	}
	// Trusted output without a closed schema is shown as untrusted, so it needs the masks.
	f2 := false
	fallback := tool("/usr/bin/df")
	fallback.Tools[0].UntrustedOutput, fallback.Tools[0].Redact = &f2, nil
	if is := Check(fallback, nil); len(is) != 1 || is[0].Code != "redact_required" {
		t.Fatalf("fallback untrusted without masks: %v", is)
	}
	for a, want := range map[string]bool{"{x}": true, "{service.unit}": true, "{}": false, "{x.}": false, "a{x}": false, "{X}": false} {
		if IsPlaceholder(a) != want {
			t.Errorf("IsPlaceholder(%q)", a)
		}
	}
}

// Every example plugin passes the checks.
func TestExamplesPass(t *testing.T) {
	for _, n := range []string{"batch", "checks", "cmd", "data", "geo", "logs", "monitor", "pipeline", "sandbox", "service", "storage"} {
		if is := Check(readPlugin(t, n), map[string]bool{"sh": true}); len(is) > 0 {
			t.Errorf("%s: %v", n, is)
		}
	}
}

func issueCodes(is []Issue) []string {
	var out []string
	for _, i := range is {
		if !slices.Contains(out, i.Code) {
			out = append(out, i.Code)
		}
	}
	slices.Sort(out)
	return out
}

// INJ-6: the model has no URL or host argument; an address comes from a closed list of the
// manifest or the catalog.
func TestAddressParam(t *testing.T) {
	p := readPlugin(t, "checks")
	p.Tools[0].Params = map[string]map[string]any{"url": {"type": "string", "x-botaru-format": "id"}}
	if got := issueCodes(Check(p, nil)); !slices.Contains(got, "tool_address_param") {
		t.Fatalf("a url parameter: %v", got)
	}
	p.Tools[0].Params = map[string]map[string]any{"target_host": {"type": "string", "x-botaru-format": "id"}}
	if got := issueCodes(Check(p, nil)); !slices.Contains(got, "tool_address_param") {
		t.Fatalf("a host parameter: %v", got)
	}
	// A catalog list or an enum is the catalog's address, not the model's.
	if !slices.Equal(issueCodes(Check(readPlugin(t, "cmd"), nil)), nil) || AddressParam("host", map[string]any{"enum_from": "catalog.hosts"}) ||
		AddressParam("url", map[string]any{"enum": []any{"a"}}) || AddressParam("hostage", map[string]any{"type": "string"}) {
		t.Fatal("a closed list or another name counts as an address argument")
	}
	// Plurals, the other address words, array items and object properties count.
	for n, s := range map[string]map[string]any{
		"urls":         {"type": "array", "items": map[string]any{"type": "string"}},
		"addresses":    {"type": "string"},
		"link":         {"type": "string"},
		"target":       {"type": "string"},
		"dest":         {"type": "string", "x-botaru-format": "search_query"},
		"subnet":       {"type": "string", "x-botaru-format": "ipv4_subnet24"},
		"cidrs":        {"type": "array", "items": map[string]any{"type": "string"}},
		"server":       {"anyOf": []any{map[string]any{"enum": []any{"a"}}, map[string]any{"type": "string"}}},
		"opt":          {"type": "object", "properties": map[string]any{"x": map[string]any{"oneOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "object", "properties": map[string]any{"url": map[string]any{"type": "string"}}}}}}},
		"dest_servers": {"type": "array", "items": map[string]any{"type": "string"}},
		"fqdn":         {"type": "string"},
		"domain":       {"type": "string"},
		"opts":         {"type": "object", "properties": map[string]any{"endpoint": map[string]any{"type": "string"}}},
	} {
		if !AddressParam(n, s) {
			t.Errorf("%s is not an address argument", n)
		}
	}
	// A closed list, also in every branch, and target or dest of a closed format are no address.
	for n, s := range map[string]map[string]any{
		"hosts":  {"type": "array", "items": map[string]any{"enum_from": "catalog.hosts"}},
		"host":   {"anyOf": []any{map[string]any{"enum": []any{"a"}}, map[string]any{"enum_from": "catalog.hosts"}}},
		"target": {"type": "string", "x-botaru-format": "id"},
		"dest":   {"type": "string", "x-botaru-format": "type_name"},
	} {
		if AddressParam(n, s) {
			t.Errorf("%s %v counts as an address argument", n, s)
		}
	}
	// On a plugin with egress a free string or a search_query reaches anywhere, whatever its
	// name; a formatted value does not.
	p = readPlugin(t, "checks")
	p.Network.Egress = []string{"http-check-targets"}
	for _, s := range []map[string]any{{"type": "string"}, {"type": "string", "x-botaru-format": "search_query"},
		{"type": "array", "items": map[string]any{"type": "string"}}, {"type": "string", "x-botaru-format": "ipv4_subnet24"},
		{"oneOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "string"}}},
		{"anyOf": []any{map[string]any{"type": "integer"}, map[string]any{"type": "string", "x-botaru-format": "search_query"}}}} {
		p.Tools[0].Params = map[string]map[string]any{"q": s}
		if got := issueCodes(Check(p, nil)); !slices.Contains(got, "tool_address_param") {
			t.Errorf("egress and %v: %v", s, got)
		}
	}
	p.Tools[0].Params = map[string]map[string]any{"q": {"type": "string", "x-botaru-format": "id"}}
	if got := issueCodes(Check(p, nil)); slices.Contains(got, "tool_address_param") {
		t.Errorf("egress and an id: %v", got)
	}
}

// INJ-14: a command that prints the service journal declares untrusted output of class raw_logs.
func TestOutsideTextCommand(t *testing.T) {
	p := readPlugin(t, "cmd")
	run := &p.Tools[0]
	run.Commands["disk_usage"] = Command{Argv: []string{"/usr/bin/journalctl", "-u", "--", "{service.unit}"},
		Params: map[string]map[string]any{"service": {"enum_from": "catalog.services"}}}
	if got := issueCodes(Check(p, nil)); len(got) > 0 {
		t.Fatalf("untrusted raw_logs journal: %v", got)
	}
	f := false
	run.UntrustedOutput = &f
	if got := issueCodes(Check(p, nil)); !slices.Equal(got, []string{"untrusted_output_false_text"}) {
		t.Fatalf("journal with untrusted_output false: %v", got)
	}
	run.UntrustedOutput, run.OutputClass = nil, "internal"
	if got := issueCodes(Check(p, nil)); !slices.Equal(got, []string{"untrusted_output_class"}) {
		t.Fatalf("journal of class internal: %v", got)
	}
}
