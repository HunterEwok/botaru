// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// controlBench is the manager bench with an open disk incident on db1 and an attempt of the
// seat holder; call applies control_ops through that attempt's tools handle.
type controlBench struct {
	*bench
	incident string
	handle   string
}

func newControlBench(t *testing.T) *controlBench {
	t.Helper()
	ctx := context.Background()
	b := newManagerBench(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "farm-manager"})
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	if _, err := b.c.SeatAssign(ctx, b.farmOp(), SeatRequest{Seat: managerSeatID}); err != nil {
		t.Fatal(err)
	}
	item := b.claim(t, w, "attempt")
	if item == nil {
		t.Fatal("the holder's task was not handed out")
	}
	incs := incidentsOf(t, b, "disk_full")
	if len(incs) != 1 {
		t.Fatalf("incidents %d", len(incs))
	}
	return &controlBench{bench: b, incident: incs[0].ID, handle: item.Payload["handles"].(map[string]any)["tools"].(string)}
}

func (b *controlBench) call(t *testing.T, op, kind, id string) string {
	t.Helper()
	args := map[string]any{"op": op, "target": map[string]any{"kind": kind, "id": id}, "basis": map[string]any{"incident": b.incident}}
	if op == "hold" {
		args["effects"] = []any{"no_claim"}
		args["ttl"] = "3h"
	}
	r, err := b.c.P3ToolsCall(context.Background(), b.handle, mcp.CallParams{Name: controlToolName, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Code string `json:"code"`
		ID   string `json:"id"`
	}
	if err := json.Unmarshal([]byte(callText(r)), &out); err != nil {
		t.Fatalf("result %q: %v", callText(r), err)
	}
	if out.Code == "applied" && out.ID == "" {
		t.Fatal("an applied operation without its id")
	}
	return out.Code
}

// The seat holder applies an operation on a target involved in the incident: the hold carries
// the seat as its author with the ttl capped at hold_ttl_max, the operation is recorded and
// handles the incident; a person reverts the seat's holds on the incident (MGR-9, MGR-14, MGR-15).
func TestControlOpsSeat(t *testing.T) {
	ctx := context.Background()
	b := newControlBench(t)
	if code := b.call(t, "hold", "resource", "db1"); code != "applied" {
		t.Fatalf("hold %s", code)
	}
	var n int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE author_kind = 'seat_holder' AND state = 'active'
		AND until <= created_at + interval '2 hours'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("seat holds %d %v", n, err)
	}
	if countEvents(t, b.bench, "control.applied.v1", "") != 1 {
		t.Fatal("no control.applied event")
	}
	inc := incidentsOf(t, b.bench, "disk_full")[0]
	if !inc.Handled || len(inc.Record["ops"].([]any)) != 1 {
		t.Fatalf("incident %+v", inc)
	}
	list, err := b.c.ControlList(ctx, b.op, TenantRequest{})
	if err != nil || len(list["items"].([]any)) != 1 {
		t.Fatalf("list %v %v", list, err)
	}
	if code := b.call(t, "hold", "resource", "db2"); code != "target_not_implicated" {
		t.Fatalf("an uninvolved target: %s", code)
	}
	if _, err := b.c.ControlRevert(ctx, b.op, ControlRevertRequest{Incident: b.incident}); err != nil {
		t.Fatal(err)
	}
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM holds WHERE author_kind = 'seat_holder' AND state = 'active'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("seat holds after the revert %d %v", n, err)
	}
	if _, err := b.c.ControlRevert(ctx, b.op, ControlRevertRequest{Since: "2026-01-01T00:00:00Z"}); err == nil {
		t.Fatal("a tenant operator reverted a period")
	}
}

// Three seat refusals in a row trip the safeguard: no_control stops the seat and
// alarm.manager.breaker pages people (MGR-14).
func TestControlOpsBreaker(t *testing.T) {
	b := newControlBench(t)
	for range 3 {
		if code := b.call(t, "hold", "resource", "db2"); code != "target_not_implicated" {
			t.Fatalf("an uninvolved target: %s", code)
		}
	}
	m := b.managerSeat(t)
	if m.State != "stopped" || !m.has("no_control") {
		t.Fatalf("seat %+v", m)
	}
	if openAlarms(t, b.bench, "alarm.manager.breaker.v1") != 1 {
		t.Fatal("no breaker alarm")
	}
}

// A person of the tenant applies an operation by the same code path, without a basis incident;
// a person may not release a hold the seat did not set by control.apply (MGR-9).
func TestControlOpsPerson(t *testing.T) {
	ctx := context.Background()
	b := newControlBench(t)
	d, err := b.c.ControlApply(ctx, b.op, ControlRequest{Op: "hold", Target: ControlTarget{Kind: "role", ID: "hello"}, Effects: []string{"no_claim"}, TTL: "30m"})
	if err != nil || d.State != "applied" {
		t.Fatalf("apply %+v %v", d, err)
	}
	var hold string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT hold FROM control_ops WHERE id = $1`, d.ID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.ControlApply(ctx, b.op, ControlRequest{Op: "release", Target: ControlTarget{Kind: "hold", ID: hold}}); err == nil {
		t.Fatal("a person's hold was released by control.apply")
	}
	if _, err := b.c.ControlApply(ctx, b.op, ControlRequest{Op: "pin_task", Target: ControlTarget{Kind: "worker", ID: "wk_x"}}); err == nil {
		t.Fatal("an operation this version does not serve was applied")
	}
}
