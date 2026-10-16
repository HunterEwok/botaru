// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A delivery hands out a result file of a task of its chain by the producer type's
// result_media_types, with the same initiator: an ancestor, or an earlier task of the tree, such
// as a chain's child whose outcome a continuation reports; a file of another tree, or one whose
// producer another principal started (even an ancestor), is not handed out (ART-5).
func TestEarlierChainFile(t *testing.T) {
	ctx := context.Background()
	b := newBenchWith(t, func(root string, ms []map[string]any) []map[string]any {
		for _, m := range ms {
			if m["kind"] == "TaskType" {
				m["spec"].(map[string]any)["result_media_types"] = []any{"image/png"}
			}
		}
		return append(ms, map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Sink", "metadata": map[string]any{"name": "reports", "tenant": "demo"},
			"spec": map[string]any{"driver": "disk", "base": "/srv/reports", "ceiling": "internal"}})
	})
	first := b.hello(t, "1")
	att := b.claim(t, b.agent, "attempt")
	job := *b.agent
	job.Kind = "job"
	r, err := b.c.FilePut(ctx, &job, header(t, att), nil, FilePutRequest{Filename: "plot.png", MediaType: "image/png"}, []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR"))
	if err != nil {
		t.Fatal(err)
	}
	// A child of the producer; one of another tree; two later tasks of the tree outside the
	// producer's line, one with the same initiator (a continuation) and one started by another.
	later, other, sameInit, otherInit, otherChild := b.hello(t, "2"), b.hello(t, "3"), b.hello(t, "4"), b.hello(t, "5"), b.hello(t, "6")
	for task, parent := range map[string]string{later: first, sameInit: "t_none", otherInit: "t_none", otherChild: first} {
		if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('root', $2::text, 'parent', $3::text), created_at = created_at + interval '1 second'
			WHERE id = $1`, task, first, parent); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := b.c.st.Pool.Exec(ctx, `UPDATE tasks SET initiator = 'system' WHERE id = ANY($1)`, []string{otherInit, otherChild}); err != nil {
		t.Fatal(err)
	}
	sw := b.enroll(t, EnrollRequest{Tenant: "demo", Kind: "sink", Target: "reports"})
	for i, c := range []struct {
		task string
		ok   bool
	}{{later, true}, {other, false}, {sameInit, true}, {otherInit, false}, {otherChild, false}} {
		id := fmt.Sprintf("dlv_chain%d", i)
		if _, err := b.c.st.Pool.Exec(ctx, `INSERT INTO deliveries (id, tenant, task, kind, addr, target_kind, target_id, exec_kind, exec_name, state, body, data_mark, dataorigin, created_at, updated_at)
			VALUES ($1, 'demo', $2, 'task_result', 'sink:reports', 'sink', 'reports', 'sink', 'reports', 'planned', $3, 'internal', 'model', botaru_now(), botaru_now())`,
			id, c.task, map[string]any{"files": []any{r.FileRef}}); err != nil {
			t.Fatal(err)
		}
		item := b.claim(t, sw, "delivery")
		if item == nil {
			t.Fatal("no delivery")
		}
		_, err := b.c.FileGet(ctx, sw, header(t, item), FileGetRequest{FileRef: r.FileRef})
		if c.ok != (err == nil) || !c.ok && !strings.Contains(err.Error(), "media_type_not_allowed") {
			t.Fatalf("delivery of %s: %v", c.task, err)
		}
	}
}
