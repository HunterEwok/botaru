// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
)

// controlBench is the manager bench with an open disk incident on db1 and an attempt of the
// seat holder; call applies control_ops through that attempt's tools handle.
type controlBench struct {
	*bench
	incident string
	handle   string
}

func newControlBench(t *testing.T, edits ...func([]map[string]any)) *controlBench {
	t.Helper()
	ctx := context.Background()
	b := newManagerBench(t, edits...)
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
		args["ttl"] = "1h"
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
// the seat as its author within hold_ttl_max, the operation is recorded and
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
	if !inc.Handled || len(inc.Record["ops"].([]any)) != 1 || inc.State != "mitigating" {
		t.Fatalf("incident %+v", inc)
	}
	if countEvents(t, b.bench, "incident.mitigating.v1", "") != 1 {
		t.Fatal("no incident.mitigating event")
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
	// The safeguard is a seat hold that a person releases; its release removes no_control.
	ctx := context.Background()
	var hold string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id FROM holds WHERE state = 'active' AND scope_kind = 'seat' AND effects = ARRAY['no_control']
		AND reason = 'manager_breaker'`).Scan(&hold); err != nil {
		t.Fatalf("safeguard hold: %v", err)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.farmOp()), HoldReleaseRequest{ID: hold}); err != nil {
		t.Fatal(err)
	}
	if m := b.managerSeat(t); m.State != "held" || m.has("no_control") {
		t.Fatalf("seat after the release %+v", m)
	}
}

// A seat hold longer than hold_ttl_max is refused, not cut short; an operation the tenant did
// not consent to is not offered (cancel is not in the bench's farm-manager-ops); release takes
// only a hold of the current epoch (MGR-3, MGR-9, MGR-14).
func TestControlOpsLimits(t *testing.T) {
	ctx := context.Background()
	b := newControlBench(t)
	args := map[string]any{"op": "hold", "target": map[string]any{"kind": "resource", "id": "db1"}, "basis": map[string]any{"incident": b.incident},
		"effects": []any{"no_claim"}, "ttl": "3h"}
	if r, err := b.c.P3ToolsCall(ctx, b.handle, mcp.CallParams{Name: controlToolName, Arguments: args}); err != nil || !strings.Contains(callText(r), "input_schema_invalid") {
		t.Fatalf("a hold above hold_ttl_max: %v %v", callText(r), err)
	}
	if code := b.call(t, "cancel", "task", "tk_x"); code != "input_schema_invalid" {
		t.Fatalf("an operation without consent: %s", code)
	}
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !seatConsents(cat, "demo", "hold") || seatConsents(cat, "demo", "cancel") {
		t.Fatal("consent of the tenant group")
	}
	if code := b.call(t, "hold", "resource", "db1"); code != "applied" {
		t.Fatalf("hold %s", code)
	}
	var hold string
	if err := b.c.st.Pool.QueryRow(ctx, `UPDATE control_ops SET incident = NULL WHERE actor_kind = 'seat' RETURNING hold`).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if code := b.call(t, "release", "hold", hold); code != "forbidden" {
		t.Fatalf("a hold of another incident was released: %s", code)
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE control_ops SET incident = $1, epoch = epoch - 1 WHERE actor_kind = 'seat'`, b.incident); err != nil {
		t.Fatal(err)
	}
	if code := b.call(t, "release", "hold", hold); code != "forbidden" {
		t.Fatalf("a hold of an earlier epoch was released: %s", code)
	}
}

// A drain by the id as the incident view shows it (lowercase) moves the stored worker to
// draining; a hello keeps it there while the hold lasts and returns it to active after the
// release (MGR-9, spec/lifecycles/worker.md).
func TestControlOpsDrain(t *testing.T) {
	ctx := context.Background()
	b := newControlBench(t)
	w := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "agent", Role: "hello"})
	d, err := b.c.ControlApply(ctx, b.op, ControlRequest{Op: "drain", Target: ControlTarget{Kind: "worker", ID: strings.ToLower(w.ID)}, TTL: "30m"})
	if err != nil {
		t.Fatal(err)
	}
	state := func() string {
		var s string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT state FROM workers WHERE id = $1`, w.ID).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	hello := func() {
		if _, err := b.c.Hello(ctx, w, HelloRequest{Kind: "agent", ProtocolVersions: []string{"v1"}}); err != nil {
			t.Fatal(err)
		}
	}
	if hello(); state() != "draining" {
		t.Fatalf("after the drain and a hello: %s", state())
	}
	var hold string
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT hold FROM control_ops WHERE id = $1`, d.ID).Scan(&hold); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.HoldRelease(ctx, OperatorActor(b.op), HoldReleaseRequest{ID: hold}); err != nil {
		t.Fatal(err)
	}
	if hello(); state() != "active" {
		t.Fatalf("after the release and a hello: %s", state())
	}
}

