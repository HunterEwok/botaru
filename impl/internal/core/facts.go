// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// Facts of bot memory and their offers to a successor (MEM-6, MEM-7, MEM-9, spec/lifecycles/
// fact.md and fact-offer.md). The model names only the kind, the predicate and the subject; the
// core takes the window and the evidence by the predicate and computes the value. A fact lives
// in the space of the identity whose attempt proposed it and never crosses tenants.

const (
	factWindow            = 14 * 24 * time.Hour
	defaultFactsMaxActive = 100
	defaultFactsPerTry    = 3
	defaultOfferMax       = 100
	// failsWithShare is the smallest share of a reason among the attempts of a type for
	// task.fails_with; the skeleton leaves it to the predicate's ADR (MEM-6).
	failsWithShare = 0.2
	// durationTolerance is the recheck tolerance of task.duration.p95_min.
	durationTolerance = 0.1
)

// factMinSamples is the smallest number of evidence items of a predicate: the outcome
// min_samples of the core series it reads (spec/model/core-series.yaml).
var factMinSamples = map[string]int{"task.duration.p95_min": 10, "task.fails_with": 14}

// factSubject is the subject of a fact.
type factSubject struct {
	Type string `json:"type"`
	ID   string `json:"id"`
	On   string `json:"on,omitempty"`
}

// factValue is what the core derives for a predicate over a window.
type factValue struct {
	Value    any
	Unit     string
	Evidence []map[string]any
	Mark     string
}

// computeFact derives the value of a predicate for a subject over a window from the records of
// the tenant; ok is false when there is too little evidence. In this version the core derives
// task.duration.p95_min and task.fails_with; the other predicates need records it does not keep
// yet (datasets, catalog scopes of attempts, free space series, incidents), and their proposals
// end as fact_insufficient.
func computeFact(ctx context.Context, q store.Querier, tenant, predicate string, s factSubject, from, to time.Time) (*factValue, bool, error) {
	switch predicate {
	case "task.duration.p95_min":
		var n int
		var p95 *float64
		err := q.QueryRow(ctx, `SELECT count(*), percentile_disc(0.95) WITHIN GROUP (ORDER BY num) FROM obs_points
			WHERE tenant = $1 AND series = $2 AND subject = $3 AND at >= $4 AND at < $5 AND num IS NOT NULL`,
			tenant, seriesAttemptTime, "task_type:"+s.ID, from, to).Scan(&n, &p95)
		if err != nil || n < factMinSamples[predicate] || p95 == nil {
			return nil, false, err
		}
		return &factValue{Value: math.Round(*p95/6) / 10, Unit: "min", Mark: coreSeriesMark,
			Evidence: []map[string]any{{"kind": "obs_points", "series": seriesAttemptTime, "samples": n}}}, true, nil
	case "task.fails_with":
		rows, err := q.Query(ctx, `SELECT enum, count(*) FROM obs_points
			WHERE tenant = $1 AND series = $2 AND subject = $3 AND at >= $4 AND at < $5 AND enum IS NOT NULL GROUP BY enum ORDER BY count(*) DESC, enum`,
			tenant, seriesTaskOutcome, "task_type:"+s.ID, from, to)
		if err != nil {
			return nil, false, err
		}
		type row struct {
			code string
			n    int
		}
		list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
			var x row
			return x, r.Scan(&x.code, &x.n)
		})
		if err != nil {
			return nil, false, err
		}
		total := 0
		for _, r := range list {
			total += r.n
		}
		if total < factMinSamples[predicate] {
			return nil, false, nil
		}
		for _, r := range list {
			if r.code != "done" && float64(r.n)/float64(total) >= failsWithShare {
				return &factValue{Value: r.code, Mark: coreSeriesMark,
					Evidence: []map[string]any{{"kind": "obs_points", "series": seriesTaskOutcome, "samples": total}}}, true, nil
			}
		}
	}
	return nil, false, nil
}

