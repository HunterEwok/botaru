// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The default periods of the retention classes (spec/model/retention-classes.yaml, RET-1,
// RET-2) and the 93 days a person's usage line keeps its subject (BUD-16, SEC-14).
const (
	sessionsNoMemoryDays    = 7
	metadataDays            = 400
	personUsageDays         = 93
	metadataCeilingDays     = 3650
	retentionBatch          = 500
	retentionMaxBatchesTick = 20
	day                     = 24 * time.Hour
)

// retentionPeriods are the periods of one tenant (RET-9): content, the file period (never
// longer than content), memory (0: until forget or an erasure request) and metadata.
type retentionPeriods struct {
	content, files, memory, metadata time.Duration
}

// periodsOf reads the periods a tenant set and takes the class default for the rest; a tenant
// gone from the catalog (nil) has the defaults.
func periodsOf(tv *TenantView) retentionPeriods {
	p := retentionPeriods{content: contentDays * day, metadata: metadataDays * day}
	if tv != nil {
		cls := tv.Spec.Retention.Classes
		if d, ok := parseDuration(cls["content"]); ok {
			p.content = d
		}
		if d, ok := parseDuration(cls["memory"]); ok {
			p.memory = d
		}
		if d, ok := parseDuration(cls["metadata"]); ok {
			p.metadata = d
		}
	}
	p.files = p.content
	if tv != nil {
		if d, ok := parseDuration(tv.Spec.Retention.Files); ok && d < p.files {
			p.files = d
		}
	}
	return p
}

