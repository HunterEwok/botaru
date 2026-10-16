// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/testbench"
)

// retentionSet edits the tenant of the hello set to the given periods (RET-9).
func retentionSet(ret map[string]any) func(root string, ms []map[string]any) []map[string]any {
	return func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "Tenant" {
				m["spec"].(map[string]any)["retention"] = ret
			}
		}
		return ms
	}
}

func (b *bench) storeNow(t *testing.T) time.Time {
	t.Helper()
	var now time.Time
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT botaru_now()`).Scan(&now); err != nil {
		t.Fatal(err)
	}
	return now
}

func (b *bench) count(t *testing.T, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := b.c.st.Pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// The loop deletes by the tenant's periods in every core copy, without an erasure request
// (RET-9, RET-10, blackbox:core/retention-all-copies): a shorter content period reaches a task,
// a session and a file already stored; a memory period reaches a person's record and a sealed
// fact of a retired identity before its seal ends, and the open offer no longer lists that fact
// (MEM-10, blackbox:identity/seal-retention-earlier); metadata of the same age stays; and what a
// restore brings back past its period is deleted again at the next run.
func TestRetentionTenantPeriods(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, retentionSet(map[string]any{"classes": map[string]any{"content": "2d", "memory": "30d"}, "files": "1d"}))
	task := b.helloDone(t, "1", "", "CANARY-NOT-A-SECRET-ret10")
	now := b.storeNow(t)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO sessions (id, tenant, role, conversation, subjects, created_at, updated_at) VALUES ('s_ret10', 'demo', 'hello', 'c1', ARRAY[$1], $2, $2)`, b.alice, now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO session_entries (session, uuid, content, created_at) VALUES ('s_ret10', 'u1', '"CANARY-NOT-A-SECRET-ret10"', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO memory_items (id, tenant, scope, subject, role, key, content, content_hash, state, nonce, created_at, updated_at)
		VALUES ('m_ret10', 'demo', 'person', $1, 'hello', 'k', 'CANARY-NOT-A-SECRET-ret10', 'h', 'active', 'n', $2, $2)`, b.alice, now); err != nil {
		t.Fatal(err)
	}
	// A retired identity's fact under a seal that runs past the memory period, offered to a
	// successor that has not come yet.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO facts (id, tenant, bot, kind, predicate, subject, value, window_from, window_to, state, untrusted_origin, data_mark, verified_at, created_at)
		VALUES ('fact_ret10', 'demo', 'bot_old', 'observation', 'p', '{}', '1', $1, $1, 'sealed', false, 'summary', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO fact_offers (id, tenant, from_bot, role, facts, state, recipient_state, open_until, created_at)
		VALUES ('fo_ret10', 'demo', 'bot_old', 'hello', ARRAY['fact_ret10'], 'open', 'waiting', $2, $1)`, now, now.Add(60*day)); err != nil {
		t.Fatal(err)
	}
	// The content parts of orchestration records (RET-10): a finished script run and its
	// step, the items of a finished campaign, a finding's note, a closed proposal's note, a digest.
	for _, sql := range []string{
		`INSERT INTO script_runs (id, tenant, initiator, state, plan_hash, record, nonce, expires_at, created_at, updated_at)
			VALUES ('scr_ret10', 'demo', 'x', 'completed', 'h', '{"root": "t_x", "assignment": "CANARY-NOT-A-SECRET-ret10"}', 'n', $1, $1, $1)`,
		`INSERT INTO script_steps (run, n, id, kind, state, fields, untrusted) VALUES ('scr_ret10', 1, 's1', 'task', 'ok', '{"v": "CANARY-NOT-A-SECRET-ret10"}', '{"value": true}')`,
		`INSERT INTO campaigns (id, tenant, initiator, state, plan_hash, record, nonce, card, created_at, updated_at)
			VALUES ('cmp_ret10', 'demo', 'x', 'completed', 'h', '{}', 'n', false, $1, $1)`,
		`INSERT INTO campaign_items (campaign, n, resource, args, args_hash, state, summary) VALUES ('cmp_ret10', 1, 'r', '{"a": "CANARY-NOT-A-SECRET-ret10"}', 'h', 'ok', '{"s": 1}')`,
		`INSERT INTO findings (id, tenant, task, kind, subject_kind, subject_id, body, note, created_at)
			VALUES ('fd_ret10', 'demo', 't_x', 'k', 'task', 't_x', '{}', 'CANARY-NOT-A-SECRET-ret10', $1)`,
		`INSERT INTO proposals (id, tenant, role, worker, task, fingerprint, kind, state, record, created_at, updated_at, state_at)
			VALUES ('prp_ret10', 'demo', 'hello', 'w', 't_x', 'f', 'k', 'declined', '{"body": {"note": "CANARY-NOT-A-SECRET-ret10"}}', $1, $1, $1)`,
		`INSERT INTO proposal_digests (id, tenant, slot_at, urgent, body, created_at) VALUES ('dg_ret10', 'demo', $1, false, '{"x": "CANARY-NOT-A-SECRET-ret10"}', $1)`,
	} {
		args := []any{now}
		if !strings.Contains(sql, "$1") {
			args = nil
		}
		if _, err := b.c.st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	events := b.count(t, `SELECT count(*) FROM events WHERE subject = $1`, task)
	b.advance(t, 3*24*3600)
	b.loop(t, "retention")
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE id = $1 AND (body IS NOT NULL OR result IS NOT NULL)`, task); n != 0 {
		t.Fatal("the task kept its content past the tenant's content period")
	}
	if n := b.count(t, `SELECT count(*) FROM session_entries WHERE content::text LIKE '%CANARY%'`); n != 0 {
		t.Fatal("the session outlived the content period")
	}
	if n := b.count(t, `SELECT (SELECT count(*) FROM script_runs WHERE record::text LIKE '%CANARY%' OR NOT record ? 'purged')
		+ (SELECT count(*) FROM script_steps WHERE fields IS NOT NULL OR untrusted IS NOT NULL)
		+ (SELECT count(*) FROM campaign_items WHERE args <> '{}' OR summary IS NOT NULL)
		+ (SELECT count(*) FROM findings WHERE note IS NOT NULL) + (SELECT count(*) FROM proposals WHERE record::text LIKE '%CANARY%')
		+ (SELECT count(*) FROM proposal_digests)`); n != 0 {
		t.Fatalf("%d orchestration records kept their content past the period", n)
	}
	if n := b.count(t, `SELECT count(*) FROM memory_items WHERE id = 'm_ret10'`) + b.count(t, `SELECT count(*) FROM facts WHERE id = 'fact_ret10'`); n != 2 {
		t.Fatalf("memory went before its period: %d left", n)
	}
	if n := b.count(t, `SELECT count(*) FROM events WHERE subject = $1`, task); n != events {
		t.Fatalf("metadata of the same age went: %d of %d events", n, events)
	}
	b.advance(t, 28*24*3600)
	b.loop(t, "retention")
	if n := b.count(t, `SELECT count(*) FROM memory_items WHERE id = 'm_ret10'`) + b.count(t, `SELECT count(*) FROM facts WHERE id = 'fact_ret10'`); n != 0 {
		t.Fatalf("memory outlived its period under the seal: %d left", n)
	}
	if n := b.count(t, `SELECT count(*) FROM fact_offers WHERE id = 'fo_ret10' AND cardinality(facts) = 0 AND state = 'open'`); n != 1 {
		t.Fatal("the offer still lists the deleted fact")
	}
	// A restore brings the content back; the next run deletes it again.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET body = '{"goal": "CANARY-NOT-A-SECRET-ret10"}' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	b.advance(t, 3600)
	b.loop(t, "retention")
	if n := b.count(t, `SELECT count(*) FROM tasks WHERE body::text LIKE '%CANARY%'`); n != 0 {
		t.Fatal("restored content past its period stayed")
	}
}

// The months of metadata whose records are all expired, and only those (RET-10, AUD-3, D-108).
func TestExpiredMonths(t *testing.T) {
	at := func(s string) time.Time { v, _ := time.Parse(time.RFC3339, s); return v }
	last := func(sp []span) span {
		if len(sp) == 0 {
			return span{}
		}
		return sp[len(sp)-1]
	}
	// Default 400 days: on 2026-10-06 every month up to 2025-08 is past (2025-08-31 + 400 days),
	// as one span open to the past.
	sp := expiredMonths(at("2026-10-06T00:00:00Z"), nil, 400*day)
	if len(sp) != 1 || !sp[0].from.IsZero() || !sp[0].to.Equal(at("2025-09-01T00:00:00Z")) {
		t.Fatalf("spans %v", sp)
	}
	// A shorter period set on 2026-03-15 reaches only what was written after it: the month of
	// the change keeps 400 days, a later month goes by the 93 days.
	hist := []metaPeriod{{at("2026-03-15T00:00:00Z"), 93 * day}}
	sp = expiredMonths(at("2026-10-06T00:00:00Z"), hist, 93*day)
	if l := last(sp); !l.from.Equal(at("2026-04-01T00:00:00Z")) || !l.to.Equal(at("2026-07-01T00:00:00Z")) || !sp[0].to.Equal(at("2025-09-01T00:00:00Z")) {
		t.Fatalf("spans after a shorter period %v", sp)
	}
	// A longer period now holds back records written under a shorter one.
	hist = []metaPeriod{{at("2026-01-01T00:00:00Z"), 100 * day}}
	if l := last(expiredMonths(at("2026-10-06T00:00:00Z"), hist, 300*day)); !l.to.Equal(at("2025-09-01T00:00:00Z")) {
		t.Fatalf("spans under a longer period %v", l)
	}
}

// Metadata goes only in whole calendar months, and a shorter metadata period reaches only the
// records written after the apply that set it (RET-9, RET-10, D-108): events and usage lines of a
// month stay until the month is past the period, then go together; those written before the
// apply keep the longer period.
func TestMetadataWholeMonths(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	now := b.storeNow(t)
	// The clock goes to the second day of a month, so the months below are known.
	t0 := monthStart(now).AddDate(0, 2, 1)
	b.advance(t, int(t0.Sub(now)/time.Second))
	write := func(tag string) {
		t.Helper()
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			if err := b.c.appendEvent(ctx, tx, now, "demo", Event{Type: "budget.threshold", Subject: tag}); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time) VALUES ($1, 'demo', 'work', 'model_call', 'committed', 7, $2)`, ids.New(ids.Spend), now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	write("before")
	root, err := spec.FindRoot("")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := testbench.HelloBotManifests(root)
	if err != nil {
		t.Fatal(err)
	}
	ms = retentionSet(map[string]any{"classes": map[string]any{"metadata": "93d"}})(root, ms)
	if res, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: "1111111111111111111111111111111111111111", Manifests: ms}); err != nil || !res.Applied {
		t.Fatalf("apply %+v %v", res, err)
	}
	// The next month: a record under the 93 days.
	b.advance(t, 31*24*3600)
	write("after")
	t1 := b.storeNow(t)
	// Terminal records of other metadata tables in the same month, and live ones that stay.
	for _, sql := range []string{
		`INSERT INTO approvals (id, tenant, initiator, action, args_hash, state, record, nonce, expires_at, created_at, updated_at)
			VALUES ('ap_old', 'demo', 'x', 'a', 'h', 'rejected', '{}', 'n', $1, $1, $1), ('ap_live', 'demo', 'x', 'a', 'h', 'pending', '{}', 'n', $1, $1, $1)`,
		`INSERT INTO holds (id, tenant, scope_kind, effects, reason, author, author_kind, state, created_at, ended_at)
			VALUES ('h_old', 'demo', 'tenant', '{}', 'r', 'x', 'operator', 'released', $1, $1), ('h_live', 'demo', 'tenant', '{}', 'r', 'x', 'operator', 'active', $1, NULL)`,
		`INSERT INTO alarm_episodes (tenant, alarmkey, alarm, state, severity, subject, seq, opened_at, last_seen_at, resolved_at)
			VALUES ('demo', 'k_old', 'a', 'resolved', 'warn', '{}', 1, $1, $1, $1)`,
		`INSERT INTO campaigns (id, tenant, initiator, state, plan_hash, record, nonce, card, created_at, updated_at)
			VALUES ('cmp_old', 'demo', 'x', 'completed', 'h', '{}', 'n', false, $1, $1)`,
		`INSERT INTO campaign_items (campaign, n, resource, args, args_hash, state) VALUES ('cmp_old', 1, 'r', '{}', 'h', 'ok')`,
		`INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time) VALUES ('sp_stuck', 'demo', 'work', 'task', 'reserved', 7, $1)`,
	} {
		args := []any{t1}
		if !strings.Contains(sql, "$1") {
			args = nil
		}
		if _, err := b.c.st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	others := func() int {
		return b.count(t, `SELECT (SELECT count(*) FROM approvals) + (SELECT count(*) FROM holds) + (SELECT count(*) FROM alarm_episodes WHERE alarmkey = 'k_old')
			+ (SELECT count(*) FROM campaigns) + (SELECT count(*) FROM campaign_items) + (SELECT count(*) FROM spend WHERE id = 'sp_stuck')`)
	}
	if n := others(); n != 8 {
		t.Fatalf("records written: %d", n)
	}
	events := func(tag string) int {
		return b.count(t, `SELECT count(*) FROM events WHERE subject = $1`, tag)
	}
	// 93 days after the record, its month has not ended its period yet.
	b.advance(t, int(monthStart(t1).AddDate(0, 1, 93).Add(-time.Hour).Sub(t1)/time.Second))
	b.loop(t, "retention")
	if events("after") != 1 || events("before") != 1 || others() != 8 {
		t.Fatal("a month went before all of it was past the period")
	}
	b.advance(t, 2*3600)
	b.loop(t, "retention")
	if events("after") != 0 {
		t.Fatal("the month past the period stayed")
	}
	if n := b.count(t, `SELECT count(*) FROM approvals WHERE id = 'ap_live'`) + b.count(t, `SELECT count(*) FROM holds WHERE id = 'h_live'`); n != 2 || others() != 2 {
		t.Fatalf("terminal records of the month stayed or live ones went: %d of 2 left", others())
	}
	if n := b.count(t, `SELECT count(*) FROM spend WHERE tenant = 'demo' AND time >= $1 AND time < $2`, monthStart(t1), monthStart(t1).AddDate(0, 1, 0)); n != 0 {
		t.Fatalf("usage lines of the month stayed: %d", n)
	}
	if events("before") != 1 || b.count(t, `SELECT count(*) FROM spend WHERE tenant = 'demo' AND time < $1`, monthStart(t1)) == 0 {
		t.Fatal("a shorter period reached records written before its apply")
	}
}

