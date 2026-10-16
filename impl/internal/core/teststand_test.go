// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

func problemReason(err error) string {
	var p *Problem
	if errors.As(err, &p) {
		return p.Reason
	}
	return ""
}

// The test-control port exists only on a test bench (APR-9): a core without the test bench flag
// refuses every operation of the port, the new ones as the clock and the loops.
func TestBenchOpsOnlyOnTestBench(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	b.c.cfg.TestBench = false
	defer func() { b.c.cfg.TestBench = true }()
	var stop ProcessStopRequest
	stop.Process.Kind, stop.Mode = "core", "kill"
	calls := map[string]func() error{
		"clock.advance":  func() error { _, err := b.c.TestClockAdvance(ctx, 1); return err },
		"process.stop":   func() error { _, err := b.c.TestProcessStop(ctx, stop); return err },
		"store.fault":    func() error { _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "unavailable"}); return err },
		"store.snapshot": func() error { _, err := b.c.TestStoreSnapshot(ctx, "a"); return err },
		"store.restore":  func() error { _, err := b.c.TestStoreRestore(ctx, "a"); return err },
		"authorize.eval": func() error { _, err := b.c.TestAuthorize(ctx, map[string]any{}, ""); return err },
		"function.eval": func() error {
			_, err := b.c.TestFunctionEval(ctx, FunctionEvalRequest{Function: "schedule.check"})
			return err
		},
		"event.inject": func() error { _, err := b.c.TestEventInject(ctx, EventInjectRequest{}); return err },
	}
	for op, call := range calls {
		if r := problemReason(call()); r != "forbidden" {
			t.Errorf("%s on a core that is not a test bench: %q", op, r)
		}
	}
	if err := b.c.st.Ping(ctx); err != nil {
		t.Fatalf("a refused store.fault faulted the store: %v", err)
	}
}

// authorize.eval answers rows of the permission table (PRM-2) in the C2 order: holds first,
// then the bans, then the intersection of the permissions of the principal and every other
// initiator, the chat ceiling and the budget; no allowing row means no (PRM-1). It writes
// nothing.
func TestBenchAuthorizeEval(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	row := func(principal, home, action string, extra map[string]any) map[string]any {
		r := map[string]any{
			"principal": map[string]any{"kind": "human", "id": principal},
			"action":    action,
			"resource":  map[string]any{"tenant": "demo", "kind": "task_type"},
		}
		r["principal"].(map[string]any)["tenant"] = home
		if principal == "system" {
			r["principal"].(map[string]any)["kind"] = "system"
		}
		for k, v := range extra {
			r[k] = v
		}
		return r
	}
	var auditBefore int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit`).Scan(&auditBefore); err != nil {
		t.Fatal(err)
	}
	stranger := ids.PrincipalID()
	cases := []struct {
		name, kind  string
		req         map[string]any
		decision    string
		reason      string
		stage       string
		wantProblem string
	}{
		{"allowed", "", row(b.alice, "demo", "task:hello.greet.v1", nil), "allow", "", "intersection", ""},
		{"type not allowed", "", row(b.alice, "demo", "task:ops.investigate.v1", nil), "deny", "type_not_allowed", "ban", ""},
		{"unknown principal", "", row(stranger, "demo", "task:hello.greet.v1", nil), "deny", "person_inactive", "intersection", ""},
		{"other tenant", "", row(b.alice, "demo2", "task:hello.greet.v1", nil), "deny", "tenant_mismatch", "ban", ""},
		{"action type", "", row(b.alice, "demo", "task:action.fix_web.v1", nil), "deny", "type_not_allowed", "ban", ""},
		{"data class", "", row(b.alice, "demo", "data:raw_logs", nil), "deny", "no_data_class", "intersection", ""},
		{"unknown principal of a capability", "", row(stranger, "demo", "data:raw_logs", nil), "deny", "person_inactive", "intersection", ""},
		{"system has its group only", "", row("system", "demo", "task:hello.greet.v1", nil), "deny", "no_group_cap", "intersection", ""},
		{"every initiator", "", row(b.alice, "demo", "task:hello.greet.v1", map[string]any{"context": map[string]any{
			"task": map[string]any{"initiators": []any{b.alice, stranger}}}}), "deny", "no_group_cap", "intersection", ""},
		{"the mark of the task", "", row(b.alice, "demo", "task:hello.greet.v1", map[string]any{"context": map[string]any{
			"task": map[string]any{"data_mark": "internal"}}}), "deny", "no_data_class", "intersection", ""},
		{"a group chat at the ceiling", "", row(b.alice, "demo", "task:hello.greet.v1", map[string]any{"context": map[string]any{
			"chat": map[string]any{"kind": "group", "ceiling": "summary"}}}), "allow", "", "intersection", ""},
		{"budget", "production", row(b.alice, "demo", "task:hello.greet.v1", map[string]any{"context": map[string]any{
			"budget": map[string]any{"exhausted": true}}}), "deny", "budget_exhausted", "intersection", ""},
		{"self approval in production", "production", row(b.alice, "demo", "action.approve:fix_web", map[string]any{"context": map[string]any{
			"task": map[string]any{"initiators": []any{b.alice}}}}), "deny", "forbidden", "intersection", ""},
		{"not an authz-request", "", map[string]any{"action": "task:hello.greet.v1"}, "", "", "", "input_schema_invalid"},
		{"tenant kind", "staging", row(b.alice, "demo", "task:hello.greet.v1", nil), "", "", "", "input_schema_invalid"},
	}
	for _, tc := range cases {
		d, err := b.c.TestAuthorize(ctx, tc.req, tc.kind)
		if tc.wantProblem != "" {
			if r := problemReason(err); r != tc.wantProblem {
				t.Errorf("%s: %v, want %s", tc.name, err, tc.wantProblem)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if d.Decision != tc.decision || tc.reason != "" && d.Reason != tc.reason || d.Stage != tc.stage {
			t.Errorf("%s: %+v, want %s %s at %s", tc.name, d, tc.decision, tc.reason, tc.stage)
		}
	}
	// A hold of the type comes before everything, even before the tenant ban.
	if _, err := b.c.HoldSet(ctx, OperatorActor(b.op), HoldSetRequest{Op: "hold.set",
		Scope: HoldScope{Kind: "tasktype", Tenant: "demo", Name: "hello.greet.v1"}, Effects: []string{"no_enqueue"}, Reason: "manual"}); err != nil {
		t.Fatal(err)
	}
	var auditMid int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit`).Scan(&auditMid); err != nil {
		t.Fatal(err)
	}
	if auditMid != auditBefore+1 {
		t.Fatalf("authorize.eval wrote audit records: %d before, %d after the hold", auditBefore, auditMid)
	}
	for _, tenant := range []string{"demo", "demo2"} {
		d, err := b.c.TestAuthorize(ctx, row(b.alice, tenant, "task:hello.greet.v1", nil), "")
		if err != nil || d.Decision != "deny" || d.Reason != "held" || d.Stage != "hold" || d.Hold == "" {
			t.Fatalf("held type, principal of %s: %+v %v", tenant, d, err)
		}
	}
}

