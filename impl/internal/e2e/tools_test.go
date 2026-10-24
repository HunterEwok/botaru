// SPDX-License-Identifier: AGPL-3.0-only

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/agent"
	"github.com/HunterEwok/botaru/impl/internal/channel/telegram"
	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/mockprovider"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/plugins"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
	"github.com/HunterEwok/botaru/impl/internal/toolhost"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// fakePlugin runs in the plugin process: the checks plugin of the manifest in
// BOTARU_E2E_MANIFEST, served as is (checks), or with a tools/call that sends a sampling
// request back (sample) or never ends and ignores cancellation (hang).
func fakePlugin(mode string) int {
	if mode == "sleep" {
		select {}
	}
	docs, err := manifest.ReadFiles([]string{os.Getenv("BOTARU_E2E_MANIFEST")})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	p, err := toolspec.Decode(docs[0].Name, docs[0].Value["spec"].(map[string]any))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	switch mode {
	case "fork", "checks":
		if mode == "fork" {
			// A child left behind in the plugin's process group, as a daemonizing command would.
			child := exec.Command(os.Args[0])
			child.Env = []string{"BOTARU_E2E_PLUGIN=sleep"}
			if err := child.Start(); err != nil {
				return 1
			}
			pidFile := filepath.Join(filepath.Dir(os.Getenv("BOTARU_E2E_RESULTS")), "child.pid")
			if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
				return 1
			}
		}
		h := (&plugins.Checks{Results: os.Getenv("BOTARU_E2E_RESULTS")}).Call
		if err := plugins.Serve(context.Background(), p, os.Stdin, os.Stdout, h); err != nil {
			return 1
		}
		return 0
	case "sample", "hang":
		conn := mcp.NewConn(os.Stdin, os.Stdout)
		for {
			m, err := conn.Receive(1 << 20)
			if err != nil {
				return 0
			}
			switch m.Method {
			case "tools/list":
				_ = conn.Send(mcp.Response(m.ID, mcp.ListResult{Tools: p.LiveTools()}))
			case "tools/call":
				if mode == "hang" {
					fmt.Fprintln(os.Stderr, "hanging; password=hunter2")
					select {}
				}
				_ = conn.Send(mcp.Message{ID: json.RawMessage(`"s1"`), Method: "sampling/createMessage", Params: json.RawMessage(`{}`)})
			}
		}
	}
	return 2
}

