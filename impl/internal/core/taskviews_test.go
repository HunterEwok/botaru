// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"testing"
)

// P1 task.get and cancel of a program, P2 task.show and task.cancel (PRM-9, DSP-2, APR-6,
// IDM-3): the initiator reads its task without the reply address, with fairness_key
// interactive:<principal>; a foreign task, another tenant's and a missing one answer the same
// not_found; a reader with task.read and data reads another's task; a cancellation by the
// initiator is canceled(initiator) and a repeat gives the same answer; an operator's cancel of
// another's task is canceled(admin), and a finished task is already_terminal.
func TestTaskViews(t *testing.T) {
	ctx := context.Background()
	b := newPeopleBench(t, nil)
	alice := b.aliceTask(t, "1")
	aliceOp := Operator{Principal: b.alice, Tenant: "demo"}
	v, err := b.c.TaskShow(ctx, aliceOp, P2TaskRequest{Op: "task.show", Task: alice})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "p2-task-show-response", v)
	if v.Task["fairness_key"] != "interactive:"+b.alice || v.Task["origin"] != nil || v.Task["conversation"] != nil {
		t.Fatalf("view %v", v.Task)
	}
	// Someone without task.read, another tenant's operator and a missing id: the same not_found.
	bob := Operator{Principal: "human:p_01JB0000000000000000000B0B", Tenant: "demo"}
	for _, c := range []struct {
		op Operator
		id string
	}{{bob, alice}, {Operator{Principal: b.alice, Tenant: "demo2"}, alice}, {aliceOp, "t_01JB0000000000000000000099"}} {
		if _, err := b.c.TaskShow(ctx, c.op, P2TaskRequest{Op: "task.show", Task: c.id}); reasonOf(err) != "not_found" {
			t.Fatalf("%+v: %v", c, err)
		}
	}
	if _, err := b.c.ProgramTaskGet(ctx, b.prog, TaskIDRequest{Task: alice}); reasonOf(err) != "not_found" {
		t.Fatalf("a program reads alice's task: %v", err)
	}
	if _, err := b.c.ProgramCancel(ctx, b.prog, TaskIDRequest{Task: alice}); reasonOf(err) != "not_found" {
		t.Fatalf("a program cancels alice's task: %v", err)
	}
	// A reader with task.read:<type> and data:<mark> (group leads).
	if _, err := b.c.Principal(ctx, b.op, PrincipalRequest{Op: "principal.add", Subject: bob.Principal, Identity: "telegram:1002"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: bob.Principal, Group: "leads"}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TaskShow(ctx, bob, P2TaskRequest{Op: "task.show", Task: alice}); err != nil {
		t.Fatalf("a reader with task.read: %v", err)
	}

	// The program's own task over P1.
	ref, err := b.c.ProgramEnqueue(ctx, b.prog, EnqueueRequest{Body: greet("report"), IdempotencyKey: "r1"})
	if err != nil {
		t.Fatal(err)
	}
	g, err := b.c.ProgramTaskGet(ctx, b.prog, TaskIDRequest{Task: ref.ID})
	if err != nil {
		t.Fatal(err)
	}
	validOut(t, b.bench, "p1-task-get-response", g)
	cr, err := b.c.ProgramCancel(ctx, b.prog, TaskIDRequest{Task: ref.ID})
	if err != nil || cr.State != "canceled" || cr.Reason != "initiator" {
		t.Fatalf("cancel %+v %v", cr, err)
	}
	validOut(t, b.bench, "p1-cancel-response", cr)
	if again, err := b.c.ProgramCancel(ctx, b.prog, TaskIDRequest{Task: ref.ID}); err != nil || *again != *cr {
		t.Fatalf("repeat %+v %v", again, err)
	}
	var events int
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE subject = $1 AND type = 'task.canceled'`, ref.ID).Scan(&events); err != nil || events != 1 {
		t.Fatalf("task.canceled events %d %v", events, err)
	}

	// P2 task.cancel: alice's own task, then an operator's cancel of another task.
	d, err := b.c.TaskCancel(ctx, aliceOp, P2TaskRequest{Op: "task.cancel", Task: alice})
	if err != nil || d.State != "canceled" {
		t.Fatalf("task.cancel %+v %v", d, err)
	}
	validOut(t, b.bench, "done-response", d)
	other := b.aliceTask(t, "2")
	if _, err := b.c.TaskCancel(ctx, b.op, P2TaskRequest{Op: "task.cancel", Task: other}); err != nil {
		t.Fatal(err)
	}
	var reasons []string
	for _, id := range []string{alice, other} {
		var r string
		if err := b.c.st.Pool.QueryRow(ctx, `SELECT reason FROM tasks WHERE id = $1`, id).Scan(&r); err != nil {
			t.Fatal(err)
		}
		reasons = append(reasons, r)
	}
	if reasons[0] != "initiator" || reasons[1] != "admin" {
		t.Fatalf("reasons %v", reasons)
	}
	done := b.aliceTask(t, "3")
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET state = 'completed' WHERE id = $1`, done); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.TaskCancel(ctx, aliceOp, P2TaskRequest{Op: "task.cancel", Task: done}); reasonOf(err) != "already_terminal" {
		t.Fatalf("a finished task: %v", err)
	}
}

