// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// validOut checks an answer against its response schema.
func validOut(t *testing.T, b *bench, schema string, v any) {
	t.Helper()
	if err := b.c.spec.Validate(schema, jsonMap(t, v)); err != nil {
		t.Fatalf("%s: %v\n%+v", schema, err, v)
	}
}

func jsonMap(t *testing.T, v any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// Dead work is listed and redriven: a task that ran out of attempts gives a new task with the
// refs link and the key system:redrive:<id>:<n>, a dead delivery a planned one linked to it;
// work that is not dead and an action task are refused, and every redrive is audited (DLV-4,
// APR-7).
func TestDeadletterRedrive(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	task := b.hello(t, "1")
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := b.c.st.Pool.Exec(ctx, sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	_, err := b.c.Redrive(ctx, b.op, RedriveRequest{Op: "task.redrive", ID: task})
	wantReason(t, err, "forbidden")
	exec(`UPDATE tasks SET state = 'failed', reason = 'attempts_exhausted' WHERE id = $1`, task)
	var dead string
	b.c.st.Pool.QueryRow(ctx, `SELECT id FROM deliveries WHERE kind = 'ai_notice'`).Scan(&dead)
	exec(`UPDATE deliveries SET state = 'dead', reason = 'attempts_exhausted' WHERE id = $1`, dead)

	list, err := b.c.DeadletterList(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-deadletter-list-response", list)
	if len(list.Items) != 2 {
		t.Fatalf("dead items %+v", list.Items)
	}
	page, err := b.c.DeadletterList(ctx, b.op, TenantRequest{Limit: 1})
	if err != nil || len(page.Items) != 1 || page.Next == "" {
		t.Fatalf("first page %+v %v", page, err)
	}
	page, err = b.c.DeadletterList(ctx, b.op, TenantRequest{Limit: 1, After: page.Next})
	if err != nil || len(page.Items) != 1 || page.Next != "" || page.Items[0].ID != list.Items[1].ID {
		t.Fatalf("second page %+v %v", page, err)
	}
	other := Operator{Principal: ids.PrincipalID(), Tenant: "other"}
	_, err = b.c.DeadletterList(ctx, other, TenantRequest{Tenant: "demo"})
	wantReason(t, err, "forbidden")

	for n := 1; n <= 2; n++ {
		done, err := b.c.Redrive(ctx, b.op, RedriveRequest{Op: "task.redrive", ID: task})
		if err != nil {
			t.Fatal(err)
		}
		var key, refs, state string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT idempotency_key, record->>'refs', state FROM tasks WHERE id = $1`, done.ID).Scan(&key, &refs, &state); err != nil {
			t.Fatal(err)
		}
		if key != "system:redrive:"+task+":"+string(rune('0'+n)) || refs != `["`+task+`"]` || state != "submitted" {
			t.Fatalf("redrive %d: key %s refs %s state %s", n, key, refs, state)
		}
	}
	_, err = b.c.Redrive(ctx, other, RedriveRequest{Op: "task.redrive", ID: task})
	wantReason(t, err, "not_found")
	exec(`UPDATE tasks SET type = 'action.restart.v1' WHERE id = $1`, task)
	_, err = b.c.Redrive(ctx, b.op, RedriveRequest{Op: "task.redrive", ID: task})
	wantReason(t, err, "forbidden")

	// The decision subject goes along, so a redriven card is built again (APR-3).
	exec(`UPDATE deliveries SET subject_kind = 'approval', subject_id = 'apr_1' WHERE id = $1`, dead)
	done, err := b.c.Redrive(ctx, b.op, RedriveRequest{Op: "delivery.redrive", ID: dead})
	if err != nil {
		t.Fatal(err)
	}
	var state, of, key, pointer, subject string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT d.state, d.redrive_of, d.redrive_key, ct.ai_notice_delivery, d.subject_kind || ':' || d.subject_id FROM deliveries d, contacts ct WHERE d.id = $1`, done.ID).
		Scan(&state, &of, &key, &pointer, &subject); err != nil {
		t.Fatal(err)
	}
	if state != "planned" || of != dead || key != "system:redrive:"+dead+":1" || pointer != done.ID || subject != "approval:apr_1" {
		t.Fatalf("delivery redrive: %s %s %s %s %s", state, of, key, pointer, subject)
	}
	var audits int
	b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM audit WHERE record->>'command' LIKE '%.redrive'`).Scan(&audits)
	if audits != 3 {
		t.Fatalf("%d redrive audit records", audits)
	}
}

// audit.query returns the records of the credential's tenant by a filter, pages by seq and
// audits the read itself (AUD-7).
func TestAuditQuery(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	res, err := b.c.AuditQuery(ctx, b.op, AuditQueryRequest{Command: "grant"})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-audit-query-response", res)
	if len(res.Items) != 1 {
		t.Fatalf("grant records %d", len(res.Items))
	}
	all, err := b.c.AuditQuery(ctx, b.op, AuditQueryRequest{Limit: 1})
	if err != nil || len(all.Items) != 1 || all.NextAfterSeq == 0 {
		t.Fatalf("page %+v %v", all, err)
	}
	reads, err := b.c.AuditQuery(ctx, b.op, AuditQueryRequest{Kind: "read"})
	if err != nil || len(reads.Items) != 2 || !strings.Contains(string(reads.Items[0]), `"farmctl"`) {
		t.Fatalf("read records %v %v", len(reads.Items), err)
	}
	_, err = b.c.AuditQuery(ctx, b.op, AuditQueryRequest{From: "yesterday"})
	wantReason(t, err, "input_schema_invalid")
}

// status shows the applied commit and the claim of this build; doctor lists the checks with
// the tick of each loop and the tenant counts (MAN-8, OBS-4, CAP-5).
func TestStatusDoctor(t *testing.T) {
	ctx := context.Background()
	b := newBench(t)
	st, err := b.c.Status(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-status-response", st)
	if st.Commit != strings.Repeat("0", 40) || st.ConformsTo.Skeleton != "0.3.0" || len(st.FarmManagerDisabled) != 1 ||
		!slices.Contains(st.ConformsTo.Profiles, "knowledge") || !slices.Contains(st.ConformsTo.Profiles, "ingress") || slices.Contains(st.ConformsTo.Profiles, "sandbox") {
		t.Fatalf("status %+v", st)
	}
	if err := b.c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	doc, err := b.c.Doctor(ctx, b.op, TenantRequest{})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b, "p2-doctor-response", doc)
	if !doc.OK {
		t.Fatalf("doctor %+v", doc)
	}
	b.advance(t, 60)
	doc, _ = b.c.Doctor(ctx, b.op, TenantRequest{})
	if doc.OK {
		t.Fatal("doctor ok with every loop a minute old")
	}
}