// toolManifests is the hello-bot set with the checks plugin, a catalog with web1 and its
// checks, and the hello role holding checks/latest.
func toolManifests(t *testing.T, root string) []map[string]any {
	t.Helper()
	ms, err := testbench.SharedHelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	docs, err := manifest.ReadFiles([]string{filepath.Join(root, "examples/tools/checks/tool.yaml")})
	if err != nil {
		t.Fatal(err)
	}
	ms = append(ms, docs[0].Value, map[string]any{
		"apiVersion": "botaru/v1alpha1", "kind": "Catalog", "metadata": map[string]any{"name": "demo", "tenant": "demo"},
		"spec": map[string]any{
			"hosts": []any{map[string]any{"id": "web1"}},
			"resources": []any{map[string]any{"id": "web-checks", "title": "Checks", "host": "web1", "read_by": []any{"checks/latest"},
				"list": "checks", "format": "id", "values": []any{"www-health"}}},
		},
	})
	for _, m := range ms {
		if m["kind"] == "Role" {
			m["spec"].(map[string]any)["tools"] = []any{"checks/latest"}
		}
	}
	sp, err := testbench.SharedSpec(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testbench.DropUnsupported(sp, ms); err != nil {
		t.Fatal(err)
	}
	return ms
}

// toolRig is the rig with the tool manifests applied, alice, an agent and a toolhost of the
// checks plugin on web1 whose plugin process runs in the given mode.
type toolRig struct {
	*rig
	agent      *agent.Agent
	agentToken string
	host       *toolhost.Host
	results    string
}

func newToolRig(t *testing.T, mode string, killAfter time.Duration) *toolRig {
	t.Helper()
	r := newRig(t)
	ms := toolManifests(t, r.root)
	var applied struct {
		Applied bool `json:"applied"`
		Errors  any  `json:"errors"`
	}
	r.p2call(t, r.farmToken, "apply", map[string]any{"commit": strings.Repeat("b", 40), "manifests": ms}, &applied)
	if !applied.Applied {
		t.Fatalf("apply refused: %v", applied.Errors)
	}
	var done map[string]any
	alice := ids.PrincipalID()
	r.p2call(t, r.opToken, "principal.add", map[string]any{"op": "principal.add", "subject": alice, "identity": "telegram:1001"}, &done)
	r.p2call(t, r.opToken, "grant", map[string]any{"op": "grant", "subject": alice, "group": "hello"}, &done)
	var agentEnroll, hostEnroll, chanEnroll struct {
		Token string `json:"token"`
	}
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "hello", "kind": "agent"}, &agentEnroll)
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "telegram", "kind": "channel", "target": "telegram"}, &chanEnroll)
	r.p2call(t, r.opToken, "worker.enroll", map[string]any{"tenant": "demo", "role": "toolhost", "kind": "toolhost", "plugins": []string{"checks"}, "placement": "host", "host": "web1"}, &hostEnroll)

	results := filepath.Join(t.TempDir(), "results.json")
	if err := os.WriteFile(results, []byte(`{"www-health": {"ok": 2, "total": 3, "last_status": 502}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	h, err := toolhost.New(toolhost.Config{
		Core:      &p1client.Client{BaseURL: r.p1.URL, Token: hostEnroll.Token},
		Placement: "host", KillAfter: killAfter,
		Plugins: []toolhost.Plugin{{Name: "checks", Command: []string{exe}, Env: []string{
			"BOTARU_E2E_PLUGIN=" + mode, "BOTARU_E2E_RESULTS=" + results,
			"BOTARU_E2E_MANIFEST=" + filepath.Join(r.root, "examples/tools/checks/tool.yaml"),
		}}},
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := h.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	ag := agent.New(agent.Config{Core: &p1client.Client{BaseURL: r.p1.URL, Token: agentEnroll.Token}})
	if err := ag.Hello(ctx); err != nil {
		t.Fatal(err)
	}
	// The task: a /hello from alice through the channel worker's P1 submit.
	ch := &p1client.Client{BaseURL: r.p1.URL, Token: chanEnroll.Token}
	tx, _ := testbench.SharedTexts(r.root)
	if err := telegram.New(telegram.Config{Core: ch, Channel: "telegram", BotAPI: r.botURL, BotToken: "TEST:TOKEN", Texts: tx}).Hello(ctx); err != nil {
		t.Fatal(err)
	}
	var sub map[string]any
	if err := ch.Call(ctx, "submit_activity", "", map[string]any{"kind": "message", "channel": "telegram", "update_id": "1",
		"chat": map[string]any{"kind": "direct", "external_id": "1001"}, "text": "/hello hi",
		"sender": map[string]any{"identity": "telegram:1001", "verified": true}}, &sub); err != nil || sub["result"] != "task" {
		t.Fatalf("submit_activity %v %v", sub, err)
	}
	return &toolRig{rig: r, agent: ag, agentToken: agentEnroll.Token, host: h, results: results}
}

// toolTurns is a mock scenario: the model calls checks_latest, then answers.
func toolTurns() *mockprovider.Scenario {
	sc := &mockprovider.Scenario{Profile: "base-std", Turns: make([]mockprovider.Turn, 2)}
	sc.Turns[0].ExpectRequest.Tools = []string{"checks_latest"}
	sc.Turns[0].Respond.StopReason = "tool_use"
	sc.Turns[0].Respond.ToolUse = &struct {
		Name  string         `yaml:"name"`
		Input map[string]any `yaml:"input"`
	}{Name: "checks_latest", Input: map[string]any{"check": "www-health"}}
	sc.Turns[0].Respond.Usage = map[string]int64{"input_tokens": 200, "output_tokens": 10}
	sc.Turns[1].ExpectRequest.Tools = []string{"checks_latest"}
	sc.Turns[1].Respond.Result = map[string]any{"answer": "The site answers 2 of 3 checks."}
	sc.Turns[1].Respond.Usage = map[string]int64{"input_tokens": 300, "output_tokens": 20}
	return sc
}

// The model calls a plugin tool over P3; the toolhost runs it in a plugin process and the
// answer goes back to the model, which then finishes the task (P3, E3, DEP-7).
func TestToolCallOverTheWire(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newToolRig(t, "checks", 2*time.Second)
	r.mock.Reset(toolTurns())
	go func() {
		for ctx.Err() == nil {
			_, _ = r.host.Once(ctx, 1)
		}
	}()
	if ok, err := r.agent.Once(ctx, 2); err != nil || !ok {
		t.Fatalf("attempt: %v %v", ok, err)
	}
	var state, output string
	if err := r.c.Store().Pool.QueryRow(ctx, `SELECT state, output::text FROM toolcalls`).Scan(&state, &output); err != nil || state != "done" {
		t.Fatalf("tool call %s %v", state, err)
	}
	if !strings.Contains(output, `"total": 3`) && !strings.Contains(output, `"total":3`) {
		t.Fatalf("tool output %s", output)
	}
	var taskState string
	if err := r.c.Store().Pool.QueryRow(ctx, `SELECT state FROM tasks`).Scan(&taskState); err != nil || taskState != "completed" {
		t.Fatalf("task %s %v", taskState, err)
	}
	if calls := r.mock.Calls(); len(calls) != 2 {
		t.Fatalf("model calls %d", len(calls))
	}
}

// Nothing a plugin started outlives its call: a child left in its process group is killed
// after the plugin exits on time (FEN-6).
func TestPluginChildrenDie(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newToolRig(t, "fork", 2*time.Second)
	r.mock.Reset(toolTurns())
	go func() {
		for ctx.Err() == nil {
			_, _ = r.host.Once(ctx, 1)
		}
	}()
	if ok, err := r.agent.Once(ctx, 2); err != nil || !ok {
		t.Fatalf("attempt: %v %v", ok, err)
	}
	var state string
	if err := r.c.Store().Pool.QueryRow(ctx, `SELECT state FROM toolcalls`).Scan(&state); err != nil || state != "done" {
		t.Fatalf("tool call %s %v", state, err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(r.results), "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	for range 100 {
		// A killed child that nobody reaped yet is a zombie: it counts as gone.
		st, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		if err != nil || strings.Contains(string(st), ") Z ") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatal("a child of the plugin outlived the call")
}

// A plugin that asks the client for sampling gets a refusal and the call ends with
// exec_failed (INJ-5); a plugin that ignores cancellation is killed after kill_after when
// the call deadline passes (FEN-6).
func TestToolhostRefusesAndKills(t *testing.T) {
	for _, mode := range []string{"sample", "hang"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			r := newToolRig(t, mode, 500*time.Millisecond)
			r.mock.Reset(toolTurns())
			hostDone := make(chan time.Duration, 1)
			go func() {
				start := time.Now()
				for ctx.Err() == nil {
					if ok, _ := r.host.Once(ctx, 1); ok {
						hostDone <- time.Since(start)
						return
					}
				}
			}()
			if ok, err := r.agent.Once(ctx, 2); err != nil || !ok {
				t.Fatalf("attempt: %v %v", ok, err)
			}
			var state, reason string
			if err := r.c.Store().Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM toolcalls`).Scan(&state, &reason); err != nil {
				t.Fatal(err)
			}
			if state != "error" || reason != "exec_failed" {
				t.Fatalf("tool call %s %s", state, reason)
			}
			select {
			case d := <-hostDone:
				if mode == "hang" && d > 15*time.Second {
					t.Fatalf("the hanging plugin took %v", d)
				}
			case <-time.After(20 * time.Second):
				t.Fatal("the toolhost did not finish the call")
			}
		})
	}
}

