// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// newReporterBench is the hello bench with a reporter role of the views status.tasks and
// status.proposals, its type report.status.v1 under the command /report, and the group hello
// allowed to read greetings and to hold internal.
func newReporterBench(t *testing.T) *bench {
	t.Helper()
	return newBenchWith(t, func(_ string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			s := m["spec"].(map[string]any)
			switch m["kind"] {
			case "Group":
				s["caps"] = append(s["caps"].([]any), "task:report.status.v1", "task.read:hello.greet.v1")
				s["data"] = append(s["data"].([]any), "internal")
			case "Tenant":
				cmds, _ := s["commands"].(map[string]any)
				if cmds == nil {
					cmds = map[string]any{}
					s["commands"] = cmds
				}
				cmds["/report"] = "report.status.v1"
			}
		}
		return append(ms,
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "reporter"},
				"spec": map[string]any{"profile": "reporter", "worker_kind": "agent", "claims": []any{"report.status.v1"},
					"models": map[string]any{"base": "base-std"}, "memory": "none", "views": []any{"status.tasks", "status.proposals"}}},
			map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "TaskType", "metadata": map[string]any{"name": "report.status.v1"},
				"spec": map[string]any{"title": "Status report", "worker_kind": "agent", "purpose": "report", "tier": 0, "data_class": "summary",
					"instruction":   "Write one short paragraph about the tasks of the window.",
					"params_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
					"result_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []any{"text"},
						"properties": map[string]any{"text": map[string]any{"type": "string", "maxLength": 800}}},
					"tree_budget_eur": 0.05, "proposable": false}},
		)
	})
}

