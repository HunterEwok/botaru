// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// A task ends in a terminal state with an event; a monitor_script delivery whose lease expires
// without an outcome ends dead with exec_unknown, is not handed out again, and the model gets
// exec_unknown (DLV-1). The run carries the service_grant number as its work id (FEN-7).
func TestBackfillTerminalWithEvent(t *testing.T) {
	ctx := context.Background()
	b := newScriptBench(t)
	// A task past its deadline ends failed, with its event.
	failed := b.hello(t, "9")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET deadline_at = botaru_now() - interval '1 second' WHERE id = $1`, failed); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "deadline")
	// DLV-1: the terminal state comes with its event.
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'failed'`, failed); n != 1 {
		t.Fatal("the task did not end at its deadline")
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE subject = $1 AND type = 'task.failed'`, failed); n != 1 {
		t.Fatalf("task.failed events %d", n)
	}

	b.scripts(t, "hash-0001", map[string]any{"scriptid": "21", "input": "list", "choices": []any{"static", "all"}})
	_, h := b.attemptTools(t, "1", true)
	ch := callAsync(b.bench, h, mcp.CallParams{Name: scriptToolName, Arguments: map[string]any{"resource": "web1", "script": "cache-clear", "input": "static"}})
	waitScript(t, b.bench)
	var item *WorkItem
	for range 50 {
		if item = b.claim(t, b.mon, "delivery"); item == nil || dbody(item)["scriptid"] != nil {
			break
		}
		if _, err := b.c.Complete(ctx, b.mon, header(t, item), CompleteRequest{Receipt: &Receipt{ExternalID: "x"}}); err != nil {
			t.Fatal(err)
		}
	}
	if item == nil {
		t.Fatal("no monitor_script delivery")
	}
	// FEN-7: the run carries the number of its service_grant record as the work id every
	// attempt shares; the record names the bot's task, whose initiator is alice.
	grant := asString(dbody(item)["grant_id"])
	if n := b.count(t, `SELECT count(*) FROM audit a JOIN tasks t ON t.id = a.record->'service_grant'->'author'->>'task'
		WHERE a.record->>'kind' = 'service_grant' AND a.record->'target'->>'id' = $1 AND t.initiator = $2`, grant, b.alice); grant == "" || n != 1 {
		t.Fatalf("grant %q: %d service_grant records of the initiator's task", grant, n)
	}
	// FEN-7: a sink or channel writes idempotently by the delivery id it is given.
	if item.Payload["delivery"].(map[string]any)["id"] != item.WorkID {
		t.Fatalf("delivery id %v, work %s", item.Payload["delivery"], item.WorkID)
	}
	// The adapter takes the run and goes silent past the lease.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE leases SET lease_until = botaru_now() - interval '1 second' WHERE work_id = $1 AND state = 'active'`, item.WorkID); err != nil {
		t.Fatal(err)
	}
	b.loop(t, "reaper")
	// DLV-1: dead with exec_unknown, with its event, and not handed out again.
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE id = $1 AND state = 'dead' AND reason = 'exec_unknown'`, item.WorkID); n != 1 {
		var st, r string
		_ = b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM deliveries WHERE id = $1`, item.WorkID).Scan(&st, &r)
		t.Fatalf("the script delivery is %s(%s)", st, r)
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE subject = $1 AND type = 'delivery.dead'`, item.WorkID); n != 1 {
		t.Fatalf("delivery.dead events %d", n)
	}
	for again := b.claim(t, b.mon, "delivery"); again != nil; again = b.claim(t, b.mon, "delivery") {
		if again.WorkID == item.WorkID {
			t.Fatal("the script delivery was handed out again")
		}
		if _, err := b.c.Complete(ctx, b.mon, header(t, again), CompleteRequest{Receipt: &Receipt{ExternalID: "y"}}); err != nil {
			t.Fatal(err)
		}
	}
	got := recv(t, ch)
	if got.err != nil {
		t.Fatal(got.err)
	}
	if text := callText(got.r); codeOf(text) != "exec_unknown" {
		t.Fatalf("the model got %s", text)
	}
}

// codeOf reads the code field of a JSON tool answer.
func codeOf(text string) string {
	var out struct {
		Code string `json:"code"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	return out.Code
}

