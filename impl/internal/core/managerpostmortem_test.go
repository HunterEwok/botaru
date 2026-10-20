// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Tests of a person's request for the model paragraph of an incident's analysis (MGR-53).

// sev2Postmortem makes disk_full a sev2 class, so the analysis gets a model paragraph only for
// link candidates or a person's request (MGR-53).
func sev2Postmortem(ms []map[string]any) {
	for _, m := range ms {
		if m["kind"] == "IncidentClass" && asMap(m["metadata"])["name"] == "disk_full" {
			asMap(m["spec"])["severity"] = "sev2"
		}
	}
}

// A person asks for the model paragraph of an analysis before the incident resolves, or while the
// analysis waits; afterwards it is too late, and with reading off there is none. Link candidates
// of every rule (GOV-13, GOV-38, TRN-7) count for a paragraph, and the analysis input never has
// them (MGR-53, MGR-54).
func TestPostmortemRequestV03(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	b := newManagerBenchWith(t, nil, []map[string]any{postmortemDoc()}, claimPostmortem, sev2Postmortem)
	b.diskAlarm(t, true, 3)
	b.advance(t, 6*60)
	b.loop(t, "incident")
	list := incidentsOf(t, b, "disk_full")
	if len(list) != 1 {
		t.Fatalf("incidents %+v", list)
	}
	inc := list[0]
	cat, err := b.c.Catalog(ctx)
	if err != nil {
		t.Fatal(err)
	}
	tv := cat.Tenants["demo"]
	ask := func(tv *TenantView, cats ...*Catalog) string {
		cat := cat
		if len(cats) > 0 {
			cat = cats[0]
		}
		var code string
		if err := b.c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			_, code, err = b.c.paragraphAsked(ctx, tx, now, cat, tv, b.alice, inc.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return code
	}
	// A tenant whose incidents get no analysis has nothing to ask for.
	noClaim := *cat
	noClaim.Roles = maps.Clone(cat.Roles)
	role := noClaim.Roles[managerRole(cat)]
	role.Claims = slices.DeleteFunc(slices.Clone(role.Claims), func(c string) bool { return c == postmortemType })
	noClaim.Roles[managerRole(cat)] = role
	if code := ask(tv, &noClaim); code != "no_analysis" {
		t.Fatalf("without the analysis: %s", code)
	}
	off := *tv
	off.Spec.Privacy.Purposes = nil
	if code := ask(&off); code != "reading_off" {
		t.Fatalf("reading off: %s", code)
	}
	if code := ask(tv); code != "requested" {
		t.Fatalf("before resolved: %s", code)
	}
	if shown, err := b.c.IncidentShow(ctx, b.op, IncidentRequest{ID: inc.ID}); err != nil || strings.Contains(string(mustJSON(shown)), "paragraph_requested") {
		t.Fatalf("the request in the incident view %v %v", shown, err)
	}
	// A candidate of the rejected proposal rule, which the input must never show.
	b.exec(t, `INSERT INTO attributions (id, tenant, incident, proposal, state, record, created_at, updated_at, kind)
		VALUES ('att_canary01', 'demo', $1, 'prp_x', 'candidate', '{}', botaru_now(), botaru_now(), 'rejected_proposal')`, inc.ID)
	b.diskAlarm(t, false, 50)
	b.advance(t, 11*60)
	b.loop(t, "incident")
	b.advance(t, 11*60)
	b.loop(t, "incident")
	var task string
	var body, rec []byte
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT id, body, record FROM tasks WHERE type = $1 AND idempotency_key = $2`, postmortemType, "system:postmortem:"+inc.ID).
		Scan(&task, &body, &rec); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rec), `"paragraph": true`) || strings.Contains(string(body), "att_canary01") || strings.Contains(string(body), "candidate") {
		t.Fatalf("analysis record %s body %s", rec, body)
	}
	// While the analysis waits a request sets the paragraph; once it runs it is late.
	b.exec(t, `UPDATE tasks SET record = record || '{"paragraph": false}', body = jsonb_set(body, '{params,input,paragraph}', 'false') WHERE id = $1`, task)
	if code := ask(tv); code != "requested" {
		t.Fatalf("while waiting: %s", code)
	}
	var param bool
	if err := b.c.st.Pool.QueryRow(ctx, `SELECT (body->'params'->'input'->>'paragraph')::boolean FROM tasks WHERE id = $1`, task).Scan(&param); err != nil || !param {
		t.Fatalf("input paragraph %v %v", param, err)
	}
	b.exec(t, `UPDATE tasks SET state = 'working', record = record || '{"paragraph": false}' WHERE id = $1`, task)
	if code := ask(tv); code != "late" {
		t.Fatalf("after the start: %s", code)
	}
	// Every rule's candidates count (MGR-54).
	b.exec(t, `INSERT INTO attributions (id, tenant, incident, script_run, state, record, created_at, updated_at, kind)
		VALUES ('att_script01', 'demo', $1, 'scr_x', 'candidate', '{}', botaru_now(), botaru_now(), 'script_run')`, inc.ID)
	if err := b.c.tx(ctx, func(tx pgx.Tx, _ time.Time) error {
		links, err := linkCandidates(ctx, tx, "demo", inc.ID)
		if err == nil && !slices.Equal(links, []string{"att_canary01", "att_script01"}) {
			t.Errorf("candidates %v", links)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