// A reporter task asked in a direct chat reads its views from one snapshot filtered by the
// asker's permissions, rows above its mark only count as hidden, and its answer is a digest the
// code builds: the table from the snapshot and the paragraph as a separate model part, to the
// origin only (REP-2, REP-3, REP-7, REP-8, REP-9).
func TestReporterDigest(t *testing.T) {
	ctx := context.Background()
	b := newReporterBench(t)
	reporter := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reporter", Kind: "agent"})
	b.hello(t, "1")
	high := b.hello(t, "2")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"data_mark": "internal"}' WHERE id = $1`, high); err != nil {
		t.Fatal(err)
	}
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "3", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/report"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || res.Result != "task" {
		t.Fatalf("submit_activity: %+v %v", res, err)
	}
	var rec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record FROM tasks WHERE id = $1`, res.Task).Scan(&rec); err != nil {
		t.Fatal(err)
	}
	var r struct {
		Mark     string    `json:"data_mark"`
		Audience *audience `json:"audience"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		t.Fatal(err)
	}
	if r.Mark != "summary" || r.Audience == nil || r.Audience.Ceiling != "summary" || len(r.Audience.Principals) != 1 || r.Audience.Principals[0] != b.alice {
		t.Fatalf("audience %s", rec)
	}
	att := claimFor(t, b, reporter, res.Task)
	h := att.Payload["handles"].(map[string]any)["tools"].(string)
	names := toolNames(t, b, h)
	if !containsString(names, statusTasksName) || !containsString(names, statusProposalsName) {
		t.Fatalf("catalog %v", names)
	}
	view := func() map[string]any {
		ok, text := msgCall(t, b, h, statusTasksName, nil)
		if !ok {
			t.Fatalf("status.tasks: %s", text)
		}
		var v map[string]any
		if err := json.Unmarshal([]byte(text), &v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	first := view()
	counts, _ := first["counts"].([]any)
	if len(counts) != 1 || first["hidden"] != float64(1) || counts[0].(map[string]any)["count"] != float64(1) {
		t.Fatalf("view %v", first)
	}
	// The snapshot holds: a new task after the first read is not in the view.
	b.hello(t, "4")
	if second := view(); second["snapshot_id"] != first["snapshot_id"] || len(second["tasks"].([]any)) != 1 {
		t.Fatalf("snapshot moved: %v", second)
	}
	if _, err := b.c.Complete(ctx, reporter, header(t, att), CompleteRequest{Result: map[string]any{"text": "A quiet window, the greetings went fine."}}); err != nil {
		t.Fatal(err)
	}
	var body []byte
	var origin, to string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT body, dataorigin, addr FROM deliveries WHERE task = $1 AND kind = 'digest'`, res.Task).Scan(&body, &origin, &to); err != nil {
		t.Fatalf("digest: %v", err)
	}
	var d map[string]any
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(err)
	}
	if d["form"] != "digest" || d["snapshot_id"] != first["snapshot_id"] || d["hidden"] != float64(1) || len(d["rows"].([]any)) != 1 ||
		d["text"] != "A quiet window, the greetings went fine." || origin != "model" || to != "origin" {
		t.Fatalf("digest %s %s %s", body, origin, to)
	}
}

// Code lets through only a plain paragraph: no link, mention, markup, command or object id, at
// most 800 characters (REP-2).
func TestDigestParagraph(t *testing.T) {
	for text, ok := range map[string]bool{
		"All went well.":                     true,
		"See https://example.org":            false,
		"Ask @admin":                         false,
		"Use /cancel now":                    false,
		"A *bold* word":                      false,
		"Hold h_01J00000000000000000000000.": false,
		strings.Repeat("a", 801):             false,
	} {
		if got := digestParagraph(map[string]any{"text": text}) != ""; got != ok {
			t.Errorf("%q: %v", text, got)
		}
	}
}

// A person who is no principal of the tenant gets the notices and, in a direct chat, the code
// text that they are not registered, at most once per period; the audit keeps only
// activity.ignored(not_subject) (MSG-17).
func TestActivityNotSubject(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	send := func(update string) *ActivityResult {
		a := Activity{Kind: "message", Channel: "telegram", UpdateID: update, Chat: Chat{Kind: "direct", ExternalID: "777"}, Text: "hello there"}
		a.Sender.Identity, a.Sender.Verified = "telegram:777", true
		res, err := b.c.SubmitActivity(ctx, b.channel, a)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	first := send("1")
	if first.Result != "reply" || !strings.Contains(first.Text, "not registered") {
		t.Fatalf("first %+v", first)
	}
	if second := send("2"); second.Result != "ignored" || second.Reason != "not_subject" {
		t.Fatalf("second %+v", second)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' = 'activity.ignored' AND record->>'reason' = 'not_subject'`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("audit %d %v", n, err)
	}
	var tasks int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks`).Scan(&tasks); err != nil || tasks != 0 {
		t.Fatalf("tasks %d %v", tasks, err)
	}
}

// A view with no principal to read for shows nothing, a task without an audience reads for its
// initiator, and the since_last window is the tenant's own (REP-3, REP-7, PRM-9).
func TestViewFilterAudience(t *testing.T) {
	ctx := context.Background()
	b := newReporterBench(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		f, _, err := b.c.viewFilterOf(ctx, tx, now, tv, "system", []byte(`{"data_mark": "summary"}`))
		if err != nil {
			return err
		}
		if f.readable("hello.greet.v1", "") || f.readableMark("") {
			t.Fatalf("a view with no audience reads")
		}
		f, _, err = b.c.viewFilterOf(ctx, tx, now, tv, b.alice, []byte(`{"data_mark": "summary"}`))
		if err != nil {
			return err
		}
		if !f.readable("hello.greet.v1", "") || f.readable("report.status.v1x", "") {
			t.Fatalf("the initiator's filter is wrong")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, tenant, type, state, initiator, priority_class, idempotency_key, body_hash, body, record, deadline_at, created_at, updated_at)
			VALUES ('t_other', 'other', 'report.status.v1', 'completed', 'system', 1, 'other-report', 'h', '{}', '{"enqueued_by": "schedule:daily"}', $1, $1, $1)`, now.Add(-time.Hour)); err != nil {
			return err
		}
		from, _, err := reportWindow(ctx, tx, now, "demo", "t_x", "report.status.v1", []byte(`{"params": {"window": "since_last"}}`), []byte(`{"enqueued_by": "schedule:daily"}`))
		if err != nil {
			return err
		}
		if !from.Before(now.Add(-24 * time.Hour)) {
			t.Fatalf("another tenant's report started the window: %v", from)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// The paragraph is read by tokens: a token with a digit passes only as a name of the table or the
// catalog or as the snapshot time or a window bound, and a number word never passes (REP-4).
func TestParagraphNumbers(t *testing.T) {
	at := time.Date(2026, 10, 16, 9, 30, 0, 0, time.UTC)
	n := numberCheck{names: map[string]bool{"web1": true, "hello.greet.v1": true, "42": true}, exact: map[string]bool{"db7": true},
		times: []time.Time{at, at.Add(-24 * time.Hour)}, loc: time.UTC}
	// Built from a digest: a word of a multi-word title is no name.
	if got := digestNumberCheck(newCatalog(), "demo", &viewSnapshot{Counts: []map[string]any{{"title": "Disk 3 alarm", "type": "web2"}}}); got.names["3"] || !got.names["web2"] {
		t.Errorf("names %v", got.names)
	}
	words := []string{"zero", "one", "two", "three", "twenty"}
	for text, want := range map[string][2]int{
		"All went well on web1.":                {1, 0},
		"Since 2026-10-15 at 09:30 it was calm": {2, 0},
		"Two greetings failed.":                 {1, 1},
		"About 40% failed (hello.greet.v1)":     {2, 1},
		"Twenty-one runs, web2 down at 10:00":   {3, 3},
		"Nothing to report.":                    {0, 0},
		"It was 42 and db7 was calm":            {2, 1},
		"About ½ of them, x² more":              {2, 2},
		"Choose one/two or three.five":          {2, 2},
	} {
		if c, r := n.paragraphNumbers(text, words); c != want[0] || r != want[1] {
			t.Errorf("%q: checked %d rejected %d, want %v", text, c, r, want)
		}
	}
}

// A paragraph with a number is not sent: the digest goes with its code table only, and the core
// writes digest.text_dropped.v1 with counts only (REP-4).
func TestDigestNumbersDropped(t *testing.T) {
	ctx := context.Background()
	b := newReporterBench(t)
	reporter := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "reporter", Kind: "agent"})
	b.hello(t, "1")
	a := Activity{Kind: "message", Channel: "telegram", UpdateID: "3", Chat: Chat{Kind: "direct", ExternalID: "1001"}, Text: "/report"}
	a.Sender.Identity, a.Sender.Verified = "telegram:1001", true
	res, err := b.c.SubmitActivity(ctx, b.channel, a)
	if err != nil || res.Result != "task" {
		t.Fatalf("submit_activity: %+v %v", res, err)
	}
	att := claimFor(t, b, reporter, res.Task)
	if _, err := b.c.Complete(ctx, reporter, header(t, att), CompleteRequest{Result: map[string]any{"text": "Three greetings went fine, 1 was slow."}}); err != nil {
		t.Fatal(err)
	}
	var id, origin string
	var body []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, body, dataorigin FROM deliveries WHERE task = $1 AND kind = 'digest'`, res.Task).Scan(&id, &body, &origin); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "greetings") || origin != "code" {
		t.Fatalf("digest %s %s", body, origin)
	}
	var data []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT data FROM events WHERE type = 'digest.text_dropped.v1' AND subject = $1`, id).Scan(&data); err != nil {
		t.Fatalf("event: %v", err)
	}
	var ev map[string]any
	if err := json.Unmarshal(data, &ev); err != nil {
		t.Fatal(err)
	}
	if ev["numbers_checked"] != float64(2) || ev["numbers_unmatched"] != float64(2) || ev["snapshot_id"] == nil || strings.Contains(string(data), "greetings") {
		t.Fatalf("event %s", data)
	}
}
