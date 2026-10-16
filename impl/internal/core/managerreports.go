// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The farm manager's reach to people and its reports from v0.3 (MGR-50 to MGR-54): the notice
// manager_attention from closed fields, the analysis after a resolved incident with its report
// postmortem_report, the buttons "Useful" and "Not needed" of both (P1 card.feedback), the
// weekly holder_report and the limits of the manager's proposals.

const (
	attentionToolName = "attention_notice"
	attentionNotice   = "manager_attention"
	postmortemType    = "manager.postmortem.v1"
	postmortemReport  = "postmortem_report"
	holderReport      = "holder_report"
	// defaultNoticesPerPersonDay is attention.manager_notices_per_person_day; a tenant may only
	// lower it (MGR-52).
	defaultNoticesPerPersonDay = 3
	// defaultPostmortemDayEUR is farm_manager.postmortem_max_eur.day when the charter sets none
	// (MGR-53).
	defaultPostmortemDayEUR = 0.40
	// defaultCardsPerPersonDay is attention.noncritical_per_day (APR-15), the per-person limit of
	// the analysis reports (MGR-53).
	defaultCardsPerPersonDay = 10
	// paragraphEvery is how often one class and object get a model paragraph (MGR-53).
	paragraphEvery = 7 * 24 * time.Hour
	// managerProposalsMonth is how many proposals the manager submits to a tenant a month
	// (MGR-51).
	managerProposalsMonth = 2
)

// attentionTool is attention_notice in a decision attempt of the farm manager seat from v0.3
// (MGR-52): only closed codes, an object id and a proposal reference.
func (c *Core) attentionTool(cat *Catalog, g *toolsGrant) *catalogTool {
	if !claimsV03() || g.Purpose != "manage" || profileOf(cat, g.Role) != "farm-manager" {
		return nil
	}
	codes := func(kind string) []any {
		out := []any{}
		if reg, err := c.spec.Registry("spec/model/manager-attention.yaml"); err == nil {
			for _, e := range reg.Entries {
				if e["kind"] == kind {
					out = append(out, e["id"])
				}
			}
		}
		return out
	}
	id := map[string]any{"type": "string", "pattern": "^[a-z]{1,8}_[0-9A-HJKMNP-TV-Z]{26}$"}
	schema := map[string]any{"type": "object", "additionalProperties": false, "required": []any{"sees", "object", "threat", "proposal"},
		"properties": map[string]any{
			"sees": map[string]any{"enum": codes("sees")}, "threat": map[string]any{"enum": codes("threat")}, "object": id,
			"proposal": map[string]any{"type": "object", "additionalProperties": false, "minProperties": 1, "maxProperties": 1,
				"properties": map[string]any{"op": map[string]any{"type": "string", "pattern": "^[a-z][a-z0-9_]*$", "maxLength": 64}, "ref": id}},
		}}
	desc := "Tell the people of this tenant who have rights on an object what you see, what it threatens and what you propose, in codes only."
	return &catalogTool{Name: attentionToolName, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: schema}
}

// attentionObject finds an object of the tenant by its id and the capabilities that give a
// person rights on it: a task by its type, any other object by incident.manage.
func attentionObject(ctx context.Context, tx pgx.Tx, tenant, id string) ([]string, bool, error) {
	prefix, _, _ := strings.Cut(id, "_")
	q := map[string]string{"t": `SELECT type FROM tasks WHERE id = $1 AND tenant = $2`, "wk": `SELECT role FROM workers WHERE id = $1 AND tenant = $2`,
		"bot": `SELECT role FROM bots WHERE id = $1 AND tenant = $2`, "inc": `SELECT class FROM incidents WHERE id = $1 AND tenant = $2`,
		"h": `SELECT id FROM holds WHERE id = $1 AND tenant = $2`}[prefix]
	if q == "" {
		return nil, false, nil
	}
	var v string
	err := tx.QueryRow(ctx, q, id, tenant).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if prefix == "t" {
		return []string{"task:" + v, "incident.manage"}, true, nil
	}
	return []string{"incident.manage"}, true, nil
}

