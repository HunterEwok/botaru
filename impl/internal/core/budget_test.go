// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
)

// pricedChecks makes checks/latest a tool the farm pays outside the model provider.
func pricedChecks(price float64) func(ms []map[string]any) []map[string]any {
	return func(ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] != "ToolPlugin" || m["metadata"].(map[string]any)["name"] != "checks" {
				continue
			}
			for _, x := range m["spec"].(map[string]any)["tools"].([]any) {
				tool := x.(map[string]any)
				if tool["name"] == "latest" {
					tool["billing"], tool["price_eur"] = "external", price
				}
			}
		}
		return ms
	}
}

type spendLine struct {
	state, line, unit string
	micros            int64
}

func toolSpend(t *testing.T, b *bench) map[string]spendLine {
	t.Helper()
	rows, err := b.c.st.Pool.Query(context.Background(), `SELECT toolcall, state, line, unit, eur_micros FROM spend WHERE toolcall IS NOT NULL`)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]spendLine{}
	for rows.Next() {
		var id string
		var s spendLine
		if err := rows.Scan(&id, &s.state, &s.line, &s.unit, &s.micros); err != nil {
			t.Fatal(err)
		}
		out[id] = s
	}
	return out
}

func lastCall(t *testing.T, b *bench) (string, string, string) {
	t.Helper()
	var id, state string
	var reason *string
	if err := b.c.st.Pool.QueryRow(context.Background(), `SELECT id, state, reason FROM toolcalls ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&id, &state, &reason); err != nil {
		t.Fatal(err)
	}
	return id, state, deref(reason)
}

// A tool call goes only after a reserve at its manifest price; the reserve becomes the usage
// line of the call when a toolhost takes it, and is given back when nobody does (BUD-2, BUD-3,
// BUD-5).
func TestToolCallMoney(t *testing.T) {
	ctx := context.Background()
	b := newToolBenchWith(t, nil, pricedChecks(0.02))
	att, h := b.attemptTools(t, "1", false)
	th := b.toolhost(t, "checks")
	args := map[string]any{"check": "www-health"}
	// The line is pinned at enqueue: a line written in the record wins over the type's purpose
	// as applied now (BUD-7).
	task := att.Payload["task"].(map[string]any)["id"].(string)
	var pinned string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT record->>'budget_line' FROM tasks WHERE id = $1`, task).Scan(&pinned); err != nil || pinned != "work" {
		t.Fatalf("pinned line %q %v", pinned, err)
	}

	done := callAsync(b, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
	item := b.claimTool(t, th)
	id, _, _ := lastCall(t, b)
	if s := toolSpend(t, b)[id]; s.state != "reserved" || s.micros != 20000 || s.line != "work" || s.unit != "eur_cents" {
		t.Fatalf("reserve of a leased call %+v", s)
	}
	out := map[string]any{"content": []any{}, "structuredContent": map[string]any{"check": "www-health", "ok": 3, "total": 3}}
	if _, err := b.c.Complete(ctx, th, header(t, item), CompleteRequest{Output: out}); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.err != nil || res.r.IsError {
		t.Fatalf("call %+v %v", res.r, res.err)
	}
	if s := toolSpend(t, b)[id]; s.state != "committed" || s.micros != 20000 {
		t.Fatalf("usage line of a done call %+v", s)
	}

	// A call nobody took gives its reserve back.
	done = callAsync(b, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
	waitQueued(t, b)
	id, _, _ = lastCall(t, b)
	b.advance(t, 31)
	if err := b.c.toolcallDeadlines(ctx); err != nil {
		t.Fatal(err)
	}
	if res := <-done; res.err != nil || callText(res.r) != "error: no_executor" {
		t.Fatalf("no executor %+v %v", res.r, res.err)
	}
	if s, ok := toolSpend(t, b)[id]; ok {
		t.Fatalf("a call nobody took kept its reserve %+v", s)
	}
	b.advance(t, -31)

	// A call that does not fit the day limit does not go: the attempt gets budget_exhausted.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_full', 'demo', 'work', 'tokens', 'committed', 14990000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	_, err := b.c.P3ToolsCall(ctx, h, mcp.CallParams{Name: "checks_latest", Arguments: args})
	var p *Problem
	if !errors.As(err, &p) || p.Reason != "budget_exhausted" {
		t.Fatalf("over the limit: %v", err)
	}
	id, state, reason := lastCall(t, b)
	if state != "error" || reason != "budget_exhausted" {
		t.Fatalf("call record %s %s", state, reason)
	}
	if _, ok := toolSpend(t, b)[id]; ok {
		t.Fatal("a refused call has a reserve")
	}
}

// A tool without billing: external costs nothing but still gets its usage line (BUD-3), on
// the line pinned in the task record, not the one of the type as applied now (BUD-7).
func TestFreeToolCallLine(t *testing.T) {
	ctx := context.Background()
	b := newToolBench(t)
	att, h := b.attemptTools(t, "1", false)
	task := att.Payload["task"].(map[string]any)["id"].(string)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"budget_line": "review", "purpose": "review"}' WHERE id = $1`, task); err != nil {
		t.Fatal(err)
	}
	th := b.toolhost(t, "checks")
	done := callAsync(b, h, mcp.CallParams{Name: "checks_latest", Arguments: map[string]any{"check": "www-health"}})
	item := b.claimTool(t, th)
	if _, err := b.c.Fail(ctx, th, header(t, item), FailRequest{Reason: "exec_failed"}); err != nil {
		t.Fatal(err)
	}
	<-done
	id, _, _ := lastCall(t, b)
	if s := toolSpend(t, b)[id]; s.state != "committed" || s.micros != 0 || s.line != "review" {
		t.Fatalf("usage line of a free call %+v", s)
	}
}

