// SPDX-License-Identifier: AGPL-3.0-only

// Tests of the farm manager rules against the behaviour of main, kept small and apart from the
// manager tests, which change with the manager of v0.3.
package core

import (
	"context"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// implicate adds targets to the involved parties of an incident, as the class functions would.
func (b *controlBench) implicate(t *testing.T, key, id string) {
	t.Helper()
	if _, err := b.c.st.Pool.Exec(context.Background(), `UPDATE incidents SET record = jsonb_set(record, '{implicated}',
		coalesce(record->'implicated', '[]'::jsonb) || jsonb_build_array(jsonb_build_object($2::text, $3::text))) WHERE id = $1`, b.incident, key, id); err != nil {
		t.Fatal(err)
	}
}

// MGR-11: preempt moves the attempt to preempted without spending it and the task goes back
// to the queue; the third preemption of a task cancels it with preempt_limit.
func TestBackfillPreempt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newControlBench(t)
	task := b.hello(t, "p1")
	b.implicate(t, "task", task)
	for i := range 2 {
		att := b.claim(t, b.agent, "attempt")
		if att == nil || att.WorkID != task {
			t.Fatalf("attempt %d: %+v", i, att)
		}
		if code := b.call(t, "preempt", "task", task); code != "applied" {
			t.Fatalf("preempt %d: %s", i, code)
		}
		var lease, state string
		var spent int
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT l.state, t.state, t.attempts_spent FROM leases l JOIN tasks t ON t.id = l.work_id WHERE l.work_id = $1 AND l.lease_no = $2`,
			task, att.LeaseNo).Scan(&lease, &state, &spent); err != nil {
			t.Fatal(err)
		}
		if lease != "preempted" || state != "submitted" || spent != 0 {
			t.Fatalf("MGR-11: after preempt %d the lease is %s, the task %s with %d attempts spent", i+1, lease, state, spent)
		}
	}
	if att := b.claim(t, b.agent, "attempt"); att == nil || att.WorkID != task {
		t.Fatalf("the third attempt: %+v", att)
	}
	b.call(t, "preempt", "task", task)
	var state, reason string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE id = $1`, task).Scan(&state, &reason); err != nil {
		t.Fatal(err)
	}
	if state != "canceled" || reason != "preempt_limit" {
		t.Fatalf("MGR-11: the third preemption leaves the task %s(%s)", state, reason)
	}
}

// MGR-22: in distrust mode a cancellation goes only through a control card, and at most one
// quarantine per incident is applied by the seat; the next goes to a person.
func TestBackfillDistrustMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newControlBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "farm-manager-ops" {
				s := m["spec"].(map[string]any)
				s["caps"] = append(s["caps"].([]any), "control.apply:cancel")
			}
		}
	})
	// More workers of the role, so that the share limit leaves room for one quarantine.
	ws := []*Worker{b.agent}
	for range 7 {
		ws = append(ws, b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"}))
	}
	task := b.hello(t, "d1")
	b.implicate(t, "task", task)
	b.implicate(t, "worker", ws[1].ID)
	b.implicate(t, "worker", ws[2].ID)
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || '{"untrusted_seen": true}' WHERE type = $1`, incidentRespondType); err != nil {
		t.Fatal(err)
	}
	if code := b.call(t, "cancel", "task", task); code != "card" {
		t.Fatalf("MGR-22: a cancellation in distrust mode: %s", code)
	}
	var state string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM tasks WHERE id = $1`, task).Scan(&state); err != nil || state == "canceled" {
		t.Fatalf("MGR-22: the seat canceled the task itself: %s %v", state, err)
	}
	if code := b.call(t, "quarantine", "worker", ws[1].ID); code != "applied" {
		t.Fatalf("MGR-22: the first quarantine: %s", code)
	}
	if code := b.call(t, "quarantine", "worker", ws[2].ID); code != "card" {
		t.Fatalf("MGR-22: a second quarantine of the incident: %s", code)
	}
}