// attentionRefIssued says whether a reference names a card or a proposal that code issued in
// the tenant: a proposal, an approval, a control card or a takeover card.
func attentionRefIssued(ctx context.Context, tx pgx.Tx, tenant, ref string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM proposals WHERE id = $1 AND tenant = $2)
		OR EXISTS (SELECT 1 FROM approvals WHERE id = $1 AND tenant = $2)
		OR EXISTS (SELECT 1 FROM control_cards WHERE id = $1 AND tenant = $2)
		OR EXISTS (SELECT 1 FROM takeovers WHERE id = $1 AND tenant = $2)`, ref, tenant).Scan(&ok)
	return ok, err
}

// noticesPerPersonDay is attention.manager_notices_per_person_day of the tenant, at most 3.
func noticesPerPersonDay(tv *TenantView) int {
	if v, ok := mnum(mmap(mmap(tv.Raw, "spec"), "attention"), "manager_notices_per_person_day"); ok && v >= 0 && v < defaultNoticesPerPersonDay {
		return int(v)
	}
	return defaultNoticesPerPersonDay
}

// attentionToolCall builds manager_attention from the closed fields code checked and sends it
// to the people of the tenant with rights on the object, in private chats, within the daily
// limit per person; the seat gets only the count of the people reached (MGR-52).
func (c *Core) attentionToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, offered bool, args map[string]any) (*mcp.CallResult, error) {
	tool := c.attentionTool(cat, g)
	if !offered || tool == nil {
		return toolAnswer(map[string]any{"code": "forbidden"}, true), nil
	}
	if err := c.rs.validate(tool.Schema, args); err != nil {
		return toolAnswer(map[string]any{"code": "input_schema_invalid"}, true), nil
	}
	acting, err := leaseActing(ctx, tx, g.Task, g.Worker)
	if err != nil {
		return nil, err
	}
	m, err := loadManagerSeat(ctx, tx, false)
	if err != nil {
		return nil, err
	}
	if m == nil || m.State != "held" || m.Holder != acting.Bot || m.Epoch != acting.SeatEpoch {
		return toolAnswer(map[string]any{"code": "fenced"}, true), nil
	}
	tv := cat.Tenants[g.Tenant]
	if tv == nil || !managerEnabled(cat, g.Tenant) {
		return toolAnswer(map[string]any{"code": "manager_disabled"}, true), nil
	}
	object := mstr(args, "object")
	caps, ok, err := attentionObject(ctx, tx, g.Tenant, object)
	if err != nil {
		return nil, err
	}
	prop := mmap(args, "proposal")
	if ref := mstr(prop, "ref"); ok && ref != "" {
		if ok, err = attentionRefIssued(ctx, tx, g.Tenant, ref); err != nil {
			return nil, err
		}
	}
	if !ok {
		return toolAnswer(map[string]any{"code": "not_found"}, true), nil
	}
	chats, err := c.adminChats(ctx, tx, now, tv, "", caps...)
	if err != nil {
		return nil, err
	}
	limit, day := noticesPerPersonDay(tv), tenantDay(tv, now)
	params := map[string]any{"sees": mstr(args, "sees"), "object": object, "threat": mstr(args, "threat"), "op": mstr(prop, "op"), "ref": mstr(prop, "ref")}
	sent := map[string]bool{}
	for _, ch := range chats {
		if !sent[ch.Principal] {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM manager_feedback WHERE tenant = $1 AND recipient = $2 AND kind = $3 AND created_at >= $4`,
				g.Tenant, ch.Principal, attentionNotice, day).Scan(&n); err != nil {
				return nil, err
			}
			if n >= limit {
				continue
			}
		}
		if err := c.feedbackCard(ctx, tx, now, g.Tenant, attentionNotice, g.Task, ch, "notice.manager_attention", params, !sent[ch.Principal]); err != nil {
			return nil, err
		}
		sent[ch.Principal] = true
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: g.Tenant, Kind: "command", Actor: Actor{Principal: acting.Bot, Via: "seat", Role: g.Role, Task: g.Task},
		Command: attentionToolName, Target: &Target{Kind: "object", ID: object}, OK: true, After: map[string]any{"sees": params["sees"], "threat": params["threat"], "people": len(sent)},
	}); err != nil {
		return nil, err
	}
	return toolAnswer(map[string]any{"people": len(sent)}, false), nil
}