// The budget lines (BUD-7): a line other than work is closed until it gets money, its month and
// day limits and the limit per role hold, a used-up line never stops the work line, and the
// hourly figure is not a reserve limit (BUD-4).
func TestBudgetLines(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.c.budgetLine("inspect"); got != "manager" {
		t.Fatalf("line of inspect %q", got)
	}
	if got := b.c.budgetLine(""); got != "work" {
		t.Fatalf("line without a purpose %q", got)
	}
	tv := *cat.Tenants["demo"]
	eur := func(v float64) *float64 { return &v }
	tv.Spec.Budgets.Lines = map[string]manifest.LineBudget{"insight": {MonthEUR: eur(1), DayEUR: eur(0.9), PerRoleMonthEUR: eur(0.7)}}
	micros := func(v float64) int64 { return money.FromEUR(v) }
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: 1, line: "review"}); r != "budget_exhausted" {
		t.Fatalf("a line without money: %q", r)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: 0, line: "review"}); r != "budget_exhausted" {
		t.Fatalf("a line without money and no estimate: %q", r)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: micros(0.5), line: "insight", role: "a"}); r != "" {
		t.Fatalf("a line with money: %q", r)
	}
	// 0.6 of the role a is used: the role limit 0.7 stops a, not b; then the day limit 0.9.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, role, line, unit, state, eur_micros, time)
		VALUES ('spd_a', 'demo', 'a', 'insight', 'tokens', 'committed', 600000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: micros(0.2), line: "insight", role: "a"}); r != "budget_exhausted" {
		t.Fatalf("over the role limit: %q", r)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: micros(0.2), line: "insight", role: "b"}); r != "" {
		t.Fatalf("another role: %q", r)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: micros(0.35), line: "insight", role: "b"}); r != "budget_exhausted" {
		t.Fatalf("over the line day limit: %q", r)
	}
	// A line that is used up does not touch the work line; the hour figure is not a reserve
	// limit: 2.99 of the 3 EUR hour is used and a 0.45 reserve still passes.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_w', 'demo', 'work', 'tokens', 'committed', 2390000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: micros(0.45), line: "work"}); r != "" {
		t.Fatalf("the work line: %q", r)
	}
	if r := checkAsk(t, b, &tv, budgetAsk{subject: "system", extra: micros(0.45), line: "insight"}); r != "budget_exhausted" {
		t.Fatalf("over the insight line: %q", r)
	}
}

func checkAsk(t *testing.T, b *bench, tv *TenantView, ask budgetAsk) string {
	t.Helper()
	var reason string
	err := b.c.tx(context.Background(), func(tx pgx.Tx, now time.Time) error {
		if p := b.c.checkBudget(context.Background(), tx, now, tv, ask); p != nil {
			reason = p.Reason
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

// Usage over hour_eur in the sliding hour sets a no_claim hold of contain_auto on every agent
// role for an hour and raises alarm.hold.auto once; a job role keeps working; the alarm resolves
// when the holds end (HLD-7, BUD-4).
func TestRunawaySpend(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		return append(ms, map[string]any{
			"apiVersion": "botaru/v1alpha1", "kind": "Role", "metadata": map[string]any{"name": "loader"},
			"spec": map[string]any{"profile": "worker", "worker_kind": "job", "claims": []any{"hello.greet.v1"}},
		})
	})
	holds := func() map[string]string {
		rows, err := b.c.st.Pool.Query(ctx, `SELECT scope_name, author_kind || ' ' || array_to_string(effects, ',') || ' ' || (until - created_at)::text
			FROM holds WHERE state = 'active' AND reason = 'runaway_spend'`)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for rows.Next() {
			var name, v string
			if err := rows.Scan(&name, &v); err != nil {
				t.Fatal(err)
			}
			out[name] = v
		}
		return out
	}
	// 2.9 EUR of the 3 EUR hour: no hold. A reserve does not count, only usage.
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time) VALUES
		('spd_1', 'demo', 'work', 'tokens', 'committed', 2900000, botaru_now()),
		('spd_2', 'demo', 'work', 'tokens', 'reserved', 500000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	if err := b.c.holdLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if h := holds(); len(h) != 0 {
		t.Fatalf("holds under the threshold %v", h)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO spend (id, tenant, line, unit, state, eur_micros, time)
		VALUES ('spd_3', 'demo', 'work', 'tokens', 'estimated', 200000, botaru_now())`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := b.c.holdLoop(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if h := holds(); len(h) != 1 || h["hello"] != "contain_auto no_claim 01:00:00" {
		t.Fatalf("holds over the threshold %v", h)
	}
	if n := countEvents(t, b, "alarm.hold.auto.v1", "hold.auto:demo"); n != 1 {
		t.Fatalf("alarm events %d", n)
	}
	b.hello(t, "1")
	if item := b.claim(t, b.agent, "attempt"); item != nil {
		t.Fatal("an agent role under the hold took an attempt")
	}
	// After the hour the holds expire and the alarm resolves.
	b.advance(t, 3601)
	if err := b.c.holdLoop(ctx); err != nil {
		t.Fatal(err)
	}
	if h := holds(); len(h) != 0 {
		t.Fatalf("holds after the hour %v", h)
	}
	if n := countEvents(t, b, "alarm.hold.auto.v1", "hold.auto:demo"); n != 2 {
		t.Fatalf("alarm events after the hour %d", n)
	}
}