// P2 task.list, team.tasks and about (PRM-15): the person's own tasks by type title with paging,
// the group's tasks for a person with team.view only of types they may read, and the code text
// of the bot; without team.view the answer names the needed capability.
func TestTaskLists(t *testing.T) {
	ctx := context.Background()
	b := newPeopleBench(t, nil)
	var ids []string
	for _, u := range []string{"1", "2", "3"} {
		ids = append(ids, b.aliceTask(t, u))
	}
	aliceOp := Operator{Principal: b.alice, Tenant: "demo"}
	page, err := b.c.TaskList(ctx, aliceOp, TaskListRequest{Op: "task.list", Limit: 2})
	if err != nil || len(page.Items) != 2 || page.Next == "" || page.Items[0].ID != ids[0] || page.Items[0].Title == "" {
		t.Fatalf("page 1 %+v %v", page, err)
	}
	validOut(t, b.bench, "p2-task-list-response", page)
	rest, err := b.c.TaskList(ctx, aliceOp, TaskListRequest{Op: "task.list", Limit: 2, After: page.Next})
	if err != nil || len(rest.Items) != 1 || rest.Items[0].ID != ids[2] || rest.Next != "" {
		t.Fatalf("page 2 %+v %v", rest, err)
	}
	if none, err := b.c.TaskList(ctx, aliceOp, TaskListRequest{Op: "task.list", State: "completed"}); err != nil || len(none.Items) != 0 {
		t.Fatalf("state filter %+v %v", none, err)
	}
	if mine, err := b.c.TaskList(ctx, b.op, TaskListRequest{Op: "task.list"}); err != nil || len(mine.Items) != 0 {
		t.Fatalf("another person's list %+v %v", mine, err)
	}
	if _, err := b.c.TaskList(ctx, aliceOp, TaskListRequest{Op: "team.tasks", Group: "hello"}); reasonOf(err) != "forbidden" {
		t.Fatalf("without team.view: %v", err)
	}
	if _, err := b.c.Grant(ctx, b.op, GrantRequest{Op: "grant", Subject: b.alice, Group: "leads"}); err != nil {
		t.Fatal(err)
	}
	team, err := b.c.TaskList(ctx, aliceOp, TaskListRequest{Op: "team.tasks"})
	if err != nil || len(team.Items) != 3 {
		t.Fatalf("team %+v %v", team, err)
	}
	validOut(t, b.bench, "p2-task-list-response", team)
	about, err := b.c.TaskList(ctx, aliceOp, TaskListRequest{Op: "about"})
	if err != nil || about.Text == "" || len(about.Items) != 0 {
		t.Fatalf("about %+v %v", about, err)
	}
	validOut(t, b.bench, "p2-task-list-response", about)
}