// function.eval digest.sample is the monthly audit sample (DIG-5), the row dig-06: 6 dismissed
// by a veto, 10 by a low score and 4 parked by score enter, parked by quorum, budget or conflict
// do not; N = max(4, ceil(0.25 x 20)) = 5 with inclusion_prob 0.25; the same seed draws the same
// ids; 10 answers with a weighted share of 0.4 set commission_underrates, 9 answers do not.
func TestBenchDigestSample(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	seed := strings.Repeat("ab", 32)
	in := map[string]any{
		"commission": map[string]any{"audit_sample": map[string]any{"min_per_month": 4, "share": 0.25, "max_per_digest": 2}},
		"month":      "2026-11", "seed": seed,
		"verdicts":     map[string]any{"dismissed_veto": 6, "dismissed_low_score": 10, "parked_score": 4, "parked_quorum": 1, "parked_budget": 1, "parked_conflict": 1},
		"answers_180d": []any{map[string]any{"would_accept": 4, "would_not_accept": 6, "inclusion_prob": 0.25}},
	}
	eval := func(in map[string]any) map[string]any {
		t.Helper()
		r, err := b.c.TestFunctionEval(ctx, FunctionEvalRequest{Function: "digest.sample", Input: in})
		if err != nil {
			t.Fatal(err)
		}
		return r.Output
	}
	out := eval(in)
	if out["population"] != 20 || out["size"] != 5 || out["inclusion_prob"] != 0.25 || out["seed"] != seed {
		t.Fatalf("sample %v", out)
	}
	drawn := out["drawn"].([]any)
	if len(drawn) != 5 {
		t.Fatalf("drawn %v", drawn)
	}
	for _, d := range drawn {
		if s := d.(string); strings.HasPrefix(s, "parked_quorum") || strings.HasPrefix(s, "parked_budget") || strings.HasPrefix(s, "parked_conflict") {
			t.Fatalf("an item parked by %s entered the sample", s)
		}
	}
	if ex := out["excluded_states"].([]any); len(ex) != 3 {
		t.Fatalf("excluded %v", ex)
	}
	if again := eval(in)["drawn"].([]any); strings.Join(strs(again), ",") != strings.Join(strs(drawn), ",") {
		t.Fatalf("the recorded seed drew %v, then %v", drawn, again)
	}
	if out["commission_underrates"] != true || out["audit_sample"].(map[string]any)["would_accept_share"] != 0.4 {
		t.Fatalf("commission_underrates with 10 answers: %v", out["audit_sample"])
	}
	in["answers_180d"] = []any{map[string]any{"would_accept": 9, "would_not_accept": 0, "inclusion_prob": 0.25}}
	if out := eval(in); out["commission_underrates"] != false {
		t.Fatalf("commission_underrates with 9 answers: %v", out["audit_sample"])
	}
	delete(in, "seed")
	if a, c := eval(in)["seed"], eval(in)["seed"]; a == c {
		t.Fatal("two draws without a seed used the same seed")
	}
}

