// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// helloDone runs a hello task to its result: the notices are delivered, the attempt answers
// with answer, and the task's mark is set to mark first when given.
func (b *bench) helloDone(t *testing.T, update, mark, answer string) string {
	t.Helper()
	ctx := context.Background()
	task := b.hello(t, update)
	for {
		w := b.claim(t, b.channel, "delivery")
		if w == nil {
			break
		}
		if _, err := b.c.Complete(ctx, b.channel, header(t, w), CompleteRequest{Receipt: &Receipt{ExternalID: "n" + update}}); err != nil {
			t.Fatal(err)
		}
	}
	// The AI notice counts as delivered, as the channel driver confirms it.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE contacts SET ai_notice_at = now() WHERE ai_notice_at IS NULL`); err != nil {
		t.Fatal(err)
	}
	if mark != "" {
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('data_mark', $2::text) WHERE id = $1`, task, mark); err != nil {
			t.Fatal(err)
		}
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil {
		t.Fatal("no attempt")
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": answer}}); err != nil {
		t.Fatal(err)
	}
	return task
}

func deliveryStates(t *testing.T, b *bench, task string) map[string]string {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT dataorigin || ':' || coalesce(body->>'text_key', 'text'), state || ':' || coalesce(reason, '')
		FROM deliveries WHERE task = $1`, task)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	return out
}

// The ceiling of a channel comes from the privacy block by driver: its ceiling, internal for a
// third-party store without one, none for a store of the tenant, and internal for a driver the
// block does not list (SEC-9).
func TestChannelCeilingOf(t *testing.T) {
	tv := &TenantView{Channels: map[string]manifest.Channel{"telegram": {Driver: "telegram"}, "mail": {Driver: "email"}, "own": {Driver: "matrix"}}}
	tv.Spec.Privacy.Channels = []manifest.PrivacyChannel{{Driver: "telegram", ThirdPartyStorage: true}, {Driver: "matrix"}}
	for ch, want := range map[string]string{"telegram": "internal", "mail": "internal", "own": ""} {
		if got := channelCeiling(tv, ch); got != want {
			t.Errorf("%s: ceiling %q, want %q", ch, got, want)
		}
	}
	tv.Spec.Privacy.Channels[0].Ceiling = "raw_logs"
	if got := channelCeiling(tv, "telegram"); got != "raw_logs" {
		t.Errorf("raised ceiling %q", got)
	}
}

// An answer above the channel ceiling is withheld and the person gets the code text in its
// place (SEC-9); an answer within it goes.
func TestChannelCeilingWithholds(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	// The group of alice may read internal data, the chat may receive summaries only.
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		switch m["kind"] {
		case "Group":
			m["spec"].(map[string]any)["data"] = []any{"summary", "internal"}
		case "Tenant":
			privacy := m["spec"].(map[string]any)["privacy"].(map[string]any)
			privacy["channels"] = []any{map[string]any{"driver": "telegram", "third_party_storage": true, "ceiling": "summary"}}
		}
	}
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "2222222222222222222222222222222222222222", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply: %+v %v", res, err)
	}
	task := b.helloDone(t, "1", "internal", "error at 10:31 in nginx")
	if w := b.claim(t, b.channel, "delivery"); w == nil || w.Payload["delivery"].(map[string]any)["body"].(map[string]any)["text_key"] != "delivery.withheld.channel_ceiling" {
		t.Fatalf("the code text in place of the answer: %+v", w)
	}
	if got := deliveryStates(t, b, task)["model:text"]; got != "withheld:channel_ceiling" {
		t.Fatalf("answer above the ceiling: %q", got)
	}
	if n := countEvents(t, b, "delivery.withheld", ""); n != 1 {
		t.Fatalf("withheld events %d", n)
	}
	task = b.helloDone(t, "2", "", "Hello!")
	if w := b.claim(t, b.channel, "delivery"); w == nil || w.Payload["delivery"].(map[string]any)["kind"] != "task_result" {
		t.Fatalf("an answer within the ceiling: %+v", w)
	}
	if got := deliveryStates(t, b, task)["model:text"]; got != "sending:" {
		t.Fatalf("answer within the ceiling: %q", got)
	}
}