// A task's transition, its event, the deliveries it produced and its audit record are written
// in one transaction: when one of them cannot be written, none is; when they are, they carry
// one store time (DLV-2).
func TestBackfillTransitionOneTransaction(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	b.drain(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	att := claimFor(t, b, b.agent, task)
	for _, sql := range []string{
		`CREATE FUNCTION backfill_no_result() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'no result delivery'; END $$`,
		`CREATE TRIGGER backfill_no_result BEFORE INSERT ON deliveries FOR EACH ROW WHEN (NEW.kind = 'task_result') EXECUTE FUNCTION backfill_no_result()`,
	} {
		if _, err := b.c.st.Pool.Exec(ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	done := CompleteRequest{Result: map[string]any{"answer": "Hello!"}}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), done); err == nil {
		t.Fatal("complete passed without its delivery")
	}
	// DLV-2: nothing of the transition is written without the delivery.
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'working'`, task); n != 1 {
		t.Fatal("the task moved without its delivery")
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE subject = $1 AND type = 'task.completed'`, task); n != 0 {
		t.Fatal("the event was written without the delivery")
	}
	if n := b.count(t, `SELECT count(*) FROM audit WHERE record->>'command' = 'task.complete' AND record->'target'->>'id' = $1`, task); n != 0 {
		t.Fatal("the audit record was written without the delivery")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DROP TRIGGER backfill_no_result ON deliveries`); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), done); err != nil {
		t.Fatal(err)
	}
	// DLV-2: the transition, its event and its delivery share one store time.
	var updated, event, delivery time.Time
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT t.updated_at, e.time, d.created_at FROM tasks t
		JOIN events e ON e.subject = t.id AND e.type = 'task.completed'
		JOIN deliveries d ON d.task = t.id AND d.kind = 'task_result' WHERE t.id = $1`, task).Scan(&updated, &event, &delivery); err != nil {
		t.Fatal(err)
	}
	if !updated.Equal(event) || !updated.Equal(delivery) {
		t.Fatalf("times %v %v %v", updated, event, delivery)
	}
}

// A delivery that fails is retried on its own leases with a pause and ends dead with its event;
// the task it came from stays completed and is not restarted (DLV-3).
func TestBackfillDeliveryFailureLeavesTask(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.helloDone(t, "1", "", "Hello!")
	var leases []int
	for i := 0; ; i++ {
		if i > 2*b.c.cfg.DeliveryMaxAttempts {
			t.Fatal("the delivery never ended")
		}
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE deliveries SET run_at = botaru_now() WHERE task = $1 AND kind = 'task_result' AND state = 'planned'`, task); err != nil {
			t.Fatal(err)
		}
		w := b.claim(t, b.channel, "delivery")
		if w == nil {
			break
		}
		leases = append(leases, w.LeaseNo)
		r, err := b.c.Fail(ctx, b.channel, header(t, w), FailRequest{Reason: "plugin_unavailable", Retryable: true})
		if err != nil {
			t.Fatal(err)
		}
		// DLV-3: the task is not touched by the delivery's failure.
		if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'completed'`, task); n != 1 {
			t.Fatalf("after failure %d the task left completed", i)
		}
		if r.Final {
			break
		}
		if r.RetryAt == "" {
			t.Fatalf("failure %d: no pause before the retry", i)
		}
	}
	// DLV-3: its own leases, numbered on, and its own terminal outcome with the event.
	if len(leases) != b.c.cfg.DeliveryMaxAttempts || leases[len(leases)-1] != len(leases) {
		t.Fatalf("leases %v, ceiling %d", leases, b.c.cfg.DeliveryMaxAttempts)
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE task = $1 AND kind = 'task_result' AND state = 'dead' AND reason = 'attempts_exhausted'`, task); n != 1 {
		t.Fatal("the delivery is not dead")
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE type = 'delivery.dead' AND subject IN (SELECT id FROM deliveries WHERE task = $1)`, task); n != 1 {
		t.Fatal("no delivery.dead event")
	}
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND state = 'completed' AND attempts_spent = 0`, task); n != 1 {
		t.Fatal("the task was restarted")
	}
}

// Model text reaches a person only after complete, whole: heartbeats with a stage and a
// percentage send nothing to the chat, the answer goes as one delivery after the full result
// check, and a result outside the type's schema sends nothing (DLV-11).
func TestBackfillNoTextBeforeComplete(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	b.drain(t)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now()`); err != nil {
		t.Fatal(err)
	}
	att := claimFor(t, b, b.agent, task)
	h := header(t, att)
	for _, p := range []float64{10, 50, 90} {
		if _, err := b.c.Heartbeat(ctx, b.agent, h, HeartbeatRequest{Stage: "writing_answer", Progress: &p}); err != nil {
			t.Fatal(err)
		}
	}
	// DLV-11: nothing goes to the person while the attempt runs.
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE task = $1`, task); n != 0 {
		t.Fatalf("%d deliveries before complete", n)
	}
	// DLV-11: a result outside the full schema sends nothing.
	if _, err := b.c.Complete(ctx, b.agent, h, CompleteRequest{Result: map[string]any{"answer": "Hello!", "extra": "x"}}); err == nil {
		t.Fatal("a result outside the schema passed")
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE task = $1 AND dataorigin = 'model'`, task); n != 0 {
		t.Fatalf("%d model deliveries after a refused result", n)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET run_at = botaru_now() WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	att = claimFor(t, b, b.agent, task)
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello, alice!"}}); err != nil {
		t.Fatal(err)
	}
	// DLV-11: one whole answer after complete, no edit of a message as it is generated.
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE task = $1 AND dataorigin = 'model' AND body->>'text' = 'Hello, alice!'`, task); n != 1 {
		t.Fatalf("model deliveries %d", n)
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE task = $1 AND kind = 'card_update'`, task); n != 0 {
		t.Fatal("a message was edited")
	}
}