func strs(l []any) []string {
	out := make([]string, len(l))
	for i, v := range l {
		out[i], _ = v.(string)
	}
	return out
}

// function.eval reporter.number_check is the number check of a model paragraph (REP-4), the rows
// of fx-rep-no-numbers: names with digits from the table and the catalog pass, a number copied
// from the table, a number word, a percent, a number glued to a unit, a date outside the snapshot
// and the window and an unknown name with digits each drop the paragraph; a date and a time of
// the window pass.
func TestBenchNumberCheck(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	base := map[string]any{
		"binding":       map[string]any{"target": "direct:alice", "kind": "direct", "locale": "en"},
		"catalog_names": []any{"web1", "db1", "st-05"},
		"snapshot": map[string]any{"id": "snap-nn", "taken_at": "2026-11-20T09:00:00Z",
			"window": map[string]any{"from": "2026-11-19T09:00:00Z", "to": "2026-11-20T09:00:00Z"},
			"rows":   []any{map[string]any{"resource": "web1", "failed": 37, "ok": 1204}, map[string]any{"resource": "db1", "failed": 0, "ok": 880}}},
	}
	for _, tc := range []struct {
		text      string
		paragraph string
		unmatched int
	}{
		{"web1 had most of the failures; db1 and st-05 ran clean.", "present", 0},
		{"web1 had 37 failures.", "absent", 1},
		{"db1 had zero failures.", "absent", 1},
		{"About 97% succeeded and web1 kept 120GB free.", "absent", 2},
		{"Since 2026-11-19 09:00 web1 failed more often than db1.", "present", 0},
		{"Since 2026-11-01 web1 failed more often.", "absent", 1},
		{"web7 looks worse than web1.", "absent", 1},
		{"Two retries on web1 then success.", "absent", 1},
	} {
		in := map[string]any{"text": tc.text}
		for k, v := range base {
			in[k] = v
		}
		r, err := b.c.TestFunctionEval(ctx, FunctionEvalRequest{Function: "reporter.number_check", Input: in})
		if err != nil {
			t.Fatal(err)
		}
		if r.Output["paragraph"] != tc.paragraph || r.Output["numbers_unmatched"] != tc.unmatched {
			t.Errorf("%q: %v, want %s with %d unmatched", tc.text, r.Output, tc.paragraph, tc.unmatched)
		}
	}
}

// function.eval schedule.check is the check of a schedule change before "Accept" (TIM-5), for the
// cron and the time zone (a Schedule has no pause in this build), the rows of
// fx-time-schedule-proposal-check: 30 2 * * * in Europe/Berlin repeats on 2026-10-25 and
// disappears on 2027-03-28, with the button; the same for a makes_backup type is
// schedule_time_invalid without it; a zone outside the IANA database and CRON_TZ get no button.
func TestBenchScheduleCheck(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	eval := func(in map[string]any) map[string]any {
		t.Helper()
		in["at"] = "2026-10-13T09:00:00Z"
		r, err := b.c.TestFunctionEval(ctx, FunctionEvalRequest{Function: "schedule.check", Input: in})
		if err != nil {
			t.Fatal(err)
		}
		return r.Output
	}
	out := eval(map[string]any{"cron": "30 2 * * *", "timezone": "Europe/Berlin"})
	if out["accept_button"] != true || strings.Join(strs(out["times_disappear"].([]any)), ",") != "2027-03-28 02:30" ||
		strings.Join(strs(out["times_repeat"].([]any)), ",") != "2026-10-25 02:30" {
		t.Fatalf("Berlin 02:30: %v", out)
	}
	if out := eval(map[string]any{"cron": "15 6 * * *", "timezone": "Europe/Berlin"}); out["accept_button"] != true || len(out["times_disappear"].([]any)) != 0 {
		t.Fatalf("Berlin 06:15: %v", out)
	}
	if out := eval(map[string]any{"cron": "30 2 * * *", "timezone": "Europe/Berlin", "makes_backup": true}); out["accept_button"] != false || out["reason"] != "schedule_time_invalid" {
		t.Fatalf("a makes_backup schedule that loses a slot: %v", out)
	}
	for _, in := range []map[string]any{{"cron": "30 2 * * *", "timezone": "Mars/Olympus"}, {"cron": "CRON_TZ=UTC 30 2 * * *", "timezone": "Europe/Berlin"}} {
		if out := eval(in); out["accept_button"] != false {
			t.Fatalf("%v: %v", in, out)
		}
	}
	// commission.accuracy reads the store; it is not built as a pure function.
	if _, err := b.c.TestFunctionEval(ctx, FunctionEvalRequest{Function: "commission.accuracy"}); problemReason(err) != "not_found" {
		t.Fatalf("commission.accuracy: %v", err)
	}
}

