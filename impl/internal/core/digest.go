// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

const (
	// The digest when the tenant sets none: Monday 09:00 in the tenant's time zone, five items
	// (DIG-1).
	defaultDigestSchedule = "0 9 * * 1"
	defaultDigestItems    = 5
	// A slot older than a day is not sent late: the next one carries its items.
	digestLate = 24 * time.Hour
	// Urgent items arrive outside the schedule at most once a day per tenant (DIG-1).
	urgentEvery = 24 * time.Hour
	// The digest record holds at most 30 items and 20 cycle-off reasons.
	maxDigestItems = 30
	maxLoopOff     = 20

	proposalDigest  = "proposal_digest"
	proposalCard    = "proposal_card"
	realizationCard = "realization_card"
)

// digestLoop sends the proposal digest of every tenant with a present_to address: on the
// digest slot, and outside it for an urgent item (DIG-1, DIG-2). A slot has one row in
// proposal_digests, so a second copy of the loop sends nothing twice.
func (c *Core) digestLoop(ctx context.Context, cat *Catalog) error {
	var errs []error
	for _, name := range slices.Sorted(mapKeys(cat.Tenants)) {
		tv := cat.Tenants[name]
		if tv.Spec.Insights.PresentTo == nil || tv.Spec.Insights.PresentTo == "" {
			continue
		}
		if err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error { return c.digestTenant(ctx, tx, now, cat, tv) }); err != nil {
			errs = append(errs, fmt.Errorf("digest %s: %w", name, err))
		}
	}
	return errors.Join(errs...)
}

func (c *Core) digestTenant(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300120, hashtext($1))`, tv.Name); err != nil {
		return err
	}
	sched := tv.Spec.Insights.Digest.Schedule
	if sched == "" {
		sched = defaultDigestSchedule
	}
	spec, err := parseCron(sched)
	if err != nil {
		return nil
	}
	loc := tenantLocation(cat, tv.Name)
	if slot, ok := latestSlot(spec, loc, now); ok && now.Sub(slot) <= digestLate {
		var sent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM proposal_digests WHERE tenant = $1 AND slot_at = $2 AND NOT urgent)`, tv.Name, slot).Scan(&sent); err != nil {
			return err
		}
		if !sent {
			return c.sendDigest(ctx, tx, now, cat, tv, slot, false)
		}
	}
	var urgentSent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM proposal_digests WHERE tenant = $1 AND urgent AND slot_at > $2)`, tv.Name, now.Add(-urgentEvery)).Scan(&urgentSent); err != nil {
		return err
	}
	if urgentSent {
		return nil
	}
	var urgent bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM proposals WHERE tenant = $1 AND state = 'recommended'
		AND record->'body'->>'category' = 'security' AND record->'body'->'problem'->>'severity' = 'high')`, tv.Name).Scan(&urgent); err != nil {
		return err
	}
	if urgent {
		return c.sendDigest(ctx, tx, now, cat, tv, now, true)
	}
	return nil
}

// latestSlot is the last slot of a schedule at or before now within the late window and an hour
// (a slot older than digestLate is not sent anyway).
func latestSlot(spec *cronSpec, loc *time.Location, now time.Time) (time.Time, bool) {
	var last time.Time
	found := false
	for s, ok := spec.nextSlot(now.Add(-digestLate-time.Hour), loc); ok && !s.At.After(now); s, ok = spec.nextSlot(s.At, loc) {
		last, found = s.At, true
	}
	return last, found
}

var severityOrder = map[string]int{"high": 0, "medium": 1, "low": 2}