// sameValue says whether a recomputed value agrees with the recorded one.
func sameValue(a, b any) bool {
	x, xok := a.(float64)
	y, yok := b.(float64)
	if xok && yok {
		return math.Abs(x-y) <= durationTolerance*math.Max(math.Abs(x), math.Abs(y))
	}
	return a == b
}

// factLimit is a numeric field of the charter's memory block that only goes down (GOV-33).
func factLimit(cat *Catalog, tenant string, path []string, def int) int {
	m := mmap(tenantSpecOf(cat, "Governance", tenant), "memory")
	for _, k := range path[:len(path)-1] {
		m = mmap(m, k)
	}
	if v, ok := mnum(m, path[len(path)-1]); ok && int(v) < def {
		return int(v)
	}
	return def
}

// factSubjectOK says whether a subject is an object of the tenant; a task type is checked
// against the catalog, and the other kinds against the predicates that use them.
func factSubjectOK(cat *Catalog, s factSubject) bool {
	if s.Type == "tasktype" {
		_, ok := cat.TaskTypes[s.ID]
		return ok
	}
	return true
}

// proposeFacts writes the facts an attempt proposed, in the transaction of its successful
// complete (MEM-6): only for a type with memory.facts: propose, a role with bot memory and an
// attempt on behalf of the identity itself (a seat holder writes none). A refused proposal
// leaves only the event fact.rejected.v1.
func (c *Core) proposeFacts(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, l *leaseRow, record map[string]any, raw any) error {
	if raw == nil || l.Acting.Bot == "" || l.Acting.Seat != "" || roleMemory(cat.Roles[w.Role]) != "bot" {
		return nil
	}
	reject := func(reason, predicate string) error {
		data := map[string]any{"task": l.WorkID, "reason": reason}
		if predicate != "" {
			data["predicate"] = predicate
		}
		return c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "fact.rejected.v1", Subject: l.WorkID, Data: data})
	}
	list, _ := raw.([]any)
	if c.spec.Validate("urn:botaru:schema:fact:1#/$defs/proposals", raw) != nil {
		return reject("output_schema_invalid", "")
	}
	reg, err := c.spec.Registry("spec/model/fact-predicates.yaml")
	if err != nil {
		return err
	}
	per := factLimit(cat, w.Tenant, []string{"facts", "per_attempt"}, defaultFactsPerTry)
	untrusted, _ := record["untrusted_seen"].(bool)
	personal := record["personal_seen"] != nil
	for i, item := range list {
		m, _ := item.(map[string]any)
		predicate := mstr(m, "predicate")
		if i >= per {
			if err := reject("output_schema_invalid", predicate); err != nil {
				return err
			}
			continue
		}
		var s factSubject
		b, _ := json.Marshal(m["subject"])
		_ = json.Unmarshal(b, &s)
		e, ok := reg.Entry(predicate)
		if !ok || mstr(e, "kind") != mstr(m, "kind") || !slices.Contains(mstrs(e, "subject_types"), s.Type) {
			if err := reject("output_schema_invalid", predicate); err != nil {
				return err
			}
			continue
		}
		if !factSubjectOK(cat, s) {
			if err := reject("not_found", predicate); err != nil {
				return err
			}
			continue
		}
		from := now.Add(-factWindow)
		v, ok, err := computeFact(ctx, tx, w.Tenant, predicate, s, from, now)
		if err != nil {
			return err
		}
		if !ok {
			if err := reject("fact_insufficient", predicate); err != nil {
				return err
			}
			continue
		}
		if _, err := c.insertFact(ctx, tx, now, w.Tenant, l.Acting.Bot, mstr(m, "kind"), predicate, s, v, from, now, untrusted, personal, l.Acting, nil); err != nil {
			return err
		}
	}
	return c.evictFacts(ctx, tx, w.Tenant, l.Acting.Bot, factLimit(cat, w.Tenant, []string{"facts", "max_active"}, defaultFactsMaxActive))
}