// recordMetadataPeriods writes, in the apply transaction, the metadata period of each tenant
// whose period changed: a record keeps the period in force when it was written, or the
// current one if that is longer, so a shorter period never reaches records already written
// (RET-9, D-108 (1), AUD-3).
func recordMetadataPeriods(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) error {
	for _, name := range slices.Sorted(mapKeys(cat.Tenants)) {
		cur := int64(periodsOf(cat.Tenants[name]).metadata / time.Second)
		var last int64
		err := tx.QueryRow(ctx, `SELECT seconds FROM retention_metadata_periods WHERE tenant = $1 ORDER BY since DESC LIMIT 1`, name).Scan(&last)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			last = metadataDays * 86400
		case err != nil:
			return err
		}
		if last == cur {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO retention_metadata_periods (tenant, seconds, since) VALUES ($1, $2, $3)
			ON CONFLICT (tenant, since) DO UPDATE SET seconds = EXCLUDED.seconds`, name, cur, now); err != nil {
			return err
		}
	}
	return nil
}

// metaPeriod is one recorded metadata period of a tenant, in force from since.
type metaPeriod struct {
	since time.Time
	d     time.Duration
}

// monthStart is the first instant of the UTC calendar month of t.
func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// span is a range of time [from, to); a zero from is open to the past.
type span struct{ from, to time.Time }

// expiredMonths gives the calendar months whose metadata records are all past their period, as
// merged spans: the loop deletes whole months and never a single row (RET-10, AUD-3). A record
// has the period in force when it was written, or the current period cur when that is longer
// (D-108 (1)); hist is sorted by since, and before its first entry the class default holds.
// Months older than the longest period a tenant may set are in the first span, open to the past.
func expiredMonths(now time.Time, hist []metaPeriod, cur time.Duration) []span {
	type seg struct {
		from, to time.Time // a zero from: since always
		d        time.Duration
	}
	var segs []seg
	from, d := time.Time{}, time.Duration(metadataDays)*day
	for _, h := range hist {
		segs = append(segs, seg{from, h.since, d})
		from, d = h.since, h.d
	}
	segs = append(segs, seg{from, now, d})
	expired := func(ms, me time.Time) bool {
		for _, s := range segs {
			if !s.from.IsZero() && !s.from.Before(me) || !s.to.After(ms) {
				continue
			}
			end := s.to
			if me.Before(end) {
				end = me
			}
			if end.Add(max(s.d, cur)).After(now) {
				return false
			}
		}
		return true
	}
	var out []span
	floor := monthStart(now.AddDate(0, 0, -metadataCeilingDays))
	if expired(time.Time{}, floor) {
		out = append(out, span{time.Time{}, floor})
	}
	for m := floor; m.Before(monthStart(now)); m = m.AddDate(0, 1, 0) {
		next := m.AddDate(0, 1, 0)
		if !expired(m, next) {
			continue
		}
		if n := len(out); n > 0 && out[n-1].to.Equal(m) {
			out[n-1].to = next
		} else {
			out = append(out, span{m, next})
		}
	}
	return out
}

// retentionCuts is the head of every deletion by a tenant period: p holds the cuts of each
// tenant in the catalog (what was last touched at or before a cut is past its period; a tenant
// without a memory period has the zero time, which nothing is older than), $6 and $7 are the
// content cut and the cut of a role without memory for a tenant gone from the catalog, $8 the
// batch and $9 the latest cut of any tenant, a bound for the age indexes.
const retentionCuts = `WITH p (tenant, content, short, files, memory) AS (SELECT * FROM unnest($1::text[], $2::timestamptz[], $3::timestamptz[], $4::timestamptz[], $5::timestamptz[])),
	q AS (SELECT $6::timestamptz AS content, $7::timestamptz AS short, $8::int AS batch, $9::timestamptz AS latest) `

// retentionLoop deletes what is past its period without an erasure request, in every copy the
// core keeps (RET-2, RET-10): the body and result of a finished task (its row stays as a
// tombstone that keeps the idempotency key, RET-3), the body of a finished delivery, the
// arguments and output of a finished tool call, a session that has not changed for the period
// (7 days for a role without memory) whether or not its bot's seal runs, the bytes of a file past
// its expiry or the file period, an ended entry of a bot message with its fields, and, when the
// tenant set a memory period, memory records and facts, sealed ones too (MEM-10). The periods
// are the tenant's (RET-9), so a shorter one reaches what is already stored at the next run.
// Metadata goes only in whole calendar months (audit, events, usage lines), and a person's usage
// line loses its subject after 93 days (BUD-16, SEC-14). The loop is driven by the age of each
// object only, so after a restore it deletes again what the backup brought back.
// Each pass deletes in bounded batches, so a long backlog does not hold one transaction.
func (c *Core) retentionLoop(ctx context.Context) error {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	var pass bool
	var start time.Time
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		pass, start = c.slowPass(&c.retentionAt, now), now
		return nil
	}); err != nil || !pass {
		return err
	}
	var noMemory []string
	for name, r := range cat.Roles {
		if roleMemory(r) == "none" {
			noMemory = append(noMemory, name)
		}
	}
	var tenants []string
	var content, short, files, memory []time.Time
	def := periodsOf(nil)
	defShort := start.Add(-min(def.content, sessionsNoMemoryDays*day))
	latest := defShort
	for _, name := range slices.Sorted(mapKeys(cat.Tenants)) {
		p := periodsOf(cat.Tenants[name])
		tenants = append(tenants, name)
		content = append(content, start.Add(-p.content))
		s := start.Add(-min(p.content, sessionsNoMemoryDays*day))
		short = append(short, s)
		files = append(files, start.Add(-p.files))
		latest = maxTime(maxTime(latest, s), start.Add(-p.files))
		var m time.Time
		if p.memory > 0 {
			m = start.Add(-p.memory)
		}
		memory = append(memory, m)
	}
	base := []any{tenants, content, short, files, memory, start.Add(-def.content), defShort, retentionBatch, latest}
	with := func(extra ...any) []any { return append(slices.Clone(base), extra...) }
	exec := func(sql string, args []any) func(tx pgx.Tx, now time.Time) (int64, error) {
		return func(tx pgx.Tx, now time.Time) (int64, error) {
			tag, err := tx.Exec(ctx, retentionCuts+sql, args...)
			return tag.RowsAffected(), err
		}
	}
	steps := []func(tx pgx.Tx, now time.Time) (int64, error){
		exec(`DELETE FROM activity_outcomes WHERE (tenant, key) IN (SELECT a.tenant, a.key FROM activity_outcomes a LEFT JOIN p ON p.tenant = a.tenant
			WHERE a.created_at <= $9 AND a.created_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF a SKIP LOCKED)`, base),
		exec(`UPDATE tasks SET body = NULL, result = NULL WHERE id IN (SELECT t.id FROM tasks t LEFT JOIN p ON p.tenant = t.tenant
			WHERE (t.body IS NOT NULL OR t.result IS NOT NULL) AND t.state IN ('completed', 'failed', 'canceled') AND t.updated_at <= $9 AND t.updated_at <= coalesce(p.content, $6)
			ORDER BY t.updated_at LIMIT $8 FOR UPDATE OF t SKIP LOCKED)`, base),
		exec(`UPDATE deliveries SET body = NULL WHERE id IN (SELECT d.id FROM deliveries d LEFT JOIN p ON p.tenant = d.tenant
			WHERE d.body IS NOT NULL AND d.state IN ('delivered', 'dead', 'withheld') AND d.updated_at <= $9 AND d.updated_at <= coalesce(p.content, $6)
			LIMIT $8 FOR UPDATE OF d SKIP LOCKED)`, base),
		exec(`UPDATE toolcalls SET args = '{}', output = NULL WHERE id IN (SELECT c.id FROM toolcalls c LEFT JOIN p ON p.tenant = c.tenant
			WHERE (c.args <> '{}' OR c.output IS NOT NULL) AND c.state NOT IN ('requested', 'queued', 'leased') AND c.updated_at <= $9 AND c.updated_at <= coalesce(p.content, $6)
			LIMIT $8 FOR UPDATE OF c SKIP LOCKED)`, base),
		// C3 prune (RET-10): a session of a retired identity goes at its period too (MEM-10).
		exec(`DELETE FROM sessions WHERE id IN (SELECT s.id FROM sessions s LEFT JOIN p ON p.tenant = s.tenant
			WHERE s.updated_at <= $9 AND (s.updated_at <= coalesce(p.content, $6) OR (s.role = ANY($10) AND s.updated_at <= coalesce(p.short, $7)))
			LIMIT $8 FOR UPDATE OF s SKIP LOCKED)`, with(noMemory)),
		// A file past its expiry, or past the tenant's file period as it stands now (RET-9).
		exec(`UPDATE files SET state = 'expired', content = NULL WHERE file_ref IN (SELECT f.file_ref FROM files f LEFT JOIN p ON p.tenant = f.tenant
			WHERE f.state = 'stored' AND (f.expires_at <= $10 OR f.created_at <= coalesce(p.files, $6)) LIMIT $8 FOR UPDATE OF f SKIP LOCKED)`, with(start)),
		// An entry of a bot message that ended (acknowledged, discarded, dead, expired, replaced,
		// removed, or a state value gone stale) leaves the store with its fields after the
		// period; the audit and the events keep its metadata (MSG-3, MSG-12).
		exec(`DELETE FROM messages WHERE seq IN (SELECT m.seq FROM messages m LEFT JOIN p ON p.tenant = m.tenant
			WHERE m.state NOT IN ('staged', 'queued', 'leased', 'current') AND m.updated_at <= $9 AND m.updated_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF m SKIP LOCKED)`, base),
		exec(`DELETE FROM message_marks WHERE (tenant, key) IN (SELECT tenant, key FROM message_marks
			WHERE until <= $10 LIMIT $8 FOR UPDATE SKIP LOCKED)`, with(start)),
		// A snapshot of the status views goes with the content of its task (REP-2).
		exec(`DELETE FROM view_snapshots WHERE task IN (SELECT v.task FROM view_snapshots v JOIN tasks t ON t.id = v.task LEFT JOIN p ON p.tenant = t.tenant
			WHERE v.created_at <= $9 AND v.created_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF v SKIP LOCKED)`, base),
		// A finished script run past the content period keeps a tombstone (RET-10).
		func(tx pgx.Tx, now time.Time) (int64, error) {
			return purgeRuns(ctx, tx, retentionCuts, `SELECT r.id FROM script_runs r LEFT JOIN p ON p.tenant = r.tenant
				WHERE r.state IN ('completed', 'failed', 'canceled', 'rejected') AND NOT r.record ? 'purged' AND r.updated_at <= $9
				AND r.updated_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF r SKIP LOCKED`, base...)
		},
		// The arguments and the result fields of an item of a finished campaign go with the
		// content of its tasks; the item keeps its state, links and the hash of its arguments.
		exec(`UPDATE campaign_items SET args = '{}', summary = NULL WHERE (campaign, n) IN (SELECT i.campaign, i.n FROM campaign_items i
			JOIN campaigns k ON k.id = i.campaign LEFT JOIN p ON p.tenant = k.tenant
			WHERE (i.args <> '{}' OR i.summary IS NOT NULL) AND k.state IN ('completed', 'failed', 'canceled', 'rejected')
			AND k.updated_at <= $9 AND k.updated_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF i SKIP LOCKED)`, base),
		// The note of a finding and the author note of a closed proposal are content
		// (MGR-30, PRP-1); the rest of each record is metadata.
		exec(`UPDATE findings SET note = NULL WHERE id IN (SELECT f.id FROM findings f LEFT JOIN p ON p.tenant = f.tenant
			WHERE f.note IS NOT NULL AND f.created_at <= $9 AND f.created_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF f SKIP LOCKED)`, base),
		exec(`UPDATE proposals SET record = record #- '{body,note}' WHERE id IN (SELECT x.id FROM proposals x LEFT JOIN p ON p.tenant = x.tenant
			WHERE x.record->'body' ? 'note' AND x.state NOT IN ('screened', 'in_review', 'recommended', 'parked', 'presented', 'realizing')
			AND x.updated_at <= $9 AND x.updated_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF x SKIP LOCKED)`, base),
		// A sent digest is content once its slot is past (DIG-1): its row only kept the slot.
		exec(`DELETE FROM proposal_digests WHERE id IN (SELECT d.id FROM proposal_digests d LEFT JOIN p ON p.tenant = d.tenant
			WHERE d.created_at <= $9 AND d.created_at <= coalesce(p.content, $6) LIMIT $8 FOR UPDATE OF d SKIP LOCKED)`, base),
		// An expired file has no bytes left; the next step records expired -> purged and the
		// record stays as the tombstone (spec/lifecycles/file.md, RET-2).
		exec(`UPDATE files SET state = 'purged', content = NULL WHERE file_ref IN (SELECT file_ref FROM files
			WHERE state = 'expired' LIMIT $8 FOR UPDATE SKIP LOCKED)`, base),
		// C4 by the tenant's memory period: a person's records (RET-9).
		exec(`DELETE FROM memory_items WHERE id IN (SELECT m.id FROM memory_items m JOIN p ON p.tenant = m.tenant
			WHERE m.updated_at <= p.memory LIMIT $8 FOR UPDATE OF m SKIP LOCKED)`, base),
		// Facts by the memory period from their last verification (a memory record ages from its
		// last change), a sealed fact of a retired identity too, before its seal
		// ends (MEM-10); an open offer no longer lists a deleted fact, so the successor's recheck
		// never reads it (MEM-7). The end of the seal still erases what is left (MEM-9).
		func(tx pgx.Tx, now time.Time) (int64, error) {
			var n int64
			err := tx.QueryRow(ctx, retentionCuts+`, d AS (DELETE FROM facts WHERE id IN (SELECT f.id FROM facts f JOIN p ON p.tenant = f.tenant
				WHERE greatest(f.created_at, f.verified_at) <= p.memory LIMIT $8 FOR UPDATE OF f SKIP LOCKED) RETURNING id),
				o AS (UPDATE fact_offers SET facts = array(SELECT x FROM unnest(facts) x WHERE x <> ALL (SELECT id FROM d))
				WHERE state = 'open' AND facts && array(SELECT id FROM d))
				SELECT count(*) FROM d`, base...).Scan(&n)
			return n, err
		},
		// A person's usage line past 93 days keeps its amounts and loses every reference that
		// leads back to the person (BUD-16, SEC-14): subject, task and root; the ids of the tool
		// call, campaign, script, provider request, attempt, worker and bot identity (a worker
		// and a time find the lease and its task); the exact message size; and its own id,
		// whose ULID carries the millisecond of the line, which gets a new id without time.
		// Its times go to the start of their UTC day and it records subject_kind human. Whole
		// days: a line goes once its day is older than 93 days. Nothing refers to a spend id
		// once the line is no longer reserved.
		func(tx pgx.Tx, now time.Time) (int64, error) {
			rows, err := tx.Query(ctx, `SELECT id FROM spend WHERE subject LIKE 'human:%' AND state <> 'reserved' AND time < $1 LIMIT $2 FOR UPDATE SKIP LOCKED`,
				periodStart("day", start).Add(-personUsageDays*day), retentionBatch)
			if err != nil {
				return 0, err
			}
			old, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil || len(old) == 0 {
				return 0, err
			}
			fresh := make([]string, len(old))
			for i := range old {
				fresh[i] = ids.Untimed(ids.Spend)
			}
			tag, err := tx.Exec(ctx, `UPDATE spend s SET id = n.fresh, subject = NULL, task = NULL, root = NULL, toolcall = NULL, campaign = NULL, script = NULL,
				request_id = NULL, fingerprint = NULL, attempt_lease_no = NULL, worker = NULL, acting_bot = NULL, bot_epoch = NULL, subject_kind = 'human',
				message = CASE WHEN s.message ? 'kind' THEN jsonb_build_object('kind', s.message->'kind') END,
				time = date_trunc('day', s.time, 'UTC'), settled_at = date_trunc('day', s.settled_at, 'UTC'),
				reconciled_at = date_trunc('day', s.reconciled_at, 'UTC'), released_at = date_trunc('day', s.released_at, 'UTC')
				FROM unnest($1::text[], $2::text[]) n (old, fresh) WHERE s.id = n.old`, old, fresh)
			return tag.RowsAffected(), err
		},
	}
	for _, step := range steps {
		if err := c.batches(ctx, step); err != nil {
			return err
		}
	}
	return c.metadataRetention(ctx, cat)
}

// batches runs one deletion step in transactions of at most retentionBatch rows each.
func (c *Core) batches(ctx context.Context, step func(tx pgx.Tx, now time.Time) (int64, error)) error {
	for range retentionMaxBatchesTick {
		var n int64
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
			var err error
			n, err = step(tx, now)
			return err
		}); err != nil {
			return err
		}
		if n < retentionBatch {
			break
		}
	}
	return nil
}

// metadataRetention deletes metadata in whole calendar months past the tenant's period, never
// one row (RET-10, AUD-3): usage lines and reconciliation days and events by each expired month,
// and the audit chains as the prefix of expired months behind a cut that keeps the chain
// verifiable, so a month of audit waits for the older months before it. A tenant gone from the
// catalog keeps the period it last had.
func (c *Core) metadataRetention(ctx context.Context, cat *Catalog) error {
	type tenantSpans struct {
		name     string
		from, to []time.Time
		prefix   time.Time // the end of the expired prefix, or zero
	}
	var list []tenantSpans
	if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		list = list[:0]
		rows, err := tx.Query(ctx, `SELECT tenant, seconds, since FROM retention_metadata_periods ORDER BY tenant, since`)
		if err != nil {
			return err
		}
		hist := map[string][]metaPeriod{}
		for rows.Next() {
			var t string
			var s int64
			var since time.Time
			if err := rows.Scan(&t, &s, &since); err != nil {
				rows.Close()
				return err
			}
			hist[t] = append(hist[t], metaPeriod{since, time.Duration(s) * time.Second})
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		rows, err = tx.Query(ctx, `SELECT name FROM tenants UNION SELECT tenant FROM audit_heads ORDER BY 1`)
		if err != nil {
			return err
		}
		names, err := pgx.CollectRows(rows, pgx.RowTo[string])
		if err != nil {
			return err
		}
		for _, n := range names {
			cur := periodsOf(cat.Tenants[n]).metadata
			if h := hist[n]; cat.Tenants[n] == nil && len(h) > 0 {
				cur = h[len(h)-1].d
			}
			spans := expiredMonths(now, hist[n], cur)
			if len(spans) == 0 {
				continue
			}
			ts := tenantSpans{name: n}
			for _, sp := range spans {
				ts.from, ts.to = append(ts.from, sp.from), append(ts.to, sp.to)
			}
			if spans[0].from.IsZero() {
				ts.prefix = spans[0].to
			}
			list = append(list, ts)
		}
		return nil
	}); err != nil {
		return err
	}
	const in = ` AND EXISTS (SELECT 1 FROM unnest($2::timestamptz[], $3::timestamptz[]) r (f, t) WHERE %s >= r.f AND %s < r.t)`
	for _, ts := range list {
		steps := []func(tx pgx.Tx, now time.Time) (int64, error){
			func(tx pgx.Tx, now time.Time) (int64, error) {
				tag, err := tx.Exec(ctx, `DELETE FROM spend WHERE id IN (SELECT id FROM spend WHERE tenant = $1 AND state <> 'reserved'`+fmt.Sprintf(in, "time", "time")+`
					LIMIT $4 FOR UPDATE SKIP LOCKED)`, ts.name, ts.from, ts.to, retentionBatch)
				return tag.RowsAffected(), err
			},
			// A reservation left behind is terminal once nothing live holds it: no live task,
			// tool call, campaign or script run.
			func(tx pgx.Tx, now time.Time) (int64, error) {
				tag, err := tx.Exec(ctx, `DELETE FROM spend WHERE id IN (SELECT s.id FROM spend s WHERE s.tenant = $1 AND s.state = 'reserved'`+fmt.Sprintf(in, "s.time", "s.time")+`
					AND NOT EXISTS (SELECT 1 FROM tasks t WHERE t.id = s.task AND t.state IN ('submitted', 'working'))
					AND NOT EXISTS (SELECT 1 FROM toolcalls c WHERE c.id = s.toolcall AND c.state IN ('requested', 'queued', 'leased'))
					AND NOT EXISTS (SELECT 1 FROM campaigns k WHERE k.id = s.campaign AND k.state = ANY($5))
					AND NOT EXISTS (SELECT 1 FROM script_runs r WHERE r.id = s.script AND r.state = ANY($6))
					LIMIT $4 FOR UPDATE OF s SKIP LOCKED)`, ts.name, ts.from, ts.to, retentionBatch, campaignLive, scriptLive)
				return tag.RowsAffected(), err
			},
			func(tx pgx.Tx, now time.Time) (int64, error) {
				_, err := tx.Exec(ctx, `DELETE FROM spend_reconciliations WHERE tenant = $1`+fmt.Sprintf(in, "day", "day"), ts.name, ts.from, ts.to)
				return 0, err
			},
			func(tx pgx.Tx, now time.Time) (int64, error) {
				tag, err := tx.Exec(ctx, `DELETE FROM events WHERE seq IN (SELECT seq FROM events WHERE tenant = $1`+fmt.Sprintf(in, "time", "time")+`
					LIMIT $4 FOR UPDATE SKIP LOCKED)`, ts.name, ts.from, ts.to, retentionBatch)
				return tag.RowsAffected(), err
			},
			func(tx pgx.Tx, now time.Time) (int64, error) {
				if ts.prefix.IsZero() {
					return 0, nil
				}
				return c.cutAudit(ctx, tx, now, ts.name, ts.prefix)
			},
		}
		// The other metadata records go by the month in which they ended, terminal ones only
		// and none a live object still reads (RET-10). Each is one statement: the dependents
		// (card views, alarm values, notices, items, match steps) go with their record.
		meta := func(sql string, extra ...any) func(tx pgx.Tx, now time.Time) (int64, error) {
			return func(tx pgx.Tx, now time.Time) (int64, error) {
				var n int64
				err := tx.QueryRow(ctx, sql, append([]any{ts.name, ts.from, ts.to, retentionBatch}, extra...)...).Scan(&n)
				return n, err
			}
		}
		steps = append(steps,
			meta(`WITH d AS (DELETE FROM approvals WHERE id IN (SELECT a.id FROM approvals a WHERE a.tenant = $1
				AND a.state IN ('executed', 'exec_failed', 'rejected', 'expired', 'canceled')`+fmt.Sprintf(in, "a.updated_at", "a.updated_at")+`
				AND NOT EXISTS (SELECT 1 FROM script_steps st JOIN script_runs r ON r.id = st.run WHERE st.approval = a.id AND r.state = ANY($5))
				LIMIT $4 FOR UPDATE OF a SKIP LOCKED) RETURNING id),
				v AS (DELETE FROM card_views WHERE subject_kind = 'approval' AND subject_id IN (SELECT id FROM d))
				SELECT count(*) FROM d`, scriptLive),
			meta(`WITH d AS (DELETE FROM holds WHERE id IN (SELECT h.id FROM holds h WHERE h.tenant = $1 AND h.state IN ('expired', 'released')`+
				fmt.Sprintf(in, "coalesce(h.ended_at, h.until, h.created_at)", "coalesce(h.ended_at, h.until, h.created_at)")+`
				LIMIT $4 FOR UPDATE OF h SKIP LOCKED) RETURNING id) SELECT count(*) FROM d`),
			meta(`WITH d AS (DELETE FROM incidents WHERE id IN (SELECT i.id FROM incidents i WHERE i.tenant = $1 AND i.state IN ('closed', 'dismissed')`+
				fmt.Sprintf(in, "coalesce(i.ended_at, i.updated_at)", "coalesce(i.ended_at, i.updated_at)")+`
				AND NOT EXISTS (SELECT 1 FROM control_cards k WHERE k.incident = i.id AND k.state = 'pending')
				LIMIT $4 FOR UPDATE OF i SKIP LOCKED) RETURNING id),
				n AS (DELETE FROM manager_notices WHERE basis_kind = 'incident' AND basis IN (SELECT id FROM d))
				SELECT count(*) FROM d`),
			meta(`WITH d AS (DELETE FROM alarm_episodes WHERE id IN (SELECT e.id FROM alarm_episodes e WHERE e.tenant = $1 AND e.state = 'resolved'`+
				fmt.Sprintf(in, "e.resolved_at", "e.resolved_at")+`
				AND NOT EXISTS (SELECT 1 FROM incidents i WHERE e.id = ANY (i.episodes) AND i.state NOT IN ('closed', 'dismissed'))
				LIMIT $4 FOR UPDATE OF e SKIP LOCKED) RETURNING id),
				v AS (DELETE FROM alarm_values WHERE episode IN (SELECT id FROM d))
				SELECT count(*) FROM d`),
			meta(`WITH d AS (DELETE FROM control_ops WHERE id IN (SELECT o.id FROM control_ops o WHERE o.tenant = $1`+fmt.Sprintf(in, "o.created_at", "o.created_at")+`
				LIMIT $4 FOR UPDATE OF o SKIP LOCKED) RETURNING id) SELECT count(*) FROM d`),
			meta(`WITH d AS (DELETE FROM control_cards WHERE id IN (SELECT k.id FROM control_cards k WHERE k.tenant = $1 AND k.state <> 'pending'`+
				fmt.Sprintf(in, "k.updated_at", "k.updated_at")+` LIMIT $4 FOR UPDATE OF k SKIP LOCKED) RETURNING id) SELECT count(*) FROM d`),
			meta(`WITH c AS (SELECT k.id FROM campaigns k WHERE k.tenant = $1 AND k.state IN ('completed', 'failed', 'canceled', 'rejected')`+
				fmt.Sprintf(in, "k.updated_at", "k.updated_at")+`
				AND NOT EXISTS (SELECT 1 FROM script_steps st JOIN script_runs r ON r.id = st.run WHERE st.campaign = k.id AND r.state = ANY($5))
				AND NOT EXISTS (SELECT 1 FROM spend s WHERE s.campaign = k.id AND s.state = 'reserved')
				LIMIT $4 FOR UPDATE OF k SKIP LOCKED),
				i AS (DELETE FROM campaign_items WHERE campaign IN (SELECT id FROM c)),
				d AS (DELETE FROM campaigns WHERE id IN (SELECT id FROM c) RETURNING id)
				SELECT count(*) FROM d`, scriptLive),
			// A person's request goes once it was refused or withdrawn, or once its erasure is
			// done with the ids it found recorded on the erasure itself (the replay reads those).
			meta(`WITH c AS (SELECT q.id FROM subject_requests q LEFT JOIN erasure_requests e ON e.id = q.erasure WHERE q.tenant = $1
				AND (q.state IN ('rejected', 'withdrawn') OR q.state = 'approved' AND e.state = 'done' AND (q.kind = 'erase' OR e.found IS NOT NULL))`+
				fmt.Sprintf(in, "q.updated_at", "q.updated_at")+` LIMIT $4 FOR UPDATE OF q SKIP LOCKED),
				m AS (DELETE FROM subject_match_steps WHERE request IN (SELECT id FROM c)),
				d AS (DELETE FROM subject_requests WHERE id IN (SELECT id FROM c) RETURNING id)
				SELECT count(*) FROM d`),
		)
		for _, step := range steps {
			if err := c.batches(ctx, step); err != nil {
				return err
			}
		}
	}
	return nil
}

// cutAudit deletes, in each chain of the tenant, the prefix of records written before the cut
// month: it first moves the chain's cut to the last record deleted (its seq and hash), which is
// what lets the store trigger pass the delete; verification then starts from the cut (AUD-3,
// AUD-6). A record after the first one of a kept month stays even if its time is older, so the
// deleted part is always a prefix. Each cut leaves one period_deleted record in the tenant's
// chain with the bounds, the count and the last hash of what went.
func (c *Core) cutAudit(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, before time.Time) (int64, error) {
	rows, err := tx.Query(ctx, `SELECT epoch FROM audit_heads WHERE tenant = $1 ORDER BY epoch`, tenant)
	if err != nil {
		return 0, err
	}
	epochs, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return 0, err
	}
	var total int64
	for _, epoch := range epochs {
		var cut int64
		err := tx.QueryRow(ctx, `SELECT seq FROM audit_cuts WHERE tenant = $1 AND epoch = $2 FOR UPDATE`, tenant, epoch).Scan(&cut)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return 0, err
		}
		var upto int64
		if err := tx.QueryRow(ctx, `SELECT coalesce((SELECT min(seq) - 1 FROM audit WHERE tenant = $1 AND epoch = $2 AND at >= $3),
			(SELECT max(seq) FROM audit WHERE tenant = $1 AND epoch = $2), 0)`, tenant, epoch, before).Scan(&upto); err != nil {
			return 0, err
		}
		upto = min(upto, cut+int64(retentionBatch)-total)
		if upto <= cut {
			continue
		}
		var hash string
		if err := tx.QueryRow(ctx, `SELECT hash FROM audit WHERE tenant = $1 AND epoch = $2 AND seq = $3`, tenant, epoch, upto).Scan(&hash); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO audit_cuts (tenant, epoch, seq, hash, updated_at) VALUES ($1, $2, $3, $4, $5)
			ON CONFLICT (tenant, epoch) DO UPDATE SET seq = EXCLUDED.seq, hash = EXCLUDED.hash, updated_at = EXCLUDED.updated_at`, tenant, epoch, upto, hash, now); err != nil {
			return 0, err
		}
		var from, to time.Time
		if err := tx.QueryRow(ctx, `SELECT min(at), max(at) FROM audit WHERE tenant = $1 AND epoch = $2 AND seq > $3 AND seq <= $4`, tenant, epoch, cut, upto).Scan(&from, &to); err != nil {
			return 0, err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM audit WHERE tenant = $1 AND epoch = $2 AND seq > $3 AND seq <= $4`, tenant, epoch, cut, upto)
		if err != nil {
			return 0, err
		}
		total += tag.RowsAffected()
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tenant, Kind: "period_deleted", Actor: Actor{Principal: "system", Via: "core"}, OK: true,
			Period: map[string]any{"from": fmtTime(from), "to": fmtTime(to), "count": tag.RowsAffected(), "last_hash": hash},
		}); err != nil {
			return 0, err
		}
		if total >= retentionBatch {
			break
		}
	}
	return total, nil
}
