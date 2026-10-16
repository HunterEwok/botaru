// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

const (
	proposalRejectedType = "proposal.rejected.v1"
	// The limits of a role and a tenant when the manifests set none (INS-4, PRP-5).
	defaultMaxOpenProposals = 3
	defaultMaxPerWeek       = 3
	defaultMaxInReview      = 10
	defaultRedecideAfter    = 30 * 24 * time.Hour
	// redecideShare is how much an evidence value must move before a declined fingerprint is
	// accepted again inside redecide_after (PRP-5).
	redecideShare = 0.2
	// evidenceTolerance is the relative tolerance of a cited value for a series without an
	// outcome block; a series with one uses its own tolerance (PRP-2).
	evidenceTolerance = 0.05
	maxEvidence       = 10
)

// openProposalStates count against the open limits of a role and a tenant (INS-4, PRP-5).
var openProposalStates = []string{"screened", "in_review", "recommended", "presented", "parked"}

// proposalAuthor is the attempt that handed in an analysis or aggregator result (PRP-14).
type proposalAuthor struct {
	Tenant, Role, Worker, Task, Type, Mark string
	Level                                  int
	// Acting is the identity of the attempt that wrote the proposals (IDN-8).
	Acting    actingOf
	Untrusted bool
	Sources   []string
	Readable  []string
}

// rejection is the refusal of one proposal body: no record, an event with the reason (PRP-1).
type rejection string

func (r rejection) Error() string { return "proposal rejected: " + string(r) }

// insightBodies checks the envelope of an analysis or aggregator result, insight-result: at
// most three proposal bodies or the code nothing_found. A body is checked one by one later,
// so a bad body is a refusal of that proposal, not of the whole result (INS-1, PRP-1).
func insightBodies(result map[string]any) ([]map[string]any, error) {
	bad := refuse("output_schema_invalid", "the result of an analysis is proposals or the code nothing_found")
	if len(result) != 1 {
		return nil, bad
	}
	if code, ok := result["code"]; ok {
		if code != "nothing_found" {
			return nil, bad
		}
		return nil, nil
	}
	list, ok := result["proposals"].([]any)
	if !ok || len(list) < 1 || len(list) > 3 {
		return nil, bad
	}
	out := make([]map[string]any, 0, len(list))
	for _, x := range list {
		m, ok := x.(map[string]any)
		if !ok {
			return nil, bad
		}
		out = append(out, m)
	}
	return out, nil
}