// sendDigest assembles one digest from the records by the code template: the recommended items
// by importance whose evidence still holds, at most max_items (only urgent ones out of
// schedule), with the cycle-off reasons on a scheduled digest (DIG-1, INS-6, PRP-2).
func (c *Core) sendDigest(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, slot time.Time, urgent bool) error {
	rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND state = 'recommended' ORDER BY created_at, id FOR UPDATE`, tv.Name)
	if err != nil {
		return err
	}
	idList, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	var cands []*proposalRow
	for _, id := range idList {
		p, err := loadProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		b := p.body()
		if urgent && (b.Category != "security" || b.Problem.Severity != "high") {
			continue
		}
		cands = append(cands, p)
	}
	slices.SortStableFunc(cands, func(a, b *proposalRow) int {
		ba, bb := a.body(), b.body()
		if d := cmp.Compare(severityOrder[ba.Problem.Severity], severityOrder[bb.Problem.Severity]); d != 0 {
			return d
		}
		ma, _ := toFloat(mmapAny(a.Rec["verdict"])["weighted_mean"])
		mb, _ := toFloat(mmapAny(b.Rec["verdict"])["weighted_mean"])
		return cmp.Compare(mb, ma)
	})
	limit := defaultDigestItems
	if n := tv.Spec.Insights.Digest.MaxItems; n > 0 {
		// The digest record holds at most 30 items (digest.schema.json).
		limit = min(n, maxDigestItems)
	}
	var items []*proposalRow
	for _, p := range cands {
		if len(items) >= limit {
			break
		}
		// A field another accepted change still locks parks the item (PRP-9).
		if holders, err := lockHolders(ctx, tx, p.Tenant, p.ID, lockKeys(p.body())); err != nil {
			return err
		} else if len(holders) > 0 {
			p.Rec["conflicts_with"] = []any{holders[0]}
			if err := c.moveProposal(ctx, tx, now, p, "parked", "conflict"); err != nil {
				return err
			}
			if err := c.planProposalCardUpdates(ctx, tx, now, p); err != nil {
				return err
			}
			continue
		}
		// The evidence is reread before showing; a drift parks the item (PRP-2).
		ok, err := c.evidenceHolds(ctx, tx, now, cat, p)
		if err != nil {
			return err
		}
		if !ok {
			if err := c.moveProposal(ctx, tx, now, p, "parked", "evidence_drift"); err != nil {
				return err
			}
			if err := c.planProposalCardUpdates(ctx, tx, now, p); err != nil {
				return err
			}
			continue
		}
		items = append(items, p)
	}
	var off []loopReason
	if !urgent {
		if off, err = loopOff(ctx, tx, cat, tv.Name); err != nil {
			return err
		}
		off = off[:min(len(off), maxLoopOff)]
	}
	if len(items) == 0 && len(off) == 0 {
		// Nothing to say: the slot stays open for a day, so items recommended later in it
		// still go out.
		return nil
	}
	body := map[string]any{"tenant": tv.Name, "address": asString(tv.Spec.Insights.PresentTo), "period": slot.In(tenantLocation(cat, tv.Name)).Format("2006-01-02"),
		"items": []any{}, "loop_off": nil, "intro": nil}
	if len(off) > 0 {
		reasons := []any{}
		for _, r := range off {
			m := map[string]any{"reason": r.Reason, "role": nil}
			if r.Role != "" {
				m["role"] = r.Role
			}
			reasons = append(reasons, m)
		}
		body["loop_off"] = map[string]any{"reasons": reasons}
	}
	shown, reached, err := c.planDigest(ctx, tx, now, tv, items, off)
	if err != nil {
		return err
	}
	if !reached {
		// Nobody gets the digest (no bound chat, no reachable member): nothing is presented
		// and the slot stays open.
		return nil
	}
	body["items"] = shown
	if err := c.spec.Validate("urn:botaru:schema:digest:1", body); err != nil {
		return fmt.Errorf("digest body: %w", err)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO proposal_digests (id, tenant, slot_at, urgent, body, created_at) VALUES ($1, $2, $3, $4, $5, $6)`,
		ids.New(ids.Delivery), tv.Name, slot, urgent, raw, now); err != nil {
		return err
	}
	// The items are presented: each gets the nonce of its card (DIG-1).
	for _, p := range items {
		if _, err := tx.Exec(ctx, `UPDATE proposals SET nonce = $2, presented_at = $3 WHERE id = $1`, p.ID, ids.Secret(16), now); err != nil {
			return err
		}
		if err := c.moveProposal(ctx, tx, now, p, "presented", ""); err != nil {
			return err
		}
		if err := c.planProposalCards(ctx, tx, now, tv, p); err != nil {
			return err
		}
	}
	return nil
}