// store.snapshot and store.restore take the real restore path (RET-6, APR-9): after a restore
// from a backup taken while an approval waited, the epoch grows past the running one, the old
// lease token is fenced, the approval is canceled(restored), an erasure request made after the
// backup is replayed from the separate erasure log, the farm chain of the new epoch opens with
// the restore record, and P1 is not ready until restore.complete.
func TestBenchStoreRestore(t *testing.T) {
	ctx := context.Background()
	b := newActionBench(t, nil)
	if _, err := b.c.TestStoreRestore(ctx, "none"); problemReason(err) != "not_found" {
		t.Fatalf("a restore without a backup: %v", err)
	}
	att, h := b.attemptTools(t, "1", false)
	pend := b.request(t, h, map[string]any{"action": "fix_web", "args": map[string]any{"host": "web1"}})
	b.finish(t, att)
	if _, err := b.c.TestStoreSnapshot(ctx, "store-daily"); err != nil {
		t.Fatal(err)
	}
	att2, _ := b.attemptTools(t, "2", false)
	er, err := b.c.ErasureRequest(ctx, b.op, ErasureRequestInput{Scope: ErasureScope{Tenant: "demo", Subject: b.alice}, Reason: "manual"})
	if err != nil {
		t.Fatal(err)
	}
	epoch := b.c.Epoch()
	if _, err := b.c.TestStoreRestore(ctx, "store-daily"); err != nil {
		t.Fatal(err)
	}
	if got := b.c.Epoch(); got != epoch+1 {
		t.Fatalf("epoch %d -> %d", epoch, got)
	}
	if _, err := b.c.Heartbeat(ctx, b.agent, header(t, att2), HeartbeatRequest{}); problemReason(err) != "fenced" {
		t.Fatalf("a lease token of the previous epoch: %v", err)
	}
	if s := b.approval(t, pend.ApprovalID); s.State != "canceled" || s.Reason != "restored" {
		t.Fatalf("pending approval after a restore: %s %s", s.State, s.Reason)
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM erasure_requests WHERE id = $1`, er.ID).Scan(&state); err != nil || state != "requested" {
		t.Fatalf("the erasure request after the backup: %q %v", state, err)
	}
	var restores int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE tenant = 'farm' AND epoch = $1 AND record->>'kind' = 'restore'`, epoch+1).Scan(&restores); err != nil || restores != 1 {
		t.Fatalf("restore records in the farm chain: %d %v", restores, err)
	}
	if err := b.c.P1Ready("claim"); problemReason(err) != "not_ready" {
		t.Fatalf("P1 after a restore: %v", err)
	}
	if v, err := b.c.AuditVerify(ctx, "farm", nil); err != nil || !v.OK {
		t.Fatalf("audit.verify of farm after a restore: %+v %v", v, err)
	}
	// The replayed request erases again what the backup brought back (RET-7): the bodies of
	// the person's tasks are gone after one pass of the erasure loop.
	bodies := func() int {
		t.Helper()
		var n int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = 'demo' AND initiator = $1 AND body IS NOT NULL`, b.alice).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if bodies() == 0 {
		t.Fatal("the backup brought back no task body of the person")
	}
	b.tick(t, "erasure")
	if n := bodies(); n != 0 {
		t.Fatalf("%d task bodies of the erased person after the erasure loop", n)
	}
}

// parentsFirst orders the tables of a restore so a row's parent is inserted before it; a
// table that references itself does not block the order (its rows go in one statement).
func TestParentsFirst(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	if _, err := b.c.st.Pool.Exec(ctx, `CREATE TABLE bench_tree (id int PRIMARY KEY, parent int REFERENCES bench_tree(id))`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = b.c.st.Pool.Exec(context.Background(), `DROP TABLE IF EXISTS bench_tree`) })
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO bench_tree VALUES (2, NULL), (1, 2)`); err != nil {
		t.Fatal(err)
	}
	order, err := parentsFirst(ctx, b.c.st.Pool, []string{"campaign_items", "bench_tree", "campaigns"})
	if err != nil || len(order) != 3 || slices.Index(order, "campaigns") > slices.Index(order, "campaign_items") {
		t.Fatalf("order %v %v", order, err)
	}
	if _, err := b.c.TestStoreSnapshot(ctx, "tree"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM bench_tree`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TestStoreRestore(ctx, "tree"); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM bench_tree WHERE parent IS NULL OR parent IN (SELECT id FROM bench_tree)`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows of a self-referencing table after a restore: %d %v", n, err)
	}
}