// createProposals turns the bodies of a completed analysis or aggregator attempt into proposal
// records, in the transaction of the complete (INS-3). A body that fails a check gives
// proposal.rejected.v1 and no record.
func (c *Core) createProposals(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker, task, typ string, rec []byte, bodies []map[string]any) error {
	if len(bodies) == 0 {
		return nil
	}
	var r struct {
		DataMark        string `json:"data_mark"`
		UntrustedSeen   bool   `json:"untrusted_seen"`
		UntrustedSource string `json:"untrusted_source"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	a := proposalAuthor{Tenant: w.Tenant, Role: w.Role, Worker: w.ID, Task: task, Type: typ, Mark: r.DataMark,
		Untrusted: r.UntrustedSeen, Readable: c.readableSeries(cat, w.Tenant, w.Role, typ)}
	if r.UntrustedSeen {
		a.Sources = []string{r.UntrustedSource}
	}
	acting, err := leaseActing(ctx, tx, task, w.ID)
	if err != nil {
		return err
	}
	a.Acting = acting
	// An aggregator's proposal takes the mark and the untrusted flag of its task, which carry
	// the highest mark and the logical OR of the inputs it read (AGG-4).
	if cat.TaskTypes[typ].Purpose == "aggregate" {
		a.Level = 1
	}
	for _, body := range bodies {
		fp, err := c.createProposal(ctx, tx, now, cat, a, body)
		var rj rejection
		if errors.As(err, &rj) {
			if err := c.proposalRejected(ctx, tx, now, a, body, fp, string(rj)); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) proposalRejected(ctx context.Context, tx pgx.Tx, now time.Time, a proposalAuthor, body map[string]any, fp, reason string) error {
	var actingBot any
	if a.Acting.Bot != "" {
		actingBot = a.Acting.Bot
	}
	data := map[string]any{"reason": reason, "task": a.Task, "role": a.Role, "worker": a.Worker, "acting_bot": actingBot, "category": nil, "problem_code": nil, "fingerprint": nil}
	if cat, ok := body["category"].(string); ok && validCategory(cat) {
		data["category"] = cat
	}
	if p, ok := body["problem"].(map[string]any); ok {
		if code, ok := p["code"].(string); ok && problemCodeOK(code) {
			data["problem_code"] = code
		}
	}
	if fp != "" {
		data["fingerprint"] = fp
	}
	return c.appendEvent(ctx, tx, now, a.Tenant, Event{Type: proposalRejectedType, Subject: a.Task, Data: data})
}

func validCategory(s string) bool {
	return slices.Contains([]string{"schedule", "backup", "capacity", "cost", "reliability", "security", "data_quality", "config"}, s)
}

func problemCodeOK(s string) bool {
	if s == "" || len(s) > 64 || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '_') {
			return false
		}
	}
	return true
}

// createProposal checks one body and writes its record, or merges it into an open duplicate.
// It returns the fingerprint once it is known, for the refusal event.
func (c *Core) createProposal(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a proposalAuthor, body map[string]any) (string, error) {
	// The closed schema; the note is never read by code (PRP-1).
	if err := c.spec.Validate("proposal-body", body); err != nil {
		return "", rejection("schema_invalid")
	}
	var b proposalBody
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return "", rejection("schema_invalid")
	}
	// Every subject is an object of the author's tenant (PRP-8, TEN-7).
	subjects := slices.Clone(b.Problem.Subjects)
	if b.Benefit != nil {
		subjects = append(subjects, b.Benefit.Subject)
	}
	for _, s := range subjects {
		kind, _, _ := strings.Cut(s, ":")
		if !c.subjectOfTenant(cat, a.Tenant, kind, s) {
			return "", rejection("forbidden_change")
		}
	}
	kind := b.Change.Kind
	// Permissions and security only for information (PRP-3).
	if b.Category == "security" && kind != "none" {
		return "", rejection("forbidden_change")
	}
	// After untrusted text only none or a manifest change in the safe direction (PRP-7).
	if a.Untrusted && kind != "none" && kind != "manifest_change" {
		return "", rejection("untrusted_direction")
	}
	chk, err := c.checkChange(ctx, tx, cat, a, b.Change)
	if err != nil {
		return "", err
	}
	if err := c.cronHistory(ctx, tx, now, cat, a.Tenant, b.Change); err != nil {
		return "", err
	}
	// The trial metric is a series with a complete outcome block (PRP-21, OBV-6).
	series := c.tenantSeries(cat, a.Tenant)
	if b.Benefit != nil {
		if s, ok := series[b.Benefit.Metric]; !ok || s.Outcome == nil || !s.Outcome.Complete {
			return "", rejection("metric_unknown")
		}
	}
	// The core rereads the cited values (PRP-2).
	seen := make([]map[string]any, 0, len(b.Evidence))
	for _, e := range b.Evidence {
		v, ok, err := c.evidenceValue(ctx, tx, now, cat, a, e)
		if err != nil {
			return "", err
		}
		if !ok || !withinTolerance(series[e.Series], e.Value, v) {
			return "", rejection("evidence_mismatch")
		}
		seen = append(seen, map[string]any{"ref": e.ref(), "value": v})
	}
	fp, err := jcs.Hash(map[string]any{"category": b.Category, "problem": b.Problem.Code, "subjects": sortedSet(b.Problem.Subjects),
		"kind": kind, "targets": chk.Targets})
	if err != nil {
		return "", err
	}
	// Two submissions of one fingerprint take turns, so the second sees the first as an open
	// duplicate instead of inserting a second open proposal (PRP-5).
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 7300104))`, a.Tenant+"/"+fp); err != nil {
		return fp, err
	}
	// A fingerprint people declined is not taken again inside redecide_after unless the
	// evidence moved by more than the share (PRP-5).
	tv := cat.Tenants[a.Tenant]
	redecide := durationOr(tv.Spec.Insights.RedecideAfter, defaultRedecideAfter)
	rows, err := tx.Query(ctx, `SELECT record->'evidence_seen' FROM proposals WHERE tenant = $1 AND fingerprint = $2 AND state = 'declined' AND state_at > $3`,
		a.Tenant, fp, now.Add(-redecide))
	if err != nil {
		return fp, err
	}
	declined, err := pgx.CollectRows(rows, pgx.RowTo[[]byte])
	if err != nil {
		return fp, err
	}
	for _, d := range declined {
		var old []map[string]any
		_ = json.Unmarshal(d, &old)
		if !evidenceMoved(old, seen) {
			return fp, rejection("recently_declined")
		}
	}
	// An open duplicate counts the arrival and takes the new evidence (PRP-5).
	var dupID string
	var dupRec []byte
	err = tx.QueryRow(ctx, `SELECT id, record FROM proposals WHERE tenant = $1 AND fingerprint = $2 AND state = ANY($3) ORDER BY created_at LIMIT 1 FOR UPDATE`,
		a.Tenant, fp, openProposalStates).Scan(&dupID, &dupRec)
	if err == nil {
		return fp, c.mergeDuplicate(ctx, tx, now, a, dupID, dupRec, body, seen)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fp, err
	}
	// The flow limits of the role and the tenant (INS-4, PRP-5).
	if limited, err := proposalLimited(ctx, tx, now, cat, a); err != nil {
		return fp, err
	} else if limited {
		return fp, rejection("proposal_rate_limited")
	}
	influence, self, err := selfInfluence(ctx, tx, now, a, b.Evidence, series)
	if err != nil {
		return fp, err
	}
	id := ids.New(ids.Proposal)
	sources := a.Sources
	if sources == nil {
		sources = []string{}
	}
	var actingBot, botEpoch any
	if a.Acting.Bot != "" {
		actingBot, botEpoch = a.Acting.Bot, a.Acting.BotEpoch
	}
	record := map[string]any{
		"id": id, "tenant": a.Tenant,
		"origin":      map[string]any{"role": a.Role, "worker": a.Worker, "acting_bot": actingBot, "bot_epoch": botEpoch, "task": a.Task, "level": a.Level},
		"fingerprint": fp, "data_mark": a.Mark,
		"untrusted_seen": map[string]any{"value": a.Untrusted, "sources": sources},
		"self_influence": influence, "self_evidence": self, "conflicts_with": []any{}, "seen_count": 1,
		"state": "screened", "state_reason": nil, "body": body, "reviews": []any{}, "verdict": nil, "decision": nil,
		"realization": nil, "trial": nil,
		"evidence_seen": seen, "guarded": chk.Guarded, "targets": chk.Targets, "created_at": fmtTime(now),
	}
	recRaw, err := json.Marshal(record)
	if err != nil {
		return fp, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO proposals (id, tenant, role, worker, task, fingerprint, kind, state, record, created_at, updated_at, state_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'screened', $8, $9, $9, $9)`, id, a.Tenant, a.Role, a.Worker, a.Task, fp, kind, recRaw, now)
	return fp, err
}

// proposalBody is the part of botaru.proposal/v1 the core reads; the note is never read.
type proposalBody struct {
	Category string `json:"category"`
	Problem  struct {
		Code     string   `json:"code"`
		Subjects []string `json:"subjects"`
		Severity string   `json:"severity"`
	} `json:"problem"`
	Evidence []evidenceRef  `json:"evidence"`
	Change   proposalChange `json:"change"`
	Benefit  *struct {
		Metric            string   `json:"metric"`
		Subject           string   `json:"subject"`
		Direction         string   `json:"direction"`
		ExpectedChangePP  *float64 `json:"expected_change_pp"`
		ExpectedChangePct *float64 `json:"expected_change_pct"`
	} `json:"benefit"`
}

type evidenceRef struct {
	Kind    string `json:"kind"`
	Series  string `json:"series"`
	Subject string `json:"subject"`
	Window  string `json:"window"`
	Rollup  string `json:"rollup"`
	Slot    string `json:"slot"`
	Value   any    `json:"value"`
}

func (e evidenceRef) ref() map[string]any {
	m := map[string]any{"series": e.Series, "subject": e.Subject, "window": e.Window, "rollup": e.Rollup}
	if e.Slot != "" {
		m["slot"] = e.Slot
	}
	return m
}

type proposalChange struct {
	Kind          string `json:"kind"`
	ActionRequest *struct {
		Action string         `json:"action"`
		Tool   string         `json:"tool"`
		Args   map[string]any `json:"args"`
	} `json:"action_request"`
	ManifestChange *struct {
		Ops []manifestOp `json:"ops"`
	} `json:"manifest_change"`
	HumanChange *struct {
		Target struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		} `json:"target"`
		Path      string `json:"path"`
		Direction string `json:"direction"`
		Value     any    `json:"value"`
	} `json:"human_change"`
}

type manifestOp struct {
	Op     string `json:"op"`
	Target struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"target"`
	Field string `json:"field"`
	Value any    `json:"value"`
}

func sortedSet(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return slices.Compact(out)
}

// evidenceValue rereads a cited value with the author's own read: its series, its mark and its
// tenant (PRP-2, OBV-3). A window rollup has one value; an hour or day rollup gives the period
// of the slot, or the latest period.
func (c *Core) evidenceValue(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a proposalAuthor, e evidenceRef) (any, bool, error) {
	if a.Mark == "" {
		return nil, false, nil
	}
	out, err := c.queryObservations(ctx, tx, now, cat, a.Tenant, a.Readable, a.Mark, obsQuery{Series: e.Series, Subject: e.Subject, Window: e.Window, Rollup: e.Rollup})
	if _, ok := AsProblem(err); ok {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	groups, _ := out["groups"].([]any)
	if len(groups) != 1 {
		return nil, false, nil
	}
	points, _ := groups[0].(map[string]any)["points"].([]any)
	if e.Slot != "" {
		if !strings.HasPrefix(e.Rollup, "hour.") {
			return nil, false, nil
		}
		loc := tenantLocation(cat, a.Tenant)
		for i := len(points) - 1; i >= 0; i-- {
			p := points[i].(map[string]any)
			at, err := time.Parse(time.RFC3339, asString(p["at"]))
			if err == nil && at.In(loc).Format("15:04") == e.Slot {
				return p["value"], true, nil
			}
		}
		return nil, false, nil
	}
	if len(points) == 0 {
		return nil, false, nil
	}
	return points[len(points)-1].(map[string]any)["value"], true, nil
}

func tenantLocation(cat *Catalog, tenant string) *time.Location {
	if tv := cat.Tenants[tenant]; tv != nil && tv.Spec.Timezone != "" {
		if loc, err := time.LoadLocation(tv.Spec.Timezone); err == nil {
			return loc
		}
	}
	return time.UTC
}

// withinTolerance compares a cited value with the reread one: a value of a closed list exactly,
// a number within the series tolerance (percentage points for a share, percent otherwise).
func withinTolerance(s seriesDef, cited, got any) bool {
	cs, cok := cited.(string)
	gs, gok := got.(string)
	if cok || gok {
		return cok && gok && cs == gs
	}
	cn, ok1 := toFloat(cited)
	gn, ok2 := toFloat(got)
	if !ok1 || !ok2 {
		return false
	}
	switch {
	case s.Outcome != nil && s.Outcome.TolerancePP > 0 && len(s.Enum) > 0:
		return math.Abs(cn-gn) <= s.Outcome.TolerancePP/100
	case s.Outcome != nil && s.Outcome.TolerancePct > 0:
		return math.Abs(cn-gn) <= s.Outcome.TolerancePct/100*math.Abs(gn)
	}
	return math.Abs(cn-gn) <= evidenceTolerance*math.Abs(gn)
}

// evidenceMoved says whether any value of the same reference moved by more than the share
// since a declined proposal (PRP-5).
func evidenceMoved(old, now []map[string]any) bool {
	for _, n := range now {
		ref, _ := jcs.Hash(n["ref"])
		for _, o := range old {
			if h, _ := jcs.Hash(o["ref"]); h != ref {
				continue
			}
			a, ok1 := toFloat(o["value"])
			b, ok2 := toFloat(n["value"])
			if !ok1 || !ok2 {
				if fmt.Sprint(o["value"]) != fmt.Sprint(n["value"]) {
					return true
				}
				continue
			}
			if math.Abs(b-a) > redecideShare*math.Max(math.Abs(a), 1e-9) {
				return true
			}
		}
	}
	return false
}

// mergeDuplicate counts one more arrival of an open proposal and adds the new evidence
// references it does not have, up to ten; the author does not change (PRP-5, PRP-14). The
// record takes the higher data mark and the untrusted flag and sources of the arrival, so the
// added values are never read below their class (INJ-4); an untrusted arrival does not join a
// trusted record that changes something, because that change was checked without the
// untrusted rule (PRP-12).
func (c *Core) mergeDuplicate(ctx context.Context, tx pgx.Tx, now time.Time, a proposalAuthor, id string, raw []byte, body map[string]any, seen []map[string]any) error {
	var rec map[string]any
	if err := json.Unmarshal(raw, &rec); err != nil {
		return err
	}
	us, _ := rec["untrusted_seen"].(map[string]any)
	if us == nil {
		us = map[string]any{"value": false, "sources": []any{}}
		rec["untrusted_seen"] = us
	}
	if a.Untrusted {
		recBody, _ := rec["body"].(map[string]any)
		ch, _ := recBody["change"].(map[string]any)
		if us["value"] != true && asString(ch["kind"]) != "none" {
			return rejection("untrusted_direction")
		}
		us["value"] = true
		srcs, _ := us["sources"].([]any)
		for _, src := range a.Sources {
			if !slices.Contains(srcs, any(src)) {
				srcs = append(srcs, src)
			}
		}
		if srcs == nil {
			srcs = []any{}
		}
		us["sources"] = srcs
	}
	if newO, ok := c.markOrder(a.Mark); ok {
		if oldO, ok := c.markOrder(asString(rec["data_mark"])); !ok || newO > oldO {
			rec["data_mark"] = a.Mark
		}
	}
	n, _ := toFloat(rec["seen_count"])
	rec["seen_count"] = int(n) + 1
	recBody, _ := rec["body"].(map[string]any)
	have, _ := recBody["evidence"].([]any)
	haveSeen, _ := rec["evidence_seen"].([]any)
	known := map[string]bool{}
	for _, s := range haveSeen {
		h, _ := jcs.Hash(s.(map[string]any)["ref"])
		known[h] = true
	}
	newEv, _ := body["evidence"].([]any)
	for i, s := range seen {
		h, _ := jcs.Hash(s["ref"])
		if known[h] || len(have) >= maxEvidence {
			continue
		}
		known[h] = true
		have = append(have, newEv[i])
		haveSeen = append(haveSeen, s)
	}
	recBody["evidence"], rec["evidence_seen"] = have, haveSeen
	out, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE proposals SET record = $2, updated_at = $3 WHERE id = $1`, id, out, now)
	return err
}

// proposalLimited applies max_open_proposals and max_per_week of the role and max_in_review of
// the tenant (INS-4, PRP-5).
func proposalLimited(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, a proposalAuthor) (bool, error) {
	maxOpen, maxWeek := defaultMaxOpenProposals, defaultMaxPerWeek
	if r, ok := cat.Roles[a.Role]; ok && r.Insight != nil {
		if r.Insight.MaxOpenProposals != nil {
			maxOpen = *r.Insight.MaxOpenProposals
		}
		if r.Insight.MaxPerWeek != nil {
			maxWeek = *r.Insight.MaxPerWeek
		}
	}
	maxReview := defaultMaxInReview
	if tv := cat.Tenants[a.Tenant]; tv != nil && tv.Spec.Insights.MaxInReview != nil {
		maxReview = *tv.Spec.Insights.MaxInReview
	}
	var open, week, review int
	if err := tx.QueryRow(ctx, `SELECT
		count(*) FILTER (WHERE role = $2 AND state = ANY($3)),
		count(*) FILTER (WHERE role = $2 AND created_at > $4),
		count(*) FILTER (WHERE state IN ('screened', 'in_review'))
		FROM proposals WHERE tenant = $1`, a.Tenant, a.Role, openProposalStates, now.AddDate(0, 0, -7)).Scan(&open, &week, &review); err != nil {
		return false, err
	}
	if open >= maxOpen || week >= maxWeek || review >= maxReview {
		return true, nil
	}
	// One open proposal per bot on top of the role limits; a worker without a numbered bot
	// counts as one bot. A proposal parked as conflict behind another author's change that went
	// to realizing earlier does not take the slot until the conflict ends (PRP-16, PRP-9).
	author, col := a.Acting.Bot, `record->'origin'->>'acting_bot'`
	if author == "" {
		author, col = a.Worker, `CASE WHEN record->'origin'->>'acting_bot' IS NULL THEN worker END`
	}
	var mine int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM proposals p WHERE tenant = $1 AND state = ANY($2) AND `+col+` = $3
		AND NOT (state = 'parked' AND record->>'state_reason' = 'conflict' AND EXISTS (SELECT 1 FROM proposals h
			WHERE h.tenant = p.tenant AND h.id IN (SELECT jsonb_array_elements_text(coalesce(p.record->'conflicts_with', '[]'::jsonb)))
			AND coalesce(h.record->'origin'->>'acting_bot', h.worker) <> coalesce(p.record->'origin'->>'acting_bot', p.worker)))`,
		a.Tenant, openProposalStates, author).Scan(&mine); err != nil {
		return false, err
	}
	return mine >= 1, nil
}

// selfInfluence is the share of the author's and its role's own points in the evidence series
// over the cited window; self_evidence is an author share of 0.3 or more (PRP-22). Rollups
// keep no producer, so a window longer than the raw points of its series is counted over the
// points kept and marked partial.
func selfInfluence(ctx context.Context, tx pgx.Tx, now time.Time, a proposalAuthor, ev []evidenceRef, series map[string]seriesDef) ([]any, bool, error) {
	out := []any{}
	self := false
	done := map[string]bool{}
	for _, e := range ev {
		if done[e.Series] {
			continue
		}
		done[e.Series] = true
		days := obsWindows[e.Window]
		var total, author, role int
		if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE worker = $4), count(*) FILTER (WHERE role = $5)
			FROM obs_points WHERE tenant = $1 AND series = $2 AND at >= $3`, a.Tenant, e.Series, now.AddDate(0, 0, -days), a.Worker, a.Role).
			Scan(&total, &author, &role); err != nil {
			return nil, false, err
		}
		as, rs := 0.0, 0.0
		if total > 0 {
			as, rs = float64(author)/float64(total), float64(role)/float64(total)
		}
		if as >= 0.3 {
			self = true
		}
		si := map[string]any{"series": e.Series, "author_share": as, "role_share": rs}
		if d, ok := series[e.Series]; ok && days > d.RawDays {
			si["partial"] = true
		}
		out = append(out, si)
	}
	return out, self, nil
}

func strAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