// The seat applies for an incident only the operations its class lists in responses (MGR-9).
func TestControlOpsClassResponses(t *testing.T) {
	b := newControlBench(t, func(ms []map[string]any) {
		for _, m := range ms {
			if m["kind"] == "IncidentClass" && m["metadata"].(map[string]any)["name"] == "disk_full" {
				m["spec"].(map[string]any)["responses"] = map[string]any{"ops": []any{"drain", "release"}}
			}
		}
	})
	if code := b.call(t, "hold", "resource", "db1"); code != "forbidden" {
		t.Fatalf("an operation the class does not list: %s", code)
	}
}

// Types and roles any incident class names are protected targets (MGR-32).
func TestClassNamedProtected(t *testing.T) {
	cat := newCatalog()
	cat.RawGlobals[manifest.Key{Kind: "IncidentClass", Name: "slow"}] = map[string]any{"spec": map[string]any{
		"responses": map[string]any{"pin_types": []any{"ops.pin.v1"}}, "implicate": []any{map[string]any{"fn": "role_failing", "args": map[string]any{"role": "pinner"}}}}}
	cat.TaskTypes["ops.pin.v1"] = manifest.TaskType{}
	cat.TaskTypes["ops.other.v1"] = manifest.TaskType{}
	if !protectedType(cat, "ops.pin.v1") || protectedType(cat, "ops.other.v1") {
		t.Fatal("a type named by a class")
	}
	c := &Core{}
	if p, err := c.protectedTarget(context.Background(), nil, cat, "demo", ControlTarget{Kind: "role", ID: "pinner"}); err != nil || !p {
		t.Fatal("a role named by a class")
	}
}

// The types that publish the alarm of a class with stops_manager, and the roles that claim
// them, are out of reach of seat operations (GOV-28, MGR-9).
func TestStopWriters(t *testing.T) {
	cat := newCatalog()
	cat.RawGlobals[manifest.Key{Kind: "IncidentClass", Name: "disk_full"}] = map[string]any{"spec": map[string]any{"stops_manager": true,
		"detect": map[string]any{"alarm": "alarm.disk.free.low.v1"}}}
	cat.RawGlobals[manifest.Key{Kind: "IncidentClass", Name: "slow"}] = map[string]any{"spec": map[string]any{
		"detect": map[string]any{"alarm": "alarm.slow.v1"}}}
	cat.TaskTypes["ops.volume.series.v1"] = manifest.TaskType{Events: []string{"alarm.disk.free.low.v1"}}
	cat.TaskTypes["ops.slow.v1"] = manifest.TaskType{Events: []string{"alarm.slow.v1"}}
	cat.Roles["volume-series"] = manifest.Role{Claims: []string{"ops.volume.series.v1"}}
	cat.Roles["slow-watch"] = manifest.Role{Claims: []string{"ops.slow.v1"}}
	types, roles := stopWriters(cat)
	if !types["ops.volume.series.v1"] || types["ops.slow.v1"] || !roles["volume-series"] || roles["slow-watch"] {
		t.Fatalf("writers %v %v", types, roles)
	}
}

// A task type among the incident's involved parties is a valid target (MGR-9).
func TestImplicatedTaskType(t *testing.T) {
	inc := &incidentRow{SubjectKind: "resource", SubjectID: "db1", Record: map[string]any{"implicated": []any{implicated{TaskType: "ops.summary.v1"}.view()}}}
	if !implicatedTarget(inc, ControlTarget{Kind: "tasktype", ID: "ops.summary.v1"}) {
		t.Fatal("the involved task type is not a target")
	}
	if implicatedTarget(inc, ControlTarget{Kind: "tasktype", ID: "ops.health.v1"}) {
		t.Fatal("an uninvolved task type is a target")
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