// evidenceHolds rereads the evidence of a proposal with its author's read and compares it with
// the values the core saw at creation, within each series' tolerance (PRP-2).
func (c *Core) evidenceHolds(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, p *proposalRow) (bool, error) {
	origin := mmapAny(p.Rec["origin"])
	var typ string
	if err := tx.QueryRow(ctx, `SELECT type FROM tasks WHERE id = $1`, origin["task"]).Scan(&typ); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return false, err
	}
	a := proposalAuthor{Tenant: p.Tenant, Role: p.Role, Mark: asString(p.Rec["data_mark"]), Readable: c.readableSeries(cat, p.Tenant, p.Role, typ)}
	series := c.tenantSeries(cat, p.Tenant)
	seen, _ := p.Rec["evidence_seen"].([]any)
	for _, s := range seen {
		sm, _ := s.(map[string]any)
		ref := mmapAny(sm["ref"])
		e := evidenceRef{Series: asString(ref["series"]), Subject: asString(ref["subject"]), Window: asString(ref["window"]), Rollup: asString(ref["rollup"]), Slot: asString(ref["slot"])}
		v, ok, err := c.evidenceValue(ctx, tx, now, cat, a, e)
		if err != nil {
			return false, err
		}
		if !ok || !withinTolerance(series[e.Series], sm["value"], v) {
			return false, nil
		}
	}
	return true, nil
}

// digestShow says how an item goes to a recipient whose ceiling is mark: in full, or only its
// code heading when the item's mark is above it (DIG-2).
func (c *Core) digestShow(item, ceiling string) string {
	if ceiling != "" && c.markWithin(item, ceiling) {
		return "full"
	}
	return "title_only"
}

// planDigest plans the digest text to the present_to address: a bound chat with its ceiling,
// or the direct chat of every active member of a group with that person's ceiling (DIG-2). It
// returns the items as the address shows them and whether any target was reached.
func (c *Core) planDigest(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, items []*proposalRow, off []loopReason) ([]any, bool, error) {
	addr := asString(tv.Spec.Insights.PresentTo)
	kind, name, _ := strings.Cut(addr, ":")
	type target struct {
		channel, chat, recipient, ceiling string
	}
	var targets []target
	switch kind {
	case "chat":
		b, err := bindingByName(ctx, tx, tv.Name, name)
		if err != nil {
			return nil, false, err
		}
		if b != nil {
			targets = append(targets, target{channel: b.Channel, chat: b.Chat, ceiling: b.Ceiling})
		}
	case "group":
		people, err := groupMembers(ctx, tx, now, tv, name)
		if err != nil {
			return nil, false, err
		}
		for _, p := range people {
			info, err := loadPrincipal(ctx, tx, now, tv.Name, p)
			if err != nil {
				return nil, false, err
			}
			channel, chat, ok, err := cardChat(ctx, tx, tv, p)
			if err != nil {
				return nil, false, err
			}
			if ok {
				targets = append(targets, target{channel: channel, chat: chat.ID(), recipient: p, ceiling: c.personMark(tv, groupClosure(tv, info.Groups))})
			}
		}
	}
	var reasons []string
	for _, r := range off {
		if r.Role != "" {
			reasons = append(reasons, r.Reason+":"+r.Role)
		} else {
			reasons = append(reasons, r.Reason)
		}
	}
	shown := []any{}
	for i, t := range targets {
		var lines []string
		mark := ""
		for _, p := range items {
			b := p.body()
			itemMark := asString(p.Rec["data_mark"])
			show := c.digestShow(itemMark, t.ceiling)
			line := fmt.Sprintf("%s: %s (%s)", b.Category, b.Problem.Code, b.Problem.Severity)
			if show == "full" {
				if mo, ok := c.markOrder(itemMark); ok {
					if cur, ok2 := c.markOrder(mark); !ok2 || mo > cur {
						mark = itemMark
					}
				}
				line += " " + b.Change.Kind
			} else {
				line += " [details in a private chat]"
			}
			lines = append(lines, "- "+line)
			if i == 0 {
				us := mmapAny(p.Rec["untrusted_seen"])
				shown = append(shown, map[string]any{"proposal": p.ID, "kind": "recommended", "data_mark": itemMark, "show": show, "block": nil,
					"untrusted_seen": us["value"] == true, "self_evidence": p.Rec["self_evidence"] == true})
			}
		}
		params := map[string]any{"count": len(items), "items": strings.Join(lines, "\n"), "loop_off": strings.Join(reasons, ", ")}
		key := "proposal.digest"
		switch {
		case len(items) == 0:
			key = "proposal.digest.loop_off"
		case len(reasons) > 0:
			key = "proposal.digest.with_loop_off"
		}
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tv.Name, Kind: proposalDigest, Addr: addr, TargetKind: "chat", TargetID: t.chat, ExecKind: "channel", ExecName: t.channel,
			Recipient: t.recipient, Body: map[string]any{"form": "text_code", "text_key": key, "params": params}, DataMark: mark, DataOrigin: "code",
		}); err != nil {
			return nil, false, err
		}
	}
	return shown, len(targets) > 0, nil
}