// insertFact writes an active fact into the space of an identity.
func (c *Core) insertFact(ctx context.Context, tx pgx.Tx, now time.Time, tenant, bot, kind, predicate string, s factSubject, v *factValue, from, to time.Time,
	untrusted, personal bool, author actingOf, inherited map[string]any) (string, error) {
	id := ids.New(ids.Fact)
	subj, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	val, err := json.Marshal(v.Value)
	if err != nil {
		return "", err
	}
	ev, err := json.Marshal(v.Evidence)
	if err != nil {
		return "", err
	}
	var inh []byte
	if inherited != nil {
		if inh, err = json.Marshal(inherited); err != nil {
			return "", err
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO facts (id, tenant, bot, kind, predicate, subject, value, unit, window_from, window_to, evidence, state,
		untrusted_origin, personal_seen, data_mark, verified_at, acting_bot, bot_epoch, inherited, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'active', $12, $13, $14, $15, $16, $17, $18, $15)`,
		id, tenant, bot, kind, predicate, subj, val, nullable(v.Unit), from, to, ev, untrusted, personal, v.Mark, now,
		nullable(author.Bot), nullableInt(author.BotEpoch), inh)
	return id, err
}

func nullableInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

// evictFacts keeps at most max active facts in a space: the one used longest ago goes first.
func (c *Core) evictFacts(ctx context.Context, tx pgx.Tx, tenant, bot string, max int) error {
	_, err := tx.Exec(ctx, `DELETE FROM facts WHERE id IN (SELECT id FROM facts WHERE tenant = $1 AND bot = $2 AND state = 'active'
		ORDER BY coalesce(last_used_at, created_at) DESC, id DESC OFFSET $3)`, tenant, bot, max)
	return err
}

// factRow is a stored fact.
type factRow struct {
	ID, Kind, Predicate, Unit, Mark string
	Subject                         factSubject
	Value                           any
	From, To                        time.Time
	Untrusted, Personal             bool
	LastUsed                        *time.Time
	Created                         time.Time
}

func loadFacts(ctx context.Context, q store.Querier, tenant, bot string, states []string) ([]factRow, error) {
	rows, err := q.Query(ctx, `SELECT id, kind, predicate, coalesce(unit, ''), data_mark, subject, value, window_from, window_to, untrusted_origin, personal_seen, last_used_at, created_at
		FROM facts WHERE tenant = $1 AND bot = $2 AND state = ANY($3) ORDER BY coalesce(last_used_at, created_at) DESC, id DESC`, tenant, bot, states)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (factRow, error) {
		var f factRow
		var subj, val []byte
		if err := r.Scan(&f.ID, &f.Kind, &f.Predicate, &f.Unit, &f.Mark, &subj, &val, &f.From, &f.To, &f.Untrusted, &f.Personal, &f.LastUsed, &f.Created); err != nil {
			return f, err
		}
		if err := json.Unmarshal(subj, &f.Subject); err != nil {
			return f, err
		}
		return f, json.Unmarshal(val, &f.Value)
	})
}

// recheck computes a fact again over its own window: erased evidence makes it stale, a value
// that moved is fact_mismatch (MEM-6).
func recheck(ctx context.Context, q store.Querier, tenant string, f factRow) (*factValue, string, error) {
	v, ok, err := computeFact(ctx, q, tenant, f.Predicate, f.Subject, f.From, f.To)
	if err != nil {
		return nil, "", err
	}
	if !ok {
		return nil, "fact_insufficient", nil
	}
	if !sameValue(v.Value, f.Value) {
		return nil, "fact_mismatch", nil
	}
	return v, "", nil
}

// taskFacts are the facts an attempt reads (MEM-6): for a type with memory.read: bot and a
// role with bot memory or reading it, the active facts of the acting identity whose mark the
// task covers, for review and manage only those of trusted origin. The view has no author.
// Reading a fact does not mark the task untrusted; a fact with personal_seen marks it.
func (c *Core) taskFacts(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, typ, task string, acting actingOf, record map[string]any) ([]any, error) {
	tt := cat.TaskTypes[typ]
	if acting.Bot == "" || tt.Memory == nil || tt.Memory.Read != "bot" {
		return nil, nil
	}
	if m := roleMemory(cat.Roles[w.Role]); m != "bot" && m != "read_bot" {
		return nil, nil
	}
	list, err := loadFacts(ctx, tx, w.Tenant, acting.Bot, []string{"active"})
	if err != nil {
		return nil, err
	}
	mark, _ := record["data_mark"].(string)
	out := []any{}
	var used []string
	personal := false
	for _, f := range list {
		if (tt.Purpose == "review" || tt.Purpose == "manage") && f.Untrusted {
			continue
		}
		if mark == "" || c.higherMark(mark, f.Mark) != mark {
			continue
		}
		view := map[string]any{"kind": f.Kind, "predicate": f.Predicate, "subject": f.Subject, "value": f.Value,
			"window": map[string]any{"from": fmtTime(f.From), "to": fmtTime(f.To)}, "age_days": int(now.Sub(f.Created).Hours() / 24)}
		if f.Unit != "" {
			view["unit"] = f.Unit
		}
		out = append(out, view)
		used = append(used, f.ID)
		personal = personal || f.Personal
	}
	if len(used) > 0 {
		if _, err := tx.Exec(ctx, `UPDATE facts SET uses = uses + 1, last_used_at = $2 WHERE id = ANY($1)`, used, now); err != nil {
			return nil, err
		}
	}
	if personal {
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = jsonb_set(record, '{personal_seen}', coalesce(record->'personal_seen', '[]'::jsonb) || '["fact"]'::jsonb)
			WHERE id = $1 AND NOT coalesce(record->'personal_seen', '[]'::jsonb) ? 'fact'`, task); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// offerFacts creates the fact offer of an identity that retires or changes its binding (MEM-7):
// its active facts, each rechecked (a failed one goes stale), at most offer.max, open until the
// end of its seal. A successor that exists gets the copies at once; otherwise the offer waits
// for the next holder of seat, or for the first identity enrolled in the role.
func (c *Core) offerFacts(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, from *botRow, successor, seat string, openUntil time.Time) error {
	list, err := loadFacts(ctx, tx, from.Tenant, from.ID, []string{"active"})
	if err != nil {
		return err
	}
	max := factLimit(cat, from.Tenant, []string{"offer", "max"}, defaultOfferMax)
	var offered []string
	for _, f := range list {
		if len(offered) >= max {
			break
		}
		if _, reason, err := recheck(ctx, tx, from.Tenant, f); err != nil {
			return err
		} else if reason != "" {
			if _, err := tx.Exec(ctx, `UPDATE facts SET state = 'stale' WHERE id = $1`, f.ID); err != nil {
				return err
			}
			continue
		}
		offered = append(offered, f.ID)
	}
	if len(offered) == 0 {
		return nil
	}
	id := ids.New(ids.FactOffer)
	if _, err := tx.Exec(ctx, `INSERT INTO fact_offers (id, tenant, from_bot, role, seat, facts, state, recipient_state, open_until, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'open', 'waiting', $7, $8)`, id, from.Tenant, from.ID, from.Role, nullable(seat), offered, openUntil, now); err != nil {
		return err
	}
	if successor != "" {
		return c.copyOffer(ctx, tx, now, cat, id, successor)
	}
	return nil
}

// copyOffer copies the facts of an offer to its successor (MEM-7): each fact is rechecked with
// the successor's rights and copied with the mark inherited, the most recently used first, while
// the successor's space has room; a role without bot memory gets nothing. The offer closes.
func (c *Core) copyOffer(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, offer, successor string) error {
	var tenant, fromBot string
	var factIDs []string
	err := tx.QueryRow(ctx, `SELECT tenant, from_bot, facts FROM fact_offers WHERE id = $1 AND state = 'open' FOR UPDATE`, offer).Scan(&tenant, &fromBot, &factIDs)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	to, err := loadBot(ctx, tx, successor, false)
	if err != nil {
		return err
	}
	copied := []string{}
	if to != nil && to.Tenant == tenant && roleMemory(cat.Roles[to.Role]) == "bot" {
		var room int
		if err := tx.QueryRow(ctx, `SELECT $3 - count(*) FROM facts WHERE tenant = $1 AND bot = $2 AND state = 'active'`, tenant, to.ID,
			factLimit(cat, tenant, []string{"facts", "max_active"}, defaultFactsMaxActive)).Scan(&room); err != nil {
			return err
		}
		list, err := loadFacts(ctx, tx, tenant, fromBot, []string{"active", "sealed"})
		if err != nil {
			return err
		}
		for _, f := range list {
			if room <= 0 {
				break
			}
			if !slices.Contains(factIDs, f.ID) || !factSubjectOK(cat, f.Subject) {
				continue
			}
			v, reason, err := recheck(ctx, tx, tenant, f)
			if err != nil {
				return err
			}
			if reason != "" {
				continue
			}
			kind := f.Kind
			nid, err := c.insertFact(ctx, tx, now, tenant, to.ID, kind, f.Predicate, f.Subject, v, f.From, f.To, f.Untrusted, f.Personal, actingOf{},
				map[string]any{"from_bot": fromBot, "offer": offer, "at": fmtTime(now)})
			if err != nil {
				return err
			}
			copied = append(copied, nid)
			room--
		}
	}
	_, err = tx.Exec(ctx, `UPDATE fact_offers SET state = 'closed', recipient = $2, recipient_state = 'copied', copied = $3, closed_at = $4 WHERE id = $1`,
		offer, successor, copied, now)
	return err
}

// pickUpOffers gives a new identity the waiting offers it succeeds to: those of the seat it
// took, or, for a new identity of a worker, those of its role without a seat (MEM-7).
func (c *Core) pickUpOffers(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, role, seat, bot string) error {
	var rows pgx.Rows
	var err error
	if seat != "" {
		rows, err = tx.Query(ctx, `SELECT id FROM fact_offers WHERE tenant = $1 AND seat = $2 AND state = 'open' AND recipient_state = 'waiting' AND open_until > $3 AND from_bot <> $4 ORDER BY created_at, id`,
			tenant, seat, now, bot)
	} else {
		rows, err = tx.Query(ctx, `SELECT id FROM fact_offers WHERE tenant = $1 AND role = $2 AND seat IS NULL AND state = 'open' AND recipient_state = 'waiting' AND open_until > $3 AND from_bot <> $4 ORDER BY created_at, id`,
			tenant, role, now, bot)
	}
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range list {
		if err := c.copyOffer(ctx, tx, now, cat, id, bot); err != nil {
			return err
		}
	}
	return nil
}

// expireOffers closes the offers whose successor did not come before the end of the seal; their
// facts go with the seal (MEM-7, MEM-9).
func expireOffers(ctx context.Context, tx pgx.Tx, now time.Time) error {
	_, err := tx.Exec(ctx, `UPDATE fact_offers SET state = 'closed', recipient_state = 'expired', closed_at = $1 WHERE state = 'open' AND open_until <= $1`, now)
	return err
}

// sealFacts seals the facts of a retired identity: no bot reads them any more (MEM-4).
func sealFacts(ctx context.Context, tx pgx.Tx, b *botRow) error {
	_, err := tx.Exec(ctx, `UPDATE facts SET state = 'sealed' WHERE tenant = $1 AND bot = $2 AND state IN ('active', 'stale')`, b.Tenant, b.ID)
	return err
}

// eraseBotFacts wipes the facts of an identity and closes its offers (MEM-9, RET-7).
func eraseBotFacts(ctx context.Context, tx pgx.Tx, now time.Time, bot string) error {
	if _, err := tx.Exec(ctx, `DELETE FROM facts WHERE bot = $1`, bot); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE fact_offers SET state = 'closed', recipient_state = CASE WHEN recipient_state = 'waiting' THEN 'expired' ELSE recipient_state END,
		closed_at = coalesce(closed_at, $2) WHERE from_bot = $1 AND state = 'open'`, bot, now)
	return err
}
