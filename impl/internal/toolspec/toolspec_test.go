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
			Commands: map[string]Command{"c": {Argv: argv, Params: map[string]map[string]any{"unit": {"type": "string"}}}},
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
	for a, want := range map[string]bool{"{x}": true, "{service.unit}": true, "{}": false, "{x.}": false, "a{x}": false, "{X}": false} {
		if IsPlaceholder(a) != want {
			t.Errorf("IsPlaceholder(%q)", a)
		}
	}
}