// MGR-37: a seat hold that would put more than floor(max_share_held × n) of a role's workers
// under seat holds becomes a control card, also when the limit is 0, and the conversion is not
// a refusal for the safeguard.
func TestBackfillShareHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newControlBench(t)
	b.implicate(t, "worker", b.agent.ID)
	// One worker of the role: floor(0.25 × 1) = 0.
	if code := b.call(t, "hold", "worker", b.agent.ID); code != "card" {
		t.Fatalf("MGR-37: a hold over a limit of 0: %s", code)
	}
	var denials int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT denials FROM seats WHERE tenant = $1 AND id = $2`, farmTenant, managerSeatID).Scan(&denials); err != nil || denials != 0 {
		t.Fatalf("MGR-37: the conversion counted as a refusal: %d %v", denials, err)
	}
	// Four workers: floor(0.25 × 4) = 1, so one hold applies and the second becomes a card.
	ws := []*Worker{b.agent}
	for range 3 {
		w := b.enroll(t, EnrollRequest{Tenant: "demo", Role: "hello", Kind: "agent"})
		b.implicate(t, "worker", w.ID)
		ws = append(ws, w)
	}
	if code := b.call(t, "hold", "worker", ws[1].ID); code != "applied" {
		t.Fatalf("MGR-37: a hold within the share: %s", code)
	}
	if code := b.call(t, "hold", "worker", ws[2].ID); code != "card" {
		t.Fatalf("MGR-37: a hold over the share: %s", code)
	}
}

// MGR-24: a seat attempt acts on objects of its own tenant only: an incident of another tenant
// is no live incident of the attempt's tenant, and nothing is applied there.
func TestBackfillSeatOneTenant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newControlBench(t)
	other := ids.New(ids.Incident)
	if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO incidents SELECT (jsonb_populate_record(i, jsonb_build_object('id', $2::text, 'tenant', 'demo2'))).* FROM incidents i WHERE id = $1`, b.incident, other); err != nil {
		t.Fatal(err)
	}
	own := b.incident
	b.incident = other
	if code := b.call(t, "hold", "resource", "db1"); code != "target_not_implicated" {
		t.Fatalf("MGR-24: an operation on the incident of another tenant: %s", code)
	}
	b.incident = own
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM control_ops WHERE tenant <> 'demo' OR incident = $1`, other).Scan(&n); err != nil || n != 0 {
		t.Fatalf("MGR-24: operations outside the tenant %d %v", n, err)
	}
}

// MGR-30: a reading opens a file with personal_seen (a person's upload) only when code named it
// as the subject of the incident; another such file is not_found, a file without the flag
// opens like any task file, and the read log shows each opened file with its basis.
func TestBackfillReaderPersonalFile(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	// The reading runs as system: its group sees what the files are marked.
	systemOps := map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": "system-ops", "tenant": "demo"},
		"spec": map[string]any{"caps": []any{"task:" + inspectType}, "data": []any{"summary", "internal"}}}
	b := newManagerBenchWith(t, managerWorkFiles, []map[string]any{directors, systemOps}, workEdit, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "Group" && m["metadata"].(map[string]any)["name"] == "hello" {
				m["spec"].(map[string]any)["data"] = []any{"summary", "internal"}
			}
		}
	})
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	reader := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager-reader"})
	alice := &OnBehalf{Identity: "telegram:1001", Chat: Chat{Kind: "direct", ExternalID: "1001"}}
	put := func(text string) string {
		f, err := b.c.FilePut(ctx, b.channel, "", alice, FilePutRequest{MediaType: "text/plain"}, []byte(text))
		if err != nil {
			t.Fatal(err)
		}
		return f.FileRef
	}
	named, other, plain := put("named"), put("other"), put("plain")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE files SET origin = 'produced', producer = 't_none', data_mark = 'summary' WHERE file_ref = $1`, plain); err != nil {
		t.Fatal(err)
	}
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	incident := incidentsOf(t, b, "disk_full")[0].ID
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE incidents SET record = record || jsonb_build_object('files', jsonb_build_array($2::text)) WHERE id = $1`, incident, named); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	handle := item.Payload["handles"].(map[string]any)["tools"].(string)
	if out := seatCall(t, b, handle, inspectToolName, map[string]any{"op": "request", "incident": incident}); out["code"] != "enqueued" {
		t.Fatalf("inspect_request %v", out)
	}
	// The decision role reads no tenant file.
	if _, err := b.c.FileGet(ctx, w, header(t, item), FileGetRequest{FileRef: plain}); reasonOf(err) != "not_found" {
		t.Fatalf("MGR-30: the decision role opened a file: %v", err)
	}
	ritem := b.claim(t, reader, "attempt")
	if ritem == nil {
		t.Fatal("no reading")
	}
	h := header(t, ritem)
	if _, err := b.c.FileGet(ctx, reader, h, FileGetRequest{FileRef: named}); err != nil {
		t.Fatalf("MGR-30: the named personal file: %v", err)
	}
	if _, err := b.c.FileGet(ctx, reader, h, FileGetRequest{FileRef: other}); reasonOf(err) != "not_found" {
		t.Fatalf("MGR-30: another personal file: %v", err)
	}
	if _, err := b.c.FileGet(ctx, reader, h, FileGetRequest{FileRef: plain}); err != nil {
		t.Fatalf("MGR-30: a file without the flag: %v", err)
	}
	reads, err := b.c.ManagerReads(ctx, b.op, ManagerReadsRequest{})
	if err != nil {
		t.Fatal(err)
	}
	opened := map[string]map[string]any{}
	for _, x := range reads["items"].([]any) {
		m := asMap(x)
		opened[mstr(m, "object")] = m
	}
	if opened[named] == nil || opened[plain] == nil || opened[other] != nil || mstr(opened[named], "basis") == "" {
		t.Fatalf("MGR-30: the read log %v", reads)
	}
	_, _, rec, _ := taskRecord(t, b, ritem.WorkID)
	if seen, _ := rec["personal_seen"].([]any); len(seen) == 0 {
		t.Fatalf("MGR-30: the reading that opened a personal file has no personal_seen: %v", rec)
	}
}
