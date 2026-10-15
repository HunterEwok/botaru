// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
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
			"revert_reason": r["revert_reason"]}
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
	rs, err = tx.Query(ctx, `SELECT e.data, e.time FROM events e WHERE e.tenant = $1 AND e.type = $2
		AND EXISTS (SELECT 1 FROM leases l WHERE l.work_id = e.data->>'task' AND l.kind = 'attempt' AND l.worker_id = $3)`, tenant, proposalRejectedType, worker)
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