// feedbackCard plans a notice with the buttons "Useful" and "Not needed" to one chat; a new
// person gets a feedback record, which the limits count and P1 card.feedback answers once.
func (c *Core) feedbackCard(ctx context.Context, tx pgx.Tx, now time.Time, tenant, kind, task string, ch adminChat, textKey string, params map[string]any, record bool) error {
	id, nonce := "", ""
	if record {
		id, nonce = ids.New(ids.Notice), ids.Secret(16)
		if _, err := tx.Exec(ctx, `INSERT INTO manager_feedback (id, tenant, kind, recipient, nonce, task, created_at) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
			id, tenant, kind, ch.Principal, nonce, nullable(task), now); err != nil {
			return err
		}
	} else if err := tx.QueryRow(ctx, `SELECT id, nonce FROM manager_feedback WHERE tenant = $1 AND kind = $2 AND recipient = $3 AND task IS NOT DISTINCT FROM $4
		ORDER BY created_at DESC LIMIT 1`, tenant, kind, ch.Principal, nullable(task)).Scan(&id, &nonce); err != nil {
		return err
	}
	buttons := []any{}
	for _, code := range []string{"useful", "not_needed"} {
		buttons = append(buttons, map[string]any{"code": code, "op": "card.feedback", "id": id, "nonce": nonce})
	}
	card := map[string]any{"card": kind, "id": id, "nonce": nonce, "title": cut(c.texts.Render("en", textKey, params), 1000), "buttons": buttons,
		"untrusted_seen": false, "urgent": false}
	_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
		Tenant: tenant, Kind: kind, Addr: "admins", TargetKind: "chat", TargetID: Chat{Kind: "direct", ExternalID: ch.ExternalID}.ID(),
		ExecKind: "channel", ExecName: ch.Channel, Recipient: ch.Principal, DataOrigin: "code", Task: task,
		Body: map[string]any{"form": "card", "card": card},
	})
	return err
}

// CardFeedbackRequest is p1-card-feedback-request.
type CardFeedbackRequest struct {
	ID    string `json:"id"`
	Nonce string `json:"nonce"`
	Value string `json:"value"`
}

// CardFeedback is P1 card.feedback: the person a notice of the manager went to answers "Useful"
// or "Not needed" once; the answer goes only into the holder report (MGR-50, MGR-52).
func (c *Core) CardFeedback(ctx context.Context, w *Worker, ob *OnBehalf, req CardFeedbackRequest) (map[string]any, error) {
	if ob == nil || w.Kind != "channel" {
		return nil, refuse("forbidden", "card.feedback comes from a channel on behalf of a person")
	}
	if req.Value != "useful" && req.Value != "not_needed" {
		return nil, refuse("input_schema_invalid", "value")
	}
	var out map[string]any
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		principal, err := principalByIdentity(ctx, tx, w.Tenant, ob.Identity)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("forbidden", "the person is not a subject of the tenant")
		}
		if err != nil {
			return err
		}
		var recipient, nonce string
		var value *string
		err = tx.QueryRow(ctx, `SELECT recipient, nonce, value FROM manager_feedback WHERE id = $1 AND tenant = $2 FOR UPDATE`, req.ID, w.Tenant).Scan(&recipient, &nonce, &value)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such notice")
		}
		if err != nil {
			return err
		}
		if recipient != principal || nonce != req.Nonce {
			return refuse("forbidden", "the notice went to someone else")
		}
		out = map[string]any{"ok": true, "id": req.ID, "value": req.Value}
		if value != nil {
			out["value"] = *value
			return nil
		}
		_, err = tx.Exec(ctx, `UPDATE manager_feedback SET value = $2, answered_at = $3 WHERE id = $1`, req.ID, req.Value, now)
		return err
	})
	return out, err
}

// postmortemDayEUR is postmortem_max_eur.day of a farm_manager block of the charter.
func postmortemDayEUR(fm map[string]any) float64 {
	if v, ok := mnum(mmap(fm, "postmortem_max_eur"), "day"); ok && v >= 0 {
		return v
	}
	return defaultPostmortemDayEUR
}

// enqueuePostmortem enqueues the analysis of an incident that moved to resolved, in the
// transaction of the move: one task by the key system:postmortem:<incident>, so a reopened and
// again resolved incident gets none. Code builds the table; the model writes a paragraph only for
// sev1, when code found link candidates, and not twice for one class and object in 7 days; with
// incident_review off there is none and the report says "reading off". The task is paid from the
// manager line outside the sev1 reserve within the daily ceiling, and waits for the next day
// without money (MGR-53).
func (c *Core) enqueuePostmortem(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, r *incidentRow) error {
	role := managerRole(cat)
	fm := managerCharter(cat)
	tv := cat.Tenants[r.Tenant]
	if !claimsV03() || tv == nil || !managerEnabled(cat, r.Tenant) || fm["postmortem"] == false || role == "" || !slices.Contains(cat.Roles[role].Claims, postmortemType) {
		return nil
	}
	if _, ok := cat.TaskTypes[postmortemType]; !ok {
		return nil
	}
	links, err := linkCandidates(ctx, tx, r.Tenant, r.ID)
	if err != nil {
		return err
	}
	readingOff := !incidentReviewOn(tv)
	paragraph := !readingOff && (mstr(r.Record, "severity") == "sev1" || len(links) > 0)
	if paragraph {
		var recent bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks t JOIN incidents i ON i.id = t.record->>'postmortem'
			WHERE t.tenant = $1 AND t.type = $2 AND (t.record->>'paragraph')::boolean AND t.created_at > $3 AND i.class = $4 AND i.subject_kind = $5 AND i.subject_id = $6)`,
			r.Tenant, postmortemType, now.Add(-paragraphEvery), r.Class, r.SubjectKind, r.SubjectID).Scan(&recent); err != nil {
			return err
		}
		paragraph = !recent
	}
	table, err := postmortemTable(ctx, tx, r)
	if err != nil {
		return err
	}
	// The input has the table only: no link states, no one for or against (MGR-54).
	input := map[string]any{"view": "postmortem/v1", "table": table, "paragraph": paragraph}
	body := map[string]any{"type": postmortemType, "goal": "Analyse the resolved incident from the table.", "params": map[string]any{"input": input}}
	res, err := c.enqueue(ctx, tx, now, cat, enqueueInput{Tenant: r.Tenant, Type: postmortemType, Body: body,
		IdempotencyKey: "system:postmortem:" + r.ID, Initiator: "system", EnqueuedBy: "system:postmortem", Source: seatSource, SourceID: r.ID, Priority: 5})
	if err != nil {
		return err
	}
	if res.Rejected != nil || res.Existing {
		return nil
	}
	ceiling := money.FromEUR(postmortemDayEUR(fm))
	day := tenantDay(tv, now)
	var spent int64
	if err := tx.QueryRow(ctx, `SELECT coalesce(sum(s.eur_micros + coalesce(s.correction_eur_micros, 0)), 0) FROM spend s JOIN tasks t ON t.id = s.task
		WHERE s.tenant = $1 AND s.line = 'manager' AND s.unit <> 'task' AND s.time >= $2 AND t.type = $3`, r.Tenant, day, postmortemType).Scan(&spent); err != nil {
		return err
	}
	extra := map[string]any{"seat": managerSeatID, "executor_role": role, "postmortem": r.ID, "paragraph": paragraph, "reading_off": readingOff}
	var runAt any
	if left := ceiling - spent; left > 0 {
		extra["tree_budget_micros"] = left
	} else {
		extra["tree_budget_micros"] = ceiling
		runAt = day.AddDate(0, 0, 1)
	}
	raw, err := json.Marshal(extra)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET record = record || $2::jsonb, run_at = coalesce($3, run_at) WHERE id = $1`, res.TaskID, raw, runAt)
	return err
}

// linkCandidates are the attribution candidates code found for an incident (GOV-13); only
// people with governance.attribute see them (MGR-54).
func linkCandidates(ctx context.Context, tx pgx.Tx, tenant, incident string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id FROM attributions WHERE tenant = $1 AND incident = $2 AND state <> 'canceled' ORDER BY id`, tenant, incident)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// postmortemTable is the code table of an analysis: the time, the alarms with their numbers,
// the operations and the involved parties; a person appears only as "person" (MGR-53).
func postmortemTable(ctx context.Context, tx pgx.Tx, r *incidentRow) (map[string]any, error) {
	rows, err := tx.Query(ctx, `SELECT v.at, v.vals FROM alarm_values v JOIN incidents i ON v.episode = ANY(i.episodes) WHERE i.id = $1 ORDER BY v.at LIMIT 20`, r.ID)
	if err != nil {
		return nil, err
	}
	alarms := []any{}
	for rows.Next() {
		var at time.Time
		var vals map[string]any
		if err := rows.Scan(&at, &vals); err != nil {
			rows.Close()
			return nil, err
		}
		alarms = append(alarms, map[string]any{"at": fmtTime(at), "values": vals})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ops := []any{}
	list, _ := r.Record["ops"].([]any)
	for _, o := range list {
		om := asMap(o)
		by := "person"
		if bm, ok := om["by"].(map[string]any); ok && bm["kind"] == "seat" {
			by = "seat"
		}
		ops = append(ops, map[string]any{"at": om["at"], "op": om["op"], "by": by})
	}
	implicated, _ := r.Record["implicated"].([]any)
	if implicated == nil {
		implicated = []any{}
	}
	resolved := ""
	if r.ResolvedAt != nil {
		resolved = fmtTime(*r.ResolvedAt)
	}
	return map[string]any{"incident": r.ID, "class": r.Class, "severity": mstr(r.Record, "severity"), "opened": fmtTime(r.OpenedAt), "resolved": resolved,
		"alarms": alarms, "operations": ops, "implicated": implicated}, nil
}

// cancelPostmortem cancels the analysis of an incident people declared false (MGR-53).
func (c *Core) cancelPostmortem(ctx context.Context, tx pgx.Tx, now time.Time, tenant, incident string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM tasks WHERE tenant = $1 AND type = $2 AND record->>'postmortem' = $3 AND state IN ('submitted', 'working') FOR UPDATE`,
		tenant, postmortemType, incident)
	if err != nil {
		return err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(list) == 0 {
		return err
	}
	return c.cancelTaskIDs(ctx, tx, now, tenant, list, "incident_dismissed")
}

var reNumber = regexp.MustCompile(`\d+(?:\.\d+)?`)

// paragraphChecked says whether every number of a model paragraph is a number of the table
// (MGR-53).
func paragraphChecked(paragraph string, table map[string]any) bool {
	raw, err := json.Marshal(table)
	if err != nil {
		return false
	}
	known := map[string]bool{}
	for _, n := range reNumber.FindAllString(string(raw), -1) {
		known[n] = true
	}
	for _, n := range reNumber.FindAllString(paragraph, -1) {
		if !known[n] {
			return false
		}
	}
	return true
}

// postmortemDone sends the report of a completed analysis to the people of the tenant with
// incident.manage, within the per-person limit of the day (the excess waits for the next day):
// the table, the model paragraph marked as untrusted text with its sources when code allowed one
// and its numbers are the table's, "reading off" when the purpose is off, and the link
// candidates only for people with governance.attribute. The farm operators get the numbers
// only (MGR-53, MGR-54).
func (c *Core) postmortemDone(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task string, rec []byte, body map[string]any, result map[string]any) error {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil
	}
	var r struct {
		Incident   string `json:"postmortem"`
		Paragraph  bool   `json:"paragraph"`
		ReadingOff bool   `json:"reading_off"`
	}
	if err := json.Unmarshal(rec, &r); err != nil || r.Incident == "" {
		return err
	}
	table := mmap(mmap(mmap(body, "params"), "input"), "table")
	ops, _ := table["operations"].([]any)
	alarms, _ := table["alarms"].([]any)
	implicated, _ := table["implicated"].([]any)
	params := map[string]any{"incident": r.Incident, "class": table["class"], "severity": table["severity"], "opened": table["opened"], "resolved": table["resolved"],
		"alarms": len(alarms), "ops": len(ops), "implicated": len(implicated), "paragraph": "", "links": ""}
	switch p := mstr(result, "paragraph"); {
	case r.ReadingOff:
		params["paragraph"] = c.texts.Render("en", "notice.postmortem.reading_off", nil)
	case r.Paragraph && p != "" && paragraphChecked(p, table):
		params["paragraph"] = c.texts.Render("en", "notice.postmortem.paragraph", map[string]any{
			"text": cut(p, 600), "sources": cut(strings.Join(mstrs(result, "sources"), ", "), 200)})
	}
	links, err := linkCandidates(ctx, tx, tenant, r.Incident)
	if err != nil {
		return err
	}
	chats, err := c.adminChats(ctx, tx, now, tv, "", "incident.manage")
	if err != nil {
		return err
	}
	limit := defaultCardsPerPersonDay
	if v, ok := mnum(mmap(mmap(tv.Raw, "spec"), "attention"), "noncritical_per_day"); ok && v >= 0 {
		limit = int(v)
	}
	day := tenantDay(tv, now)
	seen := map[string]bool{}
	for _, ch := range chats {
		p := map[string]any{}
		for k, v := range params {
			p[k] = v
		}
		if len(links) > 0 {
			pr, err := loadPrincipal(ctx, tx, now, tenant, ch.Principal)
			if err != nil {
				return err
			}
			if hasCap(tv, groupClosure(tv, pr.Groups), "governance.attribute") {
				p["links"] = c.texts.Render("en", "notice.postmortem.links", map[string]any{"links": strings.Join(links, ", ")})
			}
		}
		first := !seen[ch.Principal]
		if first {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM manager_feedback WHERE tenant = $1 AND recipient = $2 AND kind = $3 AND created_at >= $4`,
				tenant, ch.Principal, postmortemReport, day).Scan(&n); err != nil {
				return err
			}
			seen[ch.Principal] = true
			if n >= limit {
				// The excess waits for the next day.
				if err := c.feedbackCard(ctx, tx, now, tenant, postmortemReport, task, ch, "notice.postmortem_report", p, true); err != nil {
					return err
				}
				if _, err := tx.Exec(ctx, `UPDATE deliveries SET run_at = $2 WHERE id = (SELECT id FROM deliveries WHERE task = $1 AND kind = $3 AND recipient = $4
					ORDER BY created_at DESC, id DESC LIMIT 1)`, task, day.AddDate(0, 0, 1), postmortemReport, ch.Principal); err != nil {
					return err
				}
				continue
			}
		}
		if err := c.feedbackCard(ctx, tx, now, tenant, postmortemReport, task, ch, "notice.postmortem_report", p, first); err != nil {
			return err
		}
	}
	return c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, postmortemReport, "notice.postmortem_report_farm",
		map[string]any{"incident": r.Incident, "alarms": len(alarms), "ops": len(ops), "implicated": len(implicated)}, "admin:governance")
}

