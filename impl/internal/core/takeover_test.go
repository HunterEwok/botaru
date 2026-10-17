// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// grantManagers adds a group with model_grant.manage and puts a new person with telegram:1002
// in it.
func (b *bench) grantManagers(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	if _, err := b.c.Apply(ctx, b.op, ApplyRequest{Commit: strings.Repeat("1", 40), Manifests: []map[string]any{{"apiVersion": "botaru/v1alpha1", "kind": "Group",
		"metadata": map[string]any{"name": "grant-managers", "tenant": "demo"}, "spec": map[string]any{"caps": []any{"model_grant.manage"}}}}}); err != nil {
		t.Fatal(err)
	}
	bob := ids.PrincipalID()
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	b.member(t, "demo", bob, "grant-managers")
	return bob
}

// A takeover request of the seat is refused under distrust (takeover_untrusted) and without its
// conditions (takeover_not_allowed); a valid one is staged, published at the end of the seat
// attempt as the takeover_confirm card, and "Approve" makes the new task system:takeover:<task>
// with a takeover grant on the seat profile (untrusted text seen after staging drops the request
// at publication instead) for one attempt on the takeover line, the original
// canceled as superseded_by_takeover (MGR-21). An apply refuses a takeover card threshold above
// 0 (takeover_confirm_threshold).
func TestTakeover(t *testing.T) {
	ctx := context.Background()
	b := managerV02Bench(t, nil, nil, func(ms []map[string]any) {
		for _, m := range ms {
			spec := asMap(m["spec"])
			switch {
			case m["kind"] == "Tenant":
				spec["takeover"] = map[string]any{"types": []any{"hello.greet.v1"}}
			case m["kind"] == "Group" && mstr(asMap(m["metadata"]), "name") == "farm-manager-ops":
				spec["caps"] = append(spec["caps"].([]any), "task.takeover:hello.greet.v1")
			}
		}
	})
	bob := b.grantManagers(t)
	// The functionary role runs the takeover on the seat profile only with its eval report on
	// that profile (MGR-48).
	fp, err := takeoverFingerprint(b.catalog(t), "demo", "hello", "manager-std")
	if err != nil {
		t.Fatal(err)
	}
	b.setStoredDoc(t, "EvalReports", "", "evals", `{"apiVersion": "botaru/v1alpha1", "kind": "EvalReports", "metadata": {"name": "evals"},
		"spec": {"reports": [{"fingerprint": "`+fp+`", "subject": {"kind": "role", "name": "hello"}, "suite": "takeover", "date": "2026-10-02", "cases": 12, "failures": 0, "security_failures": 0}]}}`)
	orig := b.hello(t, "u1")
	mw := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, mw, "attempt")
	if item == nil {
		t.Fatal("no seat attempt")
	}
	h := item.Payload["handles"].(map[string]any)["tools"].(string)
	seatTask := item.WorkID
	if out := callTool(t, b, h, takeoverToolName, map[string]any{"task": ids.New(ids.Task)}); out["code"] != "takeover_not_allowed" {
		t.Fatalf("unknown task %v", out)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true}' WHERE id = $1`, seatTask); err != nil {
		t.Fatal(err)
	}
	if out := callTool(t, b, h, takeoverToolName, map[string]any{"task": orig}); out["code"] != "takeover_untrusted" {
		t.Fatalf("under distrust %v", out)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": false}' WHERE id = $1`, seatTask); err != nil {
		t.Fatal(err)
	}
	if out := callTool(t, b, h, takeoverToolName, map[string]any{"task": orig}); out["staged"] != true {
		t.Fatalf("takeover %v", out)
	}
	// Untrusted text seen after staging drops the request at publication (rolled back here).
	func() {
		tx, err := b.c.st.Pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var leaseNo int
		var state string
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true}' WHERE id = $1`, seatTask); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT lease_no FROM takeovers WHERE seat_task = $1`, seatTask).Scan(&leaseNo); err != nil {
			t.Fatal(err)
		}
		cat, err := b.c.Catalog(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := b.c.publishTakeovers(ctx, tx, time.Now(), cat, seatTask, leaseNo); err != nil {
			t.Fatal(err)
		}
		if err := tx.QueryRow(ctx, `SELECT state || ':' || coalesce(reason, '') FROM takeovers WHERE seat_task = $1`, seatTask).Scan(&state); err != nil || state != "dropped:takeover_untrusted" {
			t.Fatalf("untrusted at publication %s %v", state, err)
		}
		var audits int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'reason' = 'takeover_untrusted' AND record->'after'->>'state' = 'dropped'`).Scan(&audits); err != nil || audits != 1 {
			t.Fatalf("drop audit %d %v", audits, err)
		}
	}()
	if _, err := b.c.Complete(ctx, mw, header(t, item), CompleteRequest{Result: map[string]any{"summary": "asked"}}); err != nil {
		t.Fatal(err)
	}
	card := cardOf(t, b.drain(t), takeoverCard, "direct:1002")
	body := asMap(card.body["card"])
	if asMap(body["context"])["profile"] != "manager-std" || asMap(body["context"])["task"] != orig {
		t.Fatalf("card %v", body)
	}
	res, err := b.c.Decide(ctx, b.channel, press("telegram:1002", "direct:1002", card.external),
		DecideRequest{SubjectKind: "takeover", ID: body["id"].(string), Nonce: body["nonce"].(string), Decision: "approve"})
	if err != nil || res.State != "approved" {
		t.Fatalf("decide %+v %v", res, err)
	}
	var newTask, kind, line, profile, by, state, reason string
	var attempts int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT t.id, g.kind, g.line, g.profile, g.attempts_left, g.decided_by->>'principal' FROM tasks t JOIN model_grants g ON g.task = t.id
		WHERE t.idempotency_key LIKE '%system:takeover:' || $1`, orig).Scan(&newTask, &kind, &line, &profile, &attempts, &by); err != nil {
		t.Fatal(err)
	}
	if kind != "takeover" || line != "manager" || profile != "manager-std" || attempts != 1 || by != bob {
		t.Fatalf("grant %s %s %s %d %s", kind, line, profile, attempts, by)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, reason FROM tasks WHERE id = $1`, orig).Scan(&state, &reason); err != nil || state != "canceled" || reason != "superseded_by_takeover" {
		t.Fatalf("original %s %s %v", state, reason, err)
	}
	att := b.claim(t, b.agent, "attempt")
	if att == nil || att.WorkID != newTask {
		t.Fatalf("takeover attempt %+v", att)
	}
	if mg, err := b.c.ResolveModelHandle(ctx, att.Payload["handles"].(map[string]any)["model"].(string)); err != nil || mg.Slot != grantedSlot || mg.ProfileName != "manager-std" || mg.Line != "manager" {
		t.Fatalf("handle %+v %v", mg, err)
	}

	c := lintCore(t)
	if codes := lintCodes(t, c, `apiVersion: botaru/v1alpha1
kind: Tenant
metadata: {name: t1}
spec: {kind: test, timezone: UTC, takeover: {types: [hello.greet.v1], confirm_above_eur: 5}}
`); !strings.Contains(strings.Join(codes, ","), "takeover_confirm_threshold") {
		t.Fatalf("lint %v", codes)
	}
}
