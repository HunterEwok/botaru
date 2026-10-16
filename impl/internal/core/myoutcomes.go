// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
)

// myOutcomesRows is how many rows of "My outcomes" go into an analysis task (INS-5).
const myOutcomesRows = 20

// myOutcomes is the closed table "My outcomes" of one author (DIG-7): its proposals and its
// refusals at creation, with only the closed fields; no reason codes of people's decisions or
// refusals, no text, no names, no lens scores. Rows with a measured trial outcome come first,
// then rows with only a human decision, then the rest; the newest first within each, at most
// limit rows. While bots have no identity the author is the worker.
func myOutcomes(ctx context.Context, tx pgx.Tx, tenant, worker string, limit int) (map[string]any, error) {
	type row struct {
		rank int
		at   string
		v    map[string]any
	}
	var rows []row
	rs, err := tx.Query(ctx, `SELECT id, state, record, created_at FROM proposals WHERE tenant = $1 AND record->'origin'->>'worker' = $2`, tenant, worker)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var id, state string
		var raw []byte
		var at time.Time
		if err := rs.Scan(&id, &state, &raw, &at); err != nil {
			rs.Close()
			return nil, err
		}
		p := &proposalRow{ID: id, State: state}
		if err := json.Unmarshal(raw, &p.Rec); err != nil {
			rs.Close()
			return nil, err
		}
		b := p.body()
		var human any
		switch d := asString(mmapAny(p.Rec["decision"])["decision"]); {
		case d == "accept":
			human = "accepted"
		case d == "reject":
			human = "declined"
		case d == "defer":
			human = "parked"
		}
		var promised any
		if b.Benefit != nil {
			pm := map[string]any{"direction": b.Benefit.Direction}
			if b.Benefit.ExpectedChangePP != nil {
				pm["expected_change_pp"] = *b.Benefit.ExpectedChangePP
			}
			if b.Benefit.ExpectedChangePct != nil {
				pm["expected_change_pct"] = *b.Benefit.ExpectedChangePct
			}
			promised = pm
		}
		t := mmapAny(p.Rec["trial"])
		r := mmapAny(p.Rec["realization"])
		v := map[string]any{"id": id, "rejected": false, "category": b.Category, "problem_code": b.Problem.Code, "state": state,
			"human_outcome": human, "promised": promised, "trial_outcome": t["outcome"], "self_measured": t["self_measured"] == true,
			"revert_reason": r["revert_reason"], "trial_numbers": trialNumbers(t)}
		rank := 2
		switch {
		case t["outcome"] != nil:
			rank = 0
		case human != nil:
			rank = 1
		}
		rows = append(rows, row{rank: rank, at: fmtTime(at), v: v})
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	// A refusal is the worker's that handed in the body; an event from before the worker was
	// written falls back to the workers that held an attempt of the task (DIG-7).
	rs, err = tx.Query(ctx, `SELECT e.data, e.time FROM events e WHERE e.tenant = $1 AND e.type = $2
		AND (e.data->>'worker' = $3 OR e.data->>'worker' IS NULL
			AND EXISTS (SELECT 1 FROM leases l WHERE l.work_id = e.data->>'task' AND l.kind = 'attempt' AND l.worker_id = $3))`, tenant, proposalRejectedType, worker)
	if err != nil {
		return nil, err
	}
	for rs.Next() {
		var data map[string]any
		var at time.Time
		if err := rs.Scan(&data, &at); err != nil {
			rs.Close()
			return nil, err
		}
		if data["category"] == nil || data["problem_code"] == nil {
			// A refusal before the body was read has no closed fields to show.
			continue
		}
		rows = append(rows, row{rank: 2, at: fmtTime(at), v: map[string]any{"id": nil, "rejected": true, "category": data["category"],
			"problem_code": data["problem_code"], "state": nil, "human_outcome": nil, "promised": nil, "trial_outcome": nil}})
	}
	rs.Close()
	if err := rs.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].rank != rows[j].rank {
			return rows[i].rank < rows[j].rank
		}
		return rows[i].at > rows[j].at
	})
	out := []any{}
	for i, r := range rows {
		if i >= limit {
			break
		}
		out = append(out, r.v)
	}
	return map[string]any{"bot": worker, "rows": out}, nil
}

// trialNumbers are the trial figures of a row once the outcome is computed (DIG-8): the metric
// before and after over all points, the change over all points, without the author and
// without the author's role, and the guard that broke, if any; nil before the outcome.
func trialNumbers(t map[string]any) map[string]any {
	if t == nil || t["outcome"] == nil || t["outcome"] == "canceled" {
		return nil
	}
	before, after := mmapAny(t["before"]), mmapAny(t["after"])
	delta := func(b, a map[string]any) any {
		bv, ok1 := toFloat(b["value"])
		av, ok2 := toFloat(a["value"])
		if !ok1 || !ok2 {
			return nil
		}
		return math.Round((av-bv)*1e6) / 1e6
	}
	out := map[string]any{"before": numOrNil(before["value"]), "after": numOrNil(after["value"]), "change_all": delta(before, after),
		"change_without_author": delta(mmapAny(before["without_author"]), mmapAny(after["without_author"])),
		"change_without_role":   delta(mmapAny(before["without_role"]), mmapAny(after["without_role"])), "guard_broken": nil}
	for _, g := range asList(t["guards"]) {
		if gm := mmapAny(g); gm["broken_at"] != nil {
			out["guard_broken"] = gm["series"]
			break
		}
	}
	return out
}

func numOrNil(v any) any {
	if n, ok := toFloat(v); ok {
		return n
	}
	return nil
}