// groupMembers are the active people of a group of the tenant, directly or through a group
// that inherits it (PRM-2).
func groupMembers(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, group string) ([]string, error) {
	via := []string{group}
	for g := range tv.Groups {
		if g != group && slices.Contains(groupClosure(tv, []string{g}), group) {
			via = append(via, g)
		}
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT m.principal FROM memberships m JOIN principals p ON p.tenant = m.tenant AND p.id = m.principal
		WHERE m.tenant = $1 AND m.grp = ANY($2) AND m.state = 'active' AND (m.until IS NULL OR m.until > $3) AND p.state = 'active' AND p.kind = 'human'
		ORDER BY m.principal`, tv.Name, via, now)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// deciders are the active people of the tenant who may decide on a category and whose ceiling
// covers the mark: the recipients of a proposal card (DIG-2, DIG-3).
func (c *Core) deciders(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, category, mark string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT DISTINCT m.principal FROM memberships m JOIN principals p ON p.tenant = m.tenant AND p.id = m.principal
		WHERE m.tenant = $1 AND m.state = 'active' AND (m.until IS NULL OR m.until > $2) AND p.state = 'active' AND p.kind = 'human'
		ORDER BY m.principal`, tv.Name, now)
	if err != nil {
		return nil, err
	}
	all, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range all {
		info, err := loadPrincipal(ctx, tx, now, tv.Name, p)
		if err != nil {
			return nil, err
		}
		groups := groupClosure(tv, info.Groups)
		if hasCap(tv, groups, "proposal.decide:"+category) && c.digestShow(mark, c.personMark(tv, groups)) == "full" {
			out = append(out, p)
		}
	}
	return out, nil
}

// planProposalCards plans the card of a presented proposal to the direct chat of every person
// who may decide on it and whose ceiling covers its mark (DIG-2, DIG-3).
func (c *Core) planProposalCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow) error {
	mark := asString(p.Rec["data_mark"])
	people, err := c.deciders(ctx, tx, now, tv, p.body().Category, mark)
	if err != nil {
		return err
	}
	for _, person := range people {
		if err := c.planProposalCard(ctx, tx, now, tv, p, proposalCard, person, nil); err != nil {
			return err
		}
	}
	return nil
}

