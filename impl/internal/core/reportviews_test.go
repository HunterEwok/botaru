// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// reportTask asks /report in alice's direct chat and claims the reporter attempt.
func reportTask(t *testing.T, b *bench, reporter *Worker, update string) (string, *WorkItem) {
	t.Helper()
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/report"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(context.Background(), b.channel, a)
	if err != nil || res.Result != "task" {
		t.Fatalf("submit_activity: %+v %v", res, err)
	}
	return res.Task, claimFor(t, b, reporter, res.Task)
}

func viewOf(t *testing.T, b *bench, att *WorkItem, name string) map[string]any {
	t.Helper()
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	ok, text := msgCall(t, b, h, name, nil)
	if !ok {
		t.Fatalf("%s: %s", name, text)
	}
	var v map[string]any
	if err := json.Unmarshal([]byte(text), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// The views status.workers and status.campaigns of a reporter (PRF-1, REP-8): the self-report
// column and the mismatch code are of the deferred STA-3, so a worker's own report does not
// reach the reporter at all, neither in the view nor through the reporter's inbox, and the row
// holds the core observation only; a campaign shows its kind, state and item counts, one above
// the mark only counts in hidden.
func TestStatusWorkersCampaigns(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		ms = reporterManifests(ms)
		for _, m := range ms {
			if m["kind"] == "Role" && m["metadata"].(map[string]any)["name"] == "reporter" {
				m["spec"].(map[string]any)["views"] = []any{"status.tasks", "status.workers", "status.campaigns"}
			}
		}
		return append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "MessageType", "metadata": map[string]any{"name": "state.worker.v1"},
			"spec": map[string]any{"kind": "state", "stale_after": "10m", "note": map[string]any{"max_chars": 0}, "producers": []any{"code"},
				"fields_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"phase"},
					"properties": map[string]any{"phase": map[string]any{"enum": []any{"idle", "busy", "draining"}}}},
				"data_class": "summary", "max_bytes": 512}})
	})
	reporter := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reporter", Kind: "agent"})
	// The agent says it is busy and goes silent past two long-poll periods.
	if _, err := b.c.MsgSend(ctx, b.agent, "", MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": "busy"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TestClockAdvance(ctx, 200); err != nil {
		t.Fatal(err)
	}
	// Two campaigns of greetings: one of the class summary, one internal above the reporter's.
	for i, mark := range []string{"summary", "internal"} {
		id := []string{"cmp_01", "cmp_02"}[i]
		rec := map[string]any{"template": map[string]any{"kind": "task", "type": "hello.greet.v1"}, "data_mark": mark}
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO campaigns (id, tenant, initiator, state, plan_hash, record, nonce, card, created_at, updated_at)
			VALUES ($1, 'demo', $2, 'running', 'h', $3, 'n', false, botaru_now(), botaru_now())`, id, b.alice, rec); err != nil {
			t.Fatal(err)
		}
		for n, st := range []string{"ok", "pending"} {
			if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO campaign_items (campaign, n, resource, args, args_hash, state) VALUES ($1, $2, 'r', '{}', 'h', $3)`, id, n+1, st); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, att := reportTask(t, b, reporter, "1")
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	names := toolNames(t, b, h)
	if !containsString(names, statusWorkersName) || !containsString(names, statusCampaignsName) || containsString(names, statusProposalsName) {
		t.Fatalf("catalog %v", names)
	}
	ws := viewOf(t, b, att, statusWorkersName)["workers"].([]any)
	byID := map[string]map[string]any{}
	for _, w := range ws {
		m := w.(map[string]any)
		byID[m["worker"].(string)] = m
	}
	agent, rep := byID[strings.ToLower(b.agent.ID)], byID[strings.ToLower(reporter.ID)]
	if agent == nil || rep == nil {
		t.Fatalf("workers %v", ws)
	}
	obs := agent["observed"].(map[string]any)
	if obs["presence"] != "stale" || obs["lifecycle"] != "active" || agent["mismatch"] != nil || agent["reported"] != nil {
		t.Fatalf("agent row %v", agent)
	}
	if rep["observed"].(map[string]any)["presence"] != "connected" || rep["reported"] != nil || rep["mismatch"] != nil {
		t.Fatalf("reporter row %v", rep)
	}
	// The reporter's inbox never holds the state value (REP-8).
	var inReporter int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM messages WHERE inbox_name = 'reporter'`).Scan(&inReporter); err != nil || inReporter != 0 {
		t.Fatalf("reporter inbox %d %v", inReporter, err)
	}
	cv := viewOf(t, b, att, statusCampaignsName)
	cs := cv["campaigns"].([]any)
	if len(cs) != 1 || cv["hidden"] != float64(1) {
		t.Fatalf("campaigns %v", cv)
	}
	c0 := cs[0].(map[string]any)
	if c0["campaign"] != "cmp_01" || c0["state"] != "running" || c0["items"] != float64(2) || c0["counts"].(map[string]any)["ok"] != float64(1) {
		t.Fatalf("campaign row %v", c0)
	}
	// Both views come from the one snapshot: a later state value and heartbeat do not show.
	if _, err := b.c.MsgSend(ctx, b.agent, "", MsgSendRequest{Type: "state.worker.v1", Fields: map[string]any{"phase": "idle"}}); err != nil {
		t.Fatal(err)
	}
	for _, w := range viewOf(t, b, att, statusWorkersName)["workers"].([]any) {
		if m := w.(map[string]any); m["worker"] == strings.ToLower(b.agent.ID) && (m["reported"] != nil || m["observed"].(map[string]any)["presence"] != "stale") {
			t.Fatalf("snapshot moved %v", m)
		}
	}
}

// The mismatch table in its order (STA-3): the first condition that holds gives the code.
func TestMismatchCodes(t *testing.T) {
	now := time.Now()
	ago := func(s int) *time.Time { x := now.Add(-time.Duration(s) * time.Second); return &x }
	rep := func(phase string, at int, plugins []string) *selfReport {
		return &selfReport{phase: phase, at: *ago(at), plugins: plugins}
	}
	for _, c := range []struct {
		presence        string
		leases          int
		lastLease, take *time.Time
		plugins         []string
		r               *selfReport
		waiting         bool
		want            string
	}{
		{"stale", 0, nil, nil, nil, rep("idle", 10, nil), false, "reported_ready_unreachable"},
		{"connected", 0, nil, ago(400), nil, rep("idle", 10, nil), true, "reported_idle_not_claiming"},
		{"connected", 0, nil, ago(400), nil, rep("idle", 10, nil), false, ""},
		{"connected", 0, ago(700), nil, nil, rep("busy", 10, nil), false, "reported_busy_no_lease"},
		{"connected", 1, ago(700), nil, nil, rep("busy", 10, nil), false, ""},
		{"connected", 1, nil, ago(5), nil, rep("draining", 60, nil), false, "reported_draining_still_claiming"},
		{"connected", 0, nil, nil, []string{"ssh"}, rep("degraded", 10, []string{"ssh", "web"}), false, "reported_plugins_differ"},
		{"connected", 0, nil, nil, []string{"ssh"}, rep("degraded", 700, []string{"ssh"}), false, "state_stale"},
	} {
		if got := mismatchCode(now, c.presence, c.leases, c.lastLease, c.take, c.plugins, c.r, 10*time.Minute, c.waiting); got != c.want {
			t.Errorf("%+v: %q, want %q", c, got, c.want)
		}
	}
}

// A group chat bound with model_text: labeled gets the paragraph of a reporter attempt that
// saw untrusted text, marked labeled with its sources after the table; one bound summary_only
// gets the table only (REP-2, DLV-15).
func TestDigestLabeledGroup(t *testing.T) {
	ctx := context.Background()
	b := newReporterBench(t)
	reporter := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reporter", Kind: "agent"})
	b.hello(t, "1")
	for i, mode := range []string{"labeled", "summary_only"} {
		bind := ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "summary", ModelText: mode}
		if _, err := b.c.ChatBind(ctx, b.op, bind); err != nil {
			t.Fatalf("chat.bind %s: %v", mode, err)
		}
		task, att := reportTask(t, b, reporter, []string{"2", "3"}[i])
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('origin', '{"channel": "telegram", "chat": {"kind": "group", "external_id": "-500"}}'::jsonb,
			'untrusted_seen', true, 'untrusted_sources', '["file"]'::jsonb) WHERE id = $1`, task); err != nil {
			t.Fatal(err)
		}
		if _, err := b.c.Complete(ctx, reporter, header(t, att), CompleteRequest{Result: map[string]any{"text": "A quiet window, the greetings went fine."}}); err != nil {
			t.Fatal(err)
		}
		var body []byte
		var origin string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT body, dataorigin FROM deliveries WHERE task = $1 AND kind = 'digest'`, task).Scan(&body, &origin); err != nil {
			t.Fatalf("%s digest: %v", mode, err)
		}
		var d map[string]any
		if err := json.Unmarshal(body, &d); err != nil {
			t.Fatal(err)
		}
		if len(d["rows"].([]any)) == 0 {
			t.Fatalf("%s: the table is missing %s", mode, body)
		}
		switch mode {
		case "labeled":
			if d["text"] == nil || d["labeled"] != true || len(d["sources"].([]any)) != 1 || origin != "model" {
				t.Fatalf("labeled digest %s %s", body, origin)
			}
		default:
			if d["text"] != nil || d["labeled"] != nil || origin != "code" {
				t.Fatalf("summary_only digest %s %s", body, origin)
			}
		}
	}
	// Any other value is refused.
	bad := ChatBindRequest{Op: "chat.bind", Tenant: "demo", Channel: "telegram", Chat: Chat{Kind: "group", ExternalID: "-500"}, Ceiling: "summary", ModelText: "full"}
	if _, err := b.c.ChatBind(ctx, b.op, bad); err == nil {
		t.Fatal("model_text full accepted")
	}
}