// process.stop and store.fault (APR-9, OBS-8, CAP-5): a killed core starts again over the same
// store with its epoch; an unavailable store makes P1 not ready and every transaction fail until
// clear; before_commit rolls back the next transaction; low_space below the hard threshold makes
// claim not ready while complete still passes; event.inject submits an activity by the path of
// submit_activity of the named channel.
func TestBenchProcessAndFaults(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	var stop ProcessStopRequest
	stop.Process.Kind, stop.Mode = "core", "kill"
	epoch := b.c.Epoch()
	if _, err := b.c.TestProcessStop(ctx, stop); err != nil || b.c.Epoch() != epoch {
		t.Fatalf("core kill: %v, epoch %d -> %d", err, epoch, b.c.Epoch())
	}
	stop.Process.Kind, stop.Process.WorkerID = "worker", b.agent.ID
	if _, err := b.c.TestProcessStop(ctx, stop); err != nil {
		t.Fatal(err)
	}
	stop.Process.WorkerID = "w_" + strings.Repeat("0", 26)
	if _, err := b.c.TestProcessStop(ctx, stop); problemReason(err) != "not_found" {
		t.Fatalf("a worker that is not enrolled: %v", err)
	}

	if _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "unavailable"}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.P1Ready("claim"); problemReason(err) != "not_ready" {
		t.Fatalf("P1 with the store unavailable: %v", err)
	}
	if _, err := b.c.TestClockAdvance(ctx, 1); !errors.Is(err, store.ErrFaultUnavailable) {
		t.Fatalf("a transaction with the store unavailable: %v", err)
	}
	if _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "clear"}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.P1Ready("claim"); err != nil {
		t.Fatalf("P1 after clear: %v", err)
	}
	if _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "before_commit"}); err != nil {
		t.Fatal(err)
	}
	before := b.now(t)
	if _, err := b.c.TestClockAdvance(ctx, 3600); !errors.Is(err, store.ErrFaultCommit) {
		t.Fatalf("a transaction after before_commit: %v", err)
	}
	if after := b.now(t); after.Sub(before) > time.Minute {
		t.Fatal("the failed commit moved the clock")
	}
	if _, err := b.c.TestClockAdvance(ctx, 1); err != nil {
		t.Fatalf("the transaction after the failed one: %v", err)
	}
	if _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "low_space"}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.P1Ready("claim"); problemReason(err) != "not_ready" {
		t.Fatalf("claim below the hard threshold: %v", err)
	}
	if err := b.c.P1Ready("complete"); err != nil {
		t.Fatalf("complete below the hard threshold: %v", err)
	}
	if _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "clear"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TestStoreFault(ctx, StoreFaultRequest{Point: "melt"}); problemReason(err) != "input_schema_invalid" {
		t.Fatalf("an unknown point: %v", err)
	}

	ev := map[string]any{"kind": "message", "channel": "telegram", "update_id": "inj-1",
		"sender": map[string]any{"identity": "telegram:1001", "verified": true},
		"chat":   map[string]any{"kind": "direct", "external_id": "1001"}, "text": "/hello hi"}
	r, err := b.c.TestEventInject(ctx, EventInjectRequest{Event: ev, Via: b.channel.ID})
	if err != nil || r.Result.Result != "task" {
		t.Fatalf("event.inject: %+v %v", r, err)
	}
	// The agent is not a channel: the path of submit_activity refuses it the same way.
	if _, err := b.c.TestEventInject(ctx, EventInjectRequest{Event: ev, Via: b.agent.ID}); problemReason(err) != "forbidden" {
		t.Fatalf("event.inject through an agent: %v", err)
	}
}