func (c *Core) planProposalCard(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, p *proposalRow, kind, person string, extra map[string]any) error {
	channel, chat, ok, err := cardChat(ctx, tx, tv, person)
	if err != nil || !ok {
		return err
	}
	body := map[string]any{"form": "card", "card": kind}
	for k, v := range extra {
		body[k] = v
	}
	_, err = c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tv.Name, Kind: kind, Addr: asString(tv.Spec.Insights.PresentTo), TargetKind: "chat", TargetID: chat.ID(),
		ExecKind: "channel", ExecName: channel, Recipient: person, Body: body, DataMark: asString(p.Rec["data_mark"]), DataOrigin: "code",
		SubjectKind: "proposal", SubjectID: p.ID,
	})
	return err
}

// planProposalCardUpdates plans an update of every delivered card of a proposal, so each
// recipient sees its new state.
func (c *Core) planProposalCardUpdates(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	rows, err := tx.Query(ctx, `SELECT id, kind, exec_name, target_id, coalesce(recipient, ''), coalesce(data_mark, '') FROM deliveries
		WHERE subject_kind = 'proposal' AND subject_id = $1 AND kind IN ('proposal_card', 'realization_card', 'rollback_card') AND state IN ('planned', 'sending', 'delivered')
		ORDER BY seq`, p.ID)
	if err != nil {
		return err
	}
	type target struct{ id, kind, channel, chat, recipient, mark string }
	var ts []target
	for rows.Next() {
		var t target
		if err := rows.Scan(&t.id, &t.kind, &t.channel, &t.chat, &t.recipient, &t.mark); err != nil {
			rows.Close()
			return err
		}
		ts = append(ts, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, t := range ts {
		if _, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: p.Tenant, Kind: cardUpdate, Addr: "proposal", TargetKind: "chat", TargetID: t.chat,
			ExecKind: "channel", ExecName: t.channel, Recipient: t.recipient, Body: map[string]any{"form": "card", "card": t.kind, "of": t.id},
			DataMark: t.mark, DataOrigin: "code", SubjectKind: "proposal", SubjectID: p.ID,
		}); err != nil {
			return err
		}
	}
	return nil
}

// loadProposal is a proposal record under the lock its transaction already holds or takes.
func loadProposal(ctx context.Context, tx pgx.Tx, id string) (*proposalRow, error) {
	p := &proposalRow{ID: id}
	var raw []byte
	var nonce *string
	var presented *time.Time
	if err := tx.QueryRow(ctx, `SELECT tenant, role, state, record, state_at, nonce, presented_at FROM proposals WHERE id = $1 FOR UPDATE`, id).
		Scan(&p.Tenant, &p.Role, &p.State, &raw, &p.StateAt, &nonce, &presented); err != nil {
		return nil, err
	}
	p.Nonce, p.PresentedAt = deref(nonce), presented
	return p, json.Unmarshal(raw, &p.Rec)
}

// readProposal is a proposal record without a lock, for drawing its card.
func readProposal(ctx context.Context, tx pgx.Tx, id string) (*proposalRow, error) {
	p := &proposalRow{ID: id}
	var raw []byte
	var nonce *string
	if err := tx.QueryRow(ctx, `SELECT tenant, role, state, record, state_at, nonce, presented_at FROM proposals WHERE id = $1`, id).
		Scan(&p.Tenant, &p.Role, &p.State, &raw, &p.StateAt, &nonce, &p.PresentedAt); err != nil {
		return nil, err
	}
	p.Nonce = deref(nonce)
	return p, json.Unmarshal(raw, &p.Rec)
}

