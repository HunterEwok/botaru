// SPDX-License-Identifier: AGPL-3.0-only

package plugins

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

func cmdPlugin(argv ...string) toolspec.Plugin {
	return toolspec.Plugin{Name: "cmd", Placement: "host", Tools: []toolspec.Tool{{
		Name: "run", Access: "read", TargetFrom: "host", Redact: []string{"ip", "email", "secret"}, TimeoutS: 2,
		Params: map[string]map[string]any{"host": {"enum_from": "catalog.hosts"}},
		Commands: map[string]toolspec.Command{
			"say":  {Argv: argv, Params: map[string]map[string]any{"service": {"enum_from": "catalog.services"}}},
			"wait": {Argv: []string{"/bin/sleep", "5"}},
		},
	}}}
}

func TestCmd(t *testing.T) {
	c, err := NewCmd(cmdPlugin("/bin/echo", "unit", "--", "{service.unit}"), "web1", map[string]bool{"sh": true})
	if err != nil {
		t.Fatal(err)
	}
	meta := &mcp.Meta{Resolved: map[string]string{"service.unit": "nginx.service"}}
	r, err := c.Call(context.Background(), "run", map[string]any{"host": "web1", "command": "say", "service": "nginx"}, meta)
	if err != nil || r.Content[0].Text != "unit -- nginx.service\n" {
		t.Fatalf("say: %+v %v", r, err)
	}
	if _, err := c.Call(context.Background(), "run", map[string]any{"host": "db1", "command": "say"}, meta); err == nil {
		t.Fatal("a call for another machine ran")
	}
	for _, bad := range []string{"-rf", "", "a\nb"} {
		m := &mcp.Meta{Resolved: map[string]string{"service.unit": bad}}
		if _, err := c.Call(context.Background(), "run", map[string]any{"host": "web1", "command": "say"}, m); err == nil {
			t.Fatalf("value %q became an argument", bad)
		}
	}
	start := time.Now()
	if _, err := c.Call(context.Background(), "run", map[string]any{"host": "web1", "command": "wait"}, nil); err == nil || time.Since(start) > 4*time.Second {
		t.Fatalf("timeout: %v after %v", err, time.Since(start))
	}
	if _, err := NewCmd(cmdPlugin("/bin/sh", "-c", "id"), "web1", map[string]bool{"sh": true}); err == nil {
		t.Fatal("a forbidden program passed")
	}
	// A symlink to a forbidden program is refused at start and at the call.
	shReal, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	forbidden := map[string]bool{"sh": true, filepath.Base(shReal): true}
	link := filepath.Join(t.TempDir(), "status")
	if err := os.Symlink("/bin/sh", link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCmd(cmdPlugin(link, "-c", "id"), "", forbidden); err == nil {
		t.Fatal("a symlink to a forbidden program passed")
	}
	_ = os.Remove(link)
	if err := os.Symlink("/bin/echo", link); err != nil {
		t.Fatal(err)
	}
	l, err := NewCmd(cmdPlugin(link, "hi"), "", forbidden)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := l.Call(context.Background(), "run", map[string]any{"command": "say"}, nil); err != nil || r.Content[0].Text != "hi\n" {
		t.Fatalf("a symlink to an allowed program: %+v %v", r, err)
	}
	_ = os.Remove(link)
	if err := os.Symlink("/bin/sh", link); err != nil {
		t.Fatal(err)
	}
	if _, err := l.Call(context.Background(), "run", map[string]any{"command": "say"}, nil); err == nil {
		t.Fatal("a symlink changed to a forbidden program ran")
	}
	f, err := NewCmd(cmdPlugin("/bin/false"), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := f.Call(context.Background(), "run", map[string]any{"command": "say"}, nil); err != nil || !strings.Contains(r.Content[0].Text, "[exit status 1]") {
		t.Fatalf("nonzero exit: %+v %v", r, err)
	}
}

// TestCmdProgramRoot: under a program root a command runs the program of that tree, never the
// machine's, and a forbidden program in the tree is refused at start.
func TestCmdProgramRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "bin", "echo"), []byte("#!/bin/sh\nprintf 'stub %s\\n' \"$1\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	c, err := NewCmdUnder(cmdPlugin("/bin/echo", "hi"), "", root, map[string]bool{"sh": true})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := c.Call(context.Background(), "run", map[string]any{"command": "say"}, nil); err != nil || r.Content[0].Text != "stub hi\n" {
		t.Fatalf("the stub: %+v %v", r, err)
	}
	m, err := NewCmdUnder(cmdPlugin("/bin/false"), "", root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Call(context.Background(), "run", map[string]any{"command": "say"}, nil); err == nil {
		t.Fatal("a program missing in the tree ran from the machine")
	}
	if err := os.Symlink("/bin/sh", filepath.Join(root, "bin", "status")); err != nil {
		t.Fatal(err)
	}
	shReal, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewCmdUnder(cmdPlugin("/bin/status", "-c", "id"), "", root, map[string]bool{"sh": true, filepath.Base(shReal): true}); err == nil {
		t.Fatal("a forbidden program in the tree passed")
	}
}

func TestLogs(t *testing.T) {
	dir := t.TempDir()
	log := filepath.Join(dir, "a.log")
	var b strings.Builder
	for i := range 50 {
		b.WriteString("line ")
		b.WriteString(string(rune('A' + i%26)))
		b.WriteString("\n")
	}
	if err := os.WriteFile(log, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.log")
	if err := os.Symlink(log, link); err != nil {
		t.Fatal(err)
	}
	l := &Logs{Sources: map[string]string{"a": log, "link": link, "dir": dir}}
	r, err := l.Call(context.Background(), "tail", map[string]any{"source": "a", "lines": 10.0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(r.Content[0].Text, "\n")
	if len(lines) != 10 || lines[9] != "line X" {
		t.Fatalf("tail %q", r.Content[0].Text)
	}
	for _, src := range []string{"link", "dir", "nope"} {
		if _, err := l.Call(context.Background(), "tail", map[string]any{"source": src}, nil); err == nil {
			t.Fatalf("source %s was read", src)
		}
	}
	// With a byte bound, the cut first line is dropped.
	got, err := tail(log, 100, 20)
	if err != nil || got != "line W\nline X" {
		t.Fatalf("bounded tail %q %v", got, err)
	}
}

func TestChecks(t *testing.T) {
	res := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(res, []byte(`{"www-health": {"ok": 3, "total": 4, "last_status": 503, "ms_max": 812, "note": "x"}, "bad": {"ok": -1, "total": 1}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Checks{Results: res}
	r, err := c.Call(context.Background(), "latest", map[string]any{"check": "www-health"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sc := r.StructuredContent
	if sc["ok"] != 3 || sc["total"] != 4 || sc["last_status"] != 503 || sc["note"] != nil {
		t.Fatalf("latest %v", sc)
	}
	for _, check := range []string{"bad", "nope"} {
		if _, err := c.Call(context.Background(), "latest", map[string]any{"check": check}, nil); err == nil {
			t.Fatalf("check %s passed", check)
		}
	}
}

// Serve answers discover and tools/list from the manifest, runs a call and cancels a call on
// notifications/cancelled.
func TestServe(t *testing.T) {
	p := cmdPlugin("/bin/echo", "--", "{service.unit}")
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	started := make(chan struct{}, 1)
	h := func(ctx context.Context, tool string, args map[string]any, meta *mcp.Meta) (*mcp.CallResult, error) {
		if args["command"] == "wait" {
			started <- struct{}{}
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return Text("ok " + meta.WorkID), nil
	}
	done := make(chan error, 1)
	go func() { done <- Serve(context.Background(), p, inR, outW, h) }()
	client := mcp.NewConn(outR, inW)
	send := func(id int, method string, params any) {
		b, _ := json.Marshal(params)
		m := mcp.Message{Method: method, Params: b}
		if id > 0 {
			m.ID = json.RawMessage(strings.TrimSpace(string(rune('0' + id))))
		}
		if err := client.Send(m); err != nil {
			t.Fatal(err)
		}
	}
	recv := func() mcp.Message {
		m, err := client.Receive(1 << 20)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	send(1, "tools/list", map[string]any{})
	var l mcp.ListResult
	if err := json.Unmarshal(recv().Result, &l); err != nil || len(l.Tools) != 1 {
		t.Fatalf("tools/list %+v %v", l, err)
	}
	send(2, "tools/call", mcp.CallParams{Name: "run", Arguments: map[string]any{"command": "say"}, Meta: &mcp.Meta{WorkID: "c1"}})
	var r mcp.CallResult
	if err := json.Unmarshal(recv().Result, &r); err != nil || r.Content[0].Text != "ok c1" {
		t.Fatalf("call %+v %v", r, err)
	}
	send(3, "tools/call", mcp.CallParams{Name: "run", Arguments: map[string]any{"command": "wait"}})
	<-started
	send(0, "notifications/cancelled", map[string]any{"requestId": 3})
	r = mcp.CallResult{}
	if err := json.Unmarshal(recv().Result, &r); err != nil || !r.IsError {
		t.Fatalf("cancelled call %+v %v", r, err)
	}
	send(4, "sampling/createMessage", map[string]any{})
	if m := recv(); m.Error == nil {
		t.Fatal("an unknown method was served")
	}
	inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// service/control runs systemctl with the resolved unit after --, never a unit of the model,
// and returns only the state (MON-8, DEP-4).
func TestService(t *testing.T) {
	var calls [][]string
	state := "activating"
	s := &Service{Wait: 3 * time.Second, run: func(ctx context.Context, name string, args ...string) (string, int, error) {
		calls = append(calls, append([]string{name}, args...))
		if args[0] == "is-active" {
			out := state
			state = "active"
			return out + "\n", 3, nil
		}
		return "noise from systemctl", 0, nil
	}}
	meta := &mcp.Meta{Resolved: map[string]string{"service.unit": "nginx.service"}}
	r, err := s.Call(context.Background(), "control", map[string]any{"service": "nginx", "verb": "restart"}, meta)
	if err != nil || r.StructuredContent["state"] != "active" || len(r.Content) != 0 {
		t.Fatalf("restart %+v %v", r, err)
	}
	if strings.Join(calls[0], " ") != "/usr/bin/systemctl restart -- nginx.service" {
		t.Fatalf("argv %v", calls[0])
	}
	for _, bad := range []struct {
		verb, unit string
	}{{"reload", "nginx.service"}, {"restart", "nginx@1.service"}, {"restart", "-nginx.service"}, {"restart", ""}, {"restart", "nginx.socket"}} {
		if _, err := s.Call(context.Background(), "control", map[string]any{"verb": bad.verb}, &mcp.Meta{Resolved: map[string]string{"service.unit": bad.unit}}); err == nil {
			t.Errorf("%v ran", bad)
		}
	}
}

// The oneshot queue gets one request file per approval or grant number (APR-7).
func TestQueue(t *testing.T) {
	dir := t.TempDir()
	q := &Queue{Dir: dir}
	meta := &mcp.Meta{WorkID: "sg_01JB0000000000000000000000"}
	for range 2 {
		r, err := q.Call(context.Background(), "clean_tmp", map[string]any{"host": "web1"}, meta)
		if err != nil || r.StructuredContent["code"] != "accepted" {
			t.Fatalf("%+v %v", r, err)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 || ents[0].Name() != "sg_01JB0000000000000000000000.json" {
		t.Fatalf("queue %v", ents)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, ents[0].Name()))
	var req map[string]any
	if json.Unmarshal(raw, &req) != nil || req["tool"] != "clean_tmp" || req["args"].(map[string]any)["host"] != "web1" {
		t.Fatalf("request %s", raw)
	}
	for _, bad := range []string{"../x", "cmp_01JB0000000000000000000000:0", "cmp_01JB0000000000000000000000:1/x"} {
		if _, err := q.Call(context.Background(), "clean_tmp", nil, &mcp.Meta{WorkID: bad}); err == nil {
			t.Fatalf("a request without a number was queued: %q", bad)
		}
	}
	// A campaign item's number is the campaign's id and the item (APR-7).
	cdir := t.TempDir()
	cq := &Queue{Dir: cdir}
	r, err := cq.Call(context.Background(), "clean_tmp", nil, &mcp.Meta{WorkID: "cmp_01JB0000000000000000000000:2"})
	ents, _ = os.ReadDir(cdir)
	if err != nil || r.StructuredContent["run"] != "cmp_01JB0000000000000000000000:2" || len(ents) != 1 || ents[0].Name() != "cmp_01JB0000000000000000000000-2.json" {
		t.Fatalf("campaign item: %+v %v %v", r, err, ents)
	}
}

// The polkit rule holds only the exact units and the catalog verbs (spec/bindings/os/systemd.md).
func TestPolkitRule(t *testing.T) {
	r, err := PolkitRule("tenants/demo/catalog.yaml", "botaru-act-demo", map[string][]string{"nginx.service": {"start", "stop", "restart"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`subject.user !== "botaru-act-demo"`, `unit === "nginx.service" && (verb === "restart" || verb === "start" || verb === "stop")`, "return polkit.Result.NO;"} {
		if !strings.Contains(r, want) {
			t.Fatalf("rule lacks %q:\n%s", want, r)
		}
	}
	if _, err := PolkitRule("x", "botaru-act-demo", map[string][]string{"nginx*.service": {"restart"}}); err == nil {
		t.Fatal("a mask passed")
	}
	if _, err := PolkitRule("x", "botaru-act-demo", map[string][]string{"nginx.service": {"reload"}}); err == nil {
		t.Fatal("a verb outside the three passed")
	}
}