// P4 refuses a request whose tools are not exactly the P3 catalog of the handle: missing,
// extra or changed (GW-1); none of them reaches the provider.
func TestP4RequiresTheCatalog(t *testing.T) {
	ctx := context.Background()
	r := newToolRig(t, "checks", time.Second)
	ag := &p1client.Client{BaseURL: r.p1.URL, Token: r.agentToken}
	w, err := ag.Claim(ctx, []string{"attempt"}, 1)
	if err != nil || w == nil {
		t.Fatalf("claim %v %v", w, err)
	}
	var p struct {
		Handles map[string]string `json:"handles"`
	}
	if err := json.Unmarshal(w.Payload, &p); err != nil {
		t.Fatal(err)
	}
	var list mcp.ListResult
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/list", "params": map[string]any{}})
	code, body := rawPost(t, r.p1.URL+"/v1/p3/mcp", p.Handles["tools"], "", string(raw))
	if b, _ := json.Marshal(body["result"]); code != 200 || json.Unmarshal(b, &list) != nil || len(list.Tools) != 1 {
		t.Fatalf("tools/list %d %v", code, body)
	}
	tool := list.Tools[0].(map[string]any)
	good := map[string]any{"name": tool["name"], "input_schema": tool["inputSchema"]}
	changed := map[string]any{"name": tool["name"], "input_schema": map[string]any{"type": "object"}}
	extra := map[string]any{"name": "logs_tail", "input_schema": map[string]any{"type": "object"}}
	for i, tools := range [][]any{nil, {changed}, {good, extra}, {good, good}} {
		req := map[string]any{"model": "x", "max_tokens": 100, "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
		if tools != nil {
			req["tools"] = tools
		}
		b, _ := json.Marshal(req)
		if code, body := rawPost(t, r.p1.URL+"/v1/p4/anthropic/v1/messages", p.Handles["model"], "", string(b)); code != 400 || body["reason"] != "input_schema_invalid" {
			t.Errorf("case %d: %d %v", i, code, body)
		}
	}
	if n := len(r.mock.Calls()); n != 0 {
		t.Fatalf("%d calls reached the provider", n)
	}
	// The exact catalog passes.
	r.mock.Reset(toolTurns())
	b, _ := json.Marshal(map[string]any{"model": "x", "max_tokens": 100, "tools": []any{good},
		"messages": []any{map[string]any{"role": "user", "content": "hi"}}})
	if code, body := rawPost(t, r.p1.URL+"/v1/p4/anthropic/v1/messages", p.Handles["model"], "", string(b)); code != 200 {
		t.Fatalf("the exact catalog: %d %v", code, body)
	}
}