// proposalCard draws the card of a proposal from its record (DIG-3, PRP-18): the code heading,
// the change and the promised benefit, the verdict, the state and the bottom line; the buttons
// of a presented proposal card are "Accept", "Reject", "Postpone" while it may still be
// postponed, and "Details". There is no model text on the card.
func (c *Core) proposalCardFields(cat *Catalog, p *proposalRow, kind string, reminder bool) map[string]any {
	b := p.body()
	tv := cat.Tenants[p.Tenant]
	title := fmt.Sprintf("%s: %s (%s)", b.Category, b.Problem.Code, b.Problem.Severity)
	ctxt := map[string]any{"change": b.Change.Kind}
	if b.Benefit != nil {
		ctxt["metric"], ctxt["direction"] = b.Benefit.Metric, b.Benefit.Direction
		if b.Benefit.ExpectedChangePP != nil {
			ctxt["expected_change_pp"] = *b.Benefit.ExpectedChangePP
		}
		if b.Benefit.ExpectedChangePct != nil {
			ctxt["expected_change_pct"] = *b.Benefit.ExpectedChangePct
		}
	}
	if r := asString(mmapAny(mmapAny(p.Rec["body"])["risk"])["level"]); r != "" {
		ctxt["risk"] = r
	}
	v := mmapAny(p.Rec["verdict"])
	if m, ok := v["weighted_mean"]; ok && m != nil {
		ctxt["weighted_mean"] = m
	}
	if p.Rec["self_evidence"] == true {
		ctxt["self_evidence"] = true
	}
	// The local times of a changed schedule that disappear or repeat (TIM-5).
	for k, v := range mmapAny(p.Rec["schedule_times"]) {
		ctxt[k] = v
	}
	us := mmapAny(p.Rec["untrusted_seen"])
	card := map[string]any{
		"card": kind, "subject_kind": "proposal", "id": p.ID, "nonce": p.Nonce, "title": cut(title, 200), "context": ctxt,
		"requested_by":      map[string]any{"initiator": "system", "source": "proposal", "role": p.Role},
		"initiator_display": "role " + p.Role, "untrusted_seen": us["value"] == true, "state": p.State,
	}
	if us["value"] == true {
		card["untrusted_source"] = "proposal evidence"
	}
	if reminder {
		card["reminder"] = true
	}
	button := func(code string) map[string]any {
		return map[string]any{"code": code, "subject_kind": "proposal", "id": p.ID, "nonce": p.Nonce}
	}
	buttons := []any{}
	switch kind {
	case proposalCard:
		until := p.StateAt
		if p.PresentedAt != nil {
			until = *p.PresentedAt
		}
		if tv != nil {
			until = until.Add(durationOr(tv.Spec.Insights.DecideTTL, defaultDecideTTL))
		}
		card["expires_at"] = fmtTime(until)
		card["footer"] = map[string]any{"why_you": "proposal.decide:" + b.Category, "until": fmtTime(until), "on_silence": "the proposal expires, nothing changes"}
		if p.State == "presented" && p.Nonce != "" {
			buttons = append(buttons, button("accept"), button("reject"))
			if n, _ := toFloat(mmapAny(p.Rec["decision"])["defer_count"]); int(n) < maxDefers {
				buttons = append(buttons, button("defer"))
			}
			buttons = append(buttons, button("details"))
		}
	case realizationCard:
		r := mmapAny(p.Rec["realization"])
		by := asString(r["realize_by"])
		card["title"] = cut("Carry out: "+title, 200)
		if ops := b.Change.ManifestChange; ops != nil {
			for i, op := range ops.Ops {
				if i >= 5 {
					break
				}
				ctxt[fmt.Sprintf("op%d", i+1)] = fmt.Sprintf("%s %s %s = %v", op.Target.Kind, op.Target.Name, op.Field, op.Value)
			}
		}
		ctxt["trailer"] = "Botaru-Proposal: " + p.ID
		if by != "" {
			card["expires_at"] = by
		}
		card["footer"] = map[string]any{"why_you": "the executor of the accepted proposal", "until": by, "on_silence": "the realization fails, the trial is canceled"}
		if by == "" {
			delete(card["footer"].(map[string]any), "until")
		}
	case rollbackCard:
		r := mmapAny(p.Rec["realization"])
		card["title"] = cut("Revert: "+title, 200)
		for i, e := range asList(r["revert_patch"]) {
			if i >= 5 {
				break
			}
			em := mmapAny(e)
			tg := mmapAny(em["target"])
			ctxt[fmt.Sprintf("revert%d", i+1)] = fmt.Sprintf("%s %s %s = %v", tg["kind"], tg["name"], em["field"], em["previous"])
		}
		t := mmapAny(p.Rec["trial"])
		for _, g := range asList(t["guards"]) {
			if gm := mmapAny(g); gm["broken_at"] != nil {
				ctxt["guard_broken"] = gm["series"]
			}
		}
		if o := t["outcome"]; o != nil {
			ctxt["outcome"] = o
		}
		ctxt["trailer"] = "Botaru-Revert: " + p.ID + " harm_observed"
		card["footer"] = map[string]any{"why_you": "the executor of the accepted proposal", "on_silence": "the change trial runs to the end"}
	}
	card["buttons"] = buttons
	return card
}