// managerProposer says whether a completed task of the seat may carry proposals: from v0.3 the
// daily review and the analysis (MGR-51).
func managerProposer(typ string) bool {
	return claimsV03() && (typ == reviewType || typ == postmortemType)
}

// managerProposalLimited applies the manager's 2 proposals per tenant per month (MGR-51).
func managerProposalLimited(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string) (bool, error) {
	day := tenantDay(cat.Tenants[tenant], now)
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM proposals WHERE tenant = $1 AND record->'origin'->>'seat' = $2 AND created_at >= $3`,
		tenant, managerSeatID, time.Date(day.Year(), day.Month(), 1, 0, 0, 0, 0, day.Location())).Scan(&n)
	return n >= managerProposalsMonth, err
}

// managerProposalInfo tells the farm operators, for information only, of a change the manager
// may not propose (MGR-16, PRP-3, MGR-51): codes only.
func (c *Core) managerProposalInfo(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, body map[string]any) error {
	category := mstr(body, "category")
	if !validCategory(category) {
		category = "-"
	}
	code := mstr(mmap(body, "problem"), "code")
	if !problemCodeOK(code) {
		code = "-"
	}
	return c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, "manager_proposal_info", "notice.manager_proposal_info",
		withTenant(map[string]any{"category": category, "problem": code}, tenant), "admin:governance")
}

// holderNumbers are the numbers of a week of the seat holder, without tenant names (MGR-50).
type holderNumbers struct {
	Preventable, Notices, NoticesUseful, Asked, Approved, Done, Failed, Analyses, AnalysesUseful, Sev1Uses int
	SpentMicros, LineMicros                                                                                int64
	Answered, DontKnow                                                                                     int
}

// holderWeek counts the week before now over every tenant.
func holderWeek(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog) (holderNumbers, error) {
	from := now.Add(-7 * 24 * time.Hour)
	var n holderNumbers
	err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM incidents WHERE opened_at >= $1 AND record->>'manager_preventable' = 'true'),
		(SELECT count(*) FROM manager_feedback WHERE kind = $2 AND created_at >= $1),
		(SELECT count(*) FROM manager_feedback WHERE kind = $2 AND created_at >= $1 AND value = 'useful'),
		(SELECT count(*) FROM takeovers WHERE created_at >= $1 AND state <> 'staged'),
		(SELECT count(*) FROM takeovers WHERE created_at >= $1 AND state = 'approved'),
		(SELECT count(*) FROM takeovers k JOIN tasks t ON t.id = k.new_task WHERE k.created_at >= $1 AND t.state = 'completed'),
		(SELECT count(*) FROM takeovers k JOIN tasks t ON t.id = k.new_task WHERE k.created_at >= $1 AND t.state IN ('failed', 'canceled')),
		(SELECT count(*) FROM tasks WHERE type = $3 AND created_at >= $1),
		(SELECT count(*) FROM manager_feedback WHERE kind = $4 AND created_at >= $1 AND value = 'useful'),
		(SELECT count(DISTINCT s.task) FROM spend s JOIN tasks t ON t.id = s.task WHERE s.line = 'manager' AND s.time >= $1 AND t.record->>'severity' = 'sev1'),
		(SELECT coalesce(sum(eur_micros + coalesce(correction_eur_micros, 0)), 0) FROM spend WHERE line = 'manager' AND unit <> 'task' AND time >= $1),
		(SELECT count(*) FROM attributions WHERE updated_at >= $1 AND state IN ('confirmed', 'rejected', 'unknown')),
		(SELECT count(*) FROM attributions WHERE updated_at >= $1 AND state = 'unknown')`,
		from, attentionNotice, postmortemType, postmortemReport).Scan(&n.Preventable, &n.Notices, &n.NoticesUseful, &n.Asked, &n.Approved, &n.Done, &n.Failed,
		&n.Analyses, &n.AnalysesUseful, &n.Sev1Uses, &n.SpentMicros, &n.Answered, &n.DontKnow)
	for _, t := range slices.Sorted(mapKeys(cat.Tenants)) {
		if lb := cat.Tenants[t].Spec.Budgets.Lines["manager"]; managerEnabled(cat, t) && lb.MonthEUR != nil {
			n.LineMicros += money.FromEUR(*lb.MonthEUR)
		}
	}
	return n, err
}