// Model text with a secret is not sent and raises an alarm without content (INJ-11); the second
// case of one role within an hour stops the deliveries of the role until a person lifts the
// hold (HLD-6).
func TestSecretInOutbound(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	secret := "use password=hunter22 to log in"
	task := b.helloDone(t, "1", "", secret)
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("a message with a secret was handed out: %v", w.Payload)
	}
	if got := deliveryStates(t, b, task)["model:text"]; got != "withheld:secret_in_outbound" {
		t.Fatalf("secret: %q", got)
	}
	if n := countEvents(t, b, "alarm.outbound.secret.detected.v1", "outbound.secret.detected:hello"); n != 1 {
		t.Fatalf("alarm events %d", n)
	}
	var holds int
	count := func() {
		t.Helper()
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE state = 'active' AND reason = 'secret_in_outbound'
			AND scope_kind = 'role' AND scope_name = 'hello' AND until IS NULL AND 'no_delivery' = ANY(effects)`).Scan(&holds); err != nil {
			t.Fatal(err)
		}
	}
	if count(); holds != 0 {
		t.Fatal("a hold after one case")
	}
	b.helloDone(t, "2", "", secret)
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("the second secret was handed out: %v", w.Payload)
	}
	if count(); holds != 1 {
		t.Fatalf("holds after two cases: %d", holds)
	}
	if n := countEvents(t, b, "alarm.hold.auto.v1", "hold.auto:hello"); n != 1 {
		t.Fatalf("hold alarm events %d", n)
	}
	// The next message of the role, clean as it is, waits for a person.
	task = b.helloDone(t, "3", "", "Hello!")
	if w := b.claim(t, b.channel, "delivery"); w != nil {
		t.Fatalf("a delivery of the stopped role: %v", w.Payload)
	}
	if got := deliveryStates(t, b, task)["model:text"]; got != "withheld:held" {
		t.Fatalf("under the hold: %q", got)
	}
}

// A forwarded or quoted body makes the task untrusted and marks personal data seen (INJ-13,
// AUD-5).
func TestForwardQuoteFlags(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		forward, quote  bool
		source, persons string
	}{{true, false, "forward", `["forward"]`}, {false, true, "quote", `["quote"]`}, {true, true, "forward", `["forward", "quote"]`}, {false, false, "", ""}} {
		var task string
		err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			res, err := b.c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: "demo", Type: "hello.greet.v1", Body: map[string]any{"type": "hello.greet.v1", "goal": "hi"},
				IdempotencyKey: fmt.Sprintf("fwd-%d", i), Initiator: b.alice, EnqueuedBy: "channel:telegram", Source: "interactive", Forwarded: tc.forward, Quoted: tc.quote})
			if err == nil && res.Rejected != nil {
				t.Fatalf("rejected %+v", res.Rejected)
			}
			if res != nil {
				task = res.TaskID
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
		var seen bool
		var source, persons string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT (record->>'untrusted_seen')::bool, coalesce(record->>'untrusted_source', ''), coalesce(record->>'personal_seen', '')
			FROM tasks WHERE id = $1`, task).Scan(&seen, &source, &persons); err != nil {
			t.Fatal(err)
		}
		if seen != (tc.source != "") || source != tc.source || persons != tc.persons {
			t.Fatalf("case %d: untrusted %v %q, personal %q", i, seen, source, persons)
		}
	}
}

// An address the toolhost masked marks personal data seen on the task once, and the completion
// audit carries the source without content (AUD-5).
func TestMaskingMarksPersonalSeen(t *testing.T) {
	ctx := context.Background()
	b := newToolBench(t)
	att, h := b.attemptTools(t, "1", true)
	th := b.toolhost(t, "logs")
	for range 2 {
		done := callAsync(b, h, mcp.CallParams{Name: "logs_tail", Arguments: map[string]any{"source": "nginx-error"}})
		item := b.claimTool(t, th)
		out := map[string]any{"content": []any{map[string]any{"type": "text", "text": "[ip] GET / 404"}}}
		if _, err := b.c.Complete(ctx, th, header(t, item), CompleteRequest{Output: out}); err != nil {
			t.Fatal(err)
		}
		<-done
	}
	var persons string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT coalesce(record->>'personal_seen', '') FROM tasks WHERE id = $1`, att.WorkID).Scan(&persons); err != nil || persons != `["masking"]` {
		t.Fatalf("personal_seen %q %v", persons, err)
	}
	if _, err := b.c.Complete(ctx, b.agent, header(t, att), CompleteRequest{Result: map[string]any{"answer": "done"}}); err != nil {
		t.Fatal(err)
	}
	var audit string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'personal_seen' FROM audit WHERE record->>'command' = 'task.complete'`).Scan(&audit); err != nil || audit != `["masking"]` {
		t.Fatalf("audit marker %q %v", audit, err)
	}
}