// proposalCardBody draws a proposal card or its update at hand-out; an update names the
// delivered card message it edits, and is dropped when that card never arrived.
func (c *Core) proposalCardBody(ctx context.Context, tx pgx.Tx, cat *Catalog, kind, id, target, recipient, channel string, body map[string]any) (map[string]any, string, error) {
	p, err := readProposal(ctx, tx, id)
	if err != nil {
		return nil, "", err
	}
	cardKind, _ := body["card"].(string)
	reminder, _ := body["reminder"].(bool)
	card := c.proposalCardFields(cat, p, cardKind, reminder)
	replaces := ""
	if kind == cardUpdate {
		of, _ := body["of"].(string)
		var ext *string
		err := tx.QueryRow(ctx, `SELECT external_id FROM deliveries WHERE id = $6 AND subject_kind = 'proposal' AND subject_id = $1 AND kind = $2
			AND exec_name = $3 AND target_id = $4 AND recipient IS NOT DISTINCT FROM $5 AND state = 'delivered' AND external_id IS NOT NULL`,
			id, cardKind, channel, target, nullable(recipient), of).Scan(&ext)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, "", err
		}
		if ext == nil {
			return nil, "", nil
		}
		replaces = *ext
	}
	return map[string]any{"form": "card", "card": card}, replaces, nil
}

// proposalDetails is P1 card.details on a proposal card: the closed fields of the record to
// the same chat, and the opening is recorded for the decision (DIG-3).
func (c *Core) proposalDetails(ctx context.Context, w *Worker, ob OnBehalf, req CardOp) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		p, err := readProposal(ctx, tx, req.ID)
		if errors.Is(err, pgx.ErrNoRows) || err == nil && p.Tenant != w.Tenant {
			return refuse("not_found", "no such proposal")
		}
		if err != nil {
			return err
		}
		if p.Nonce == "" || p.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "proposal", p.ID, []string{proposalCard})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO card_views (subject_kind, subject_id, principal, opened_at) VALUES ('proposal', $1, $2, $3) ON CONFLICT DO NOTHING`,
			p.ID, press.principal, now); err != nil {
			return err
		}
		card := c.proposalCardFields(cat, p, proposalCard, false)
		fields := mmapAny(card["context"])
		fields["title"], fields["state"] = card["title"], p.State
		b := mmapAny(p.Rec["body"])
		for _, k := range []string{"problem", "cost"} {
			if v, ok := b[k]; ok {
				fields[k] = v
			}
		}
		if v := mmapAny(p.Rec["verdict"]); v != nil {
			fields["verdict"] = v["outcome"]
			fields["lenses"] = v["lenses"]
		}
		out = map[string]any{"template": "card.details.proposal", "fields": fields, "deliver_to": "same_chat"}
		return nil
	})
	return out, err
}