// Audit goes as a prefix of whole months behind a cut that keeps the chain verifiable, with one
// period_deleted record; a single record is never deleted and the cut never moves back (AUD-3,
// AUD-6, RET-10).
func TestAuditMonthCut(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	before := b.count(t, `SELECT count(*) FROM audit WHERE tenant = 'demo'`)
	if before == 0 {
		t.Fatal("no audit to cut")
	}
	now := b.storeNow(t)
	b.advance(t, int(monthStart(now).AddDate(0, 1, 401).Sub(now)/time.Second))
	b.hello(t, "1")
	b.loop(t, "retention")
	var kinds []string
	rows, err := b.c.st.Pool.Query(ctx, `SELECT record->>'kind' FROM audit WHERE tenant = 'demo' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	kinds, err = pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) == 0 || len(kinds) >= before+3 || kinds[len(kinds)-1] != "period_deleted" {
		t.Fatalf("audit after the cut: %v (of %d before)", kinds, before)
	}
	var count int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (record->'period'->>'count')::int FROM audit WHERE tenant = 'demo' AND record->>'kind' = 'period_deleted'`).Scan(&count); err != nil || count < before {
		t.Fatalf("period_deleted count %d of %d %v", count, before, err)
	}
	if n, broken, err := b.c.VerifyAudit(ctx, "demo"); err != nil || broken != 0 || n == 0 {
		t.Fatalf("verify after the cut: %d %d %v", n, broken, err)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `DELETE FROM audit WHERE tenant = 'demo' AND seq = (SELECT max(seq) FROM audit WHERE tenant = 'demo')`); err == nil {
		t.Fatal("a single audit record was deleted")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE audit_cuts SET seq = 0 WHERE tenant = 'demo'`); err == nil {
		t.Fatal("the cut moved back")
	}
	// A cut written by hand that names a recent record is refused, so it cannot open recent
	// audit to a delete.
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE audit_cuts c SET seq = a.seq, hash = a.hash FROM audit a
		WHERE c.tenant = 'demo' AND a.tenant = 'demo' AND a.epoch = c.epoch AND a.seq = (SELECT max(seq) FROM audit WHERE tenant = 'demo')`); err == nil {
		t.Fatal("a cut moved onto a recent record")
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO audit_cuts (tenant, epoch, seq, hash, updated_at) SELECT tenant, epoch, seq, hash, now() FROM audit
		WHERE tenant = 'farm' ORDER BY seq DESC LIMIT 1`); err == nil {
		t.Fatal("a cut was written for a recent record")
	}
}

// A person's usage line older than 93 days keeps its amounts and loses subject, task, root and
// the other ids that lead back to the person; its time is midnight and it says subject_kind
// human. A younger line and a line of system stay as they are (BUD-16, SEC-14,
// blackbox:budget/subject-anonymized-93d).
func TestUsageAnonymized(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	now := b.storeNow(t)
	for _, l := range []struct {
		id, subject string
		age         time.Duration
	}{{"sp_old", b.alice, 94 * day}, {"sp_young", b.alice, 92 * day}, {"sp_system", "system", 94 * day}} {
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, task, root, subject, role, worker, acting_bot, line, unit, state, eur_micros, toolcall, message, time, settled_at)
			VALUES ($1, 'demo', 't_x', 't_x', $2, 'hello', 'w_x', 'bot_x', 'work', 'model_call', 'committed', 1234, 'c_x', '{"kind": "report", "size": 321}', $3, $3)`, l.id, l.subject, now.Add(-l.age)); err != nil {
			t.Fatal(err)
		}
	}
	b.loop(t, "retention")
	var id, message string
	var subject, task, root, call, worker, bot *string
	var at, settled time.Time
	var micros int64
	var role string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, subject, task, root, toolcall, worker, acting_bot, message::text, time, settled_at, eur_micros, role FROM spend WHERE subject_kind = 'human'`).Scan(
		&id, &subject, &task, &root, &call, &worker, &bot, &message, &at, &settled, &micros, &role); err != nil {
		t.Fatal(err)
	}
	if subject != nil || task != nil || root != nil || call != nil || worker != nil || bot != nil || !at.Equal(periodStart("day", at)) || !settled.Equal(at) || micros != 1234 || role != "hello" {
		t.Fatalf("old line %v %v %v %v %v %v %v %v %d %s", subject, task, root, call, worker, bot, at, settled, micros, role)
	}
	// Its new id carries no time, and the message keeps only its kind.
	if id == "sp_old" || !strings.HasPrefix(id, "sp_0000000000") || message != `{"kind": "report"}` {
		t.Fatalf("old line id %s message %s", id, message)
	}
	if n := b.count(t, `SELECT count(*) FROM spend WHERE id IN ('sp_young', 'sp_system') AND subject IS NOT NULL AND task = 't_x' AND subject_kind IS NULL`); n != 2 {
		t.Fatalf("a line that keeps its subject was changed: %d", n)
	}
}