// Every delivery carries the origin mark the core sets: code for the notices and code texts,
// model for the model's answer, on the record and in the work the channel gets (DLV-13).
func TestBackfillDataOrigin(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	seen := map[string]string{}
	take := func() {
		for w := b.claim(t, b.channel, "delivery"); w != nil; w = b.claim(t, b.channel, "delivery") {
			d := w.Payload["delivery"].(map[string]any)
			seen[asString(d["kind"])] = asString(d["dataorigin"])
			if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "x" + w.WorkID}}); err != nil {
				t.Fatal(err)
			}
		}
	}
	take()
	att := claimFor(t, b, b.agent, task)
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "Hello!"}}); err != nil {
		t.Fatal(err)
	}
	take()
	// DLV-13: the mark on the work for the channel.
	if seen["ai_notice"] != "code" || seen["privacy_notice"] != "code" || seen["task_result"] != "model" {
		t.Fatalf("marks on the work %v", seen)
	}
	// DLV-13: the delivery record stores the mark.
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE task = $1 AND kind = 'task_result' AND dataorigin = 'model'`, task); n != 1 {
		t.Fatal("the record of the answer has no model mark")
	}
	if n := b.count(t, `SELECT count(*) FROM deliveries WHERE dataorigin IS NULL OR dataorigin NOT IN ('model', 'code')`); n != 0 {
		t.Fatalf("%d deliveries without a mark", n)
	}
}

// No record written from model output has a person as its subject: the lint refuses a result
// field read by code that names the kind human, and a proposal about a person is not created
// (DLV-14).
func TestBackfillNoHumanSubject(t *testing.T) {
	c := lintCore(t)
	typ := func(enum string) string {
		return `apiVersion: botaru/v1alpha1
kind: TaskType
metadata: {name: ops.rate.v1}
spec:
  purpose: work
  worker_kind: agent
  result_schema:
    type: object
    additionalProperties: false
    properties:
      subject: {type: string, enum: [` + enum + `], x-botaru-read-by: code}
`
	}
	// DLV-14: the schema lint of records read by code.
	if got := lintCodes(t, c, typ("human:alice, host:web1")); !slices.Contains(got, "human_subject_in_record") {
		t.Fatalf("a human subject: %v", got)
	}
	if got := lintCodes(t, c, typ("host:web1")); slices.Contains(got, "human_subject_in_record") {
		t.Fatalf("a host subject: %v", got)
	}
	b := newInsightBench(t, nil)
	b.helloDone(t, "1", "", "hi")
	b.loop(t, "observe")
	body := noneBody("slow_person", 1)
	body["problem"].(map[string]any)["subjects"] = []any{"human:" + b.alice}
	if _, err := b.analysis(t, "1", false, map[string]any{"proposals": []any{body}}); err != nil {
		t.Fatal(err)
	}
	// DLV-14: no proposal names a person.
	if recs := proposals(t, b); len(recs) != 0 {
		t.Fatalf("a proposal about a person: %v", recs)
	}
	if len(rejections(t, b)) != 1 {
		t.Fatalf("rejections %v", rejections(t, b))
	}
}