// share is a whole percent of part in all, or "-" without any.
func share(part, all int) string {
	if all == 0 {
		return "-"
	}
	return fmt.Sprintf("%d%%", part*100/all)
}

// holderReportWeek sends holder_report to the farm operators once a week, numbers only (MGR-50).
func (c *Core) holderReportWeek(ctx context.Context, cat *Catalog) error {
	if !claimsV03() || managerRole(cat) == "" {
		return nil
	}
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		y, w := now.UTC().ISOWeek()
		first, err := markOnce(ctx, tx, now, farmTenant, fmt.Sprintf("holder_report:%d-%02d", y, w), 8*24*time.Hour)
		if err != nil || !first {
			return err
		}
		n, err := holderWeek(ctx, tx, now, cat)
		if err != nil {
			return err
		}
		return c.noticeAdmins(ctx, tx, now, cat, []string{farmTenant}, holderReport, "notice.holder_report", map[string]any{
			"preventable": n.Preventable, "notices": n.Notices, "notices_useful": share(n.NoticesUseful, n.Notices),
			"takeovers": n.Asked, "approved": n.Approved, "done": n.Done, "failed": n.Failed,
			"analyses": n.Analyses, "analyses_useful": share(n.AnalysesUseful, n.Analyses),
			"spent_cents": money.CentsUp(n.SpentMicros), "line_cents": money.CentsUp(n.LineMicros), "sev1": n.Sev1Uses,
			"dont_know": share(n.DontKnow, n.Answered)}, "admin:governance")
	})
}

// recurringClasses are the incident classes that opened in two tenants or more in [since,
// until): code finds problems across tenants for the farm operators only (MGR-51).
func recurringClasses(ctx context.Context, tx pgx.Tx, since, until time.Time) (string, error) {
	rows, err := tx.Query(ctx, `SELECT class FROM incidents WHERE opened_at >= $1 AND opened_at < $2 AND tenant <> $3 GROUP BY class
		HAVING count(DISTINCT tenant) >= 2 ORDER BY class LIMIT 20`, since, until, farmTenant)
	if err != nil {
		return "", err
	}
	list, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil || len(list) == 0 {
		return "-", err
	}
	return strings.Join(list, ", "), nil
}
