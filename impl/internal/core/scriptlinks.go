// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Learning from incidents on scripts (docs/coordination.md, v0.3). The run card marks the
// targets the typed text does not name (TRN-10); code links an incident to a script run whose
// targets fixed at "Run" it touches, and a person other than the one who pressed "Run" decides
// the link on script_link_card (TRN-7); three confirmed links to the work of one role within 90
// days set a role hold with no_action (HLD-9).

const (
	scriptLinkCard = "script_link_card"
	// defaultUnnamedTargetsMax is scripts.unnamed_targets_max by default (TRN-10).
	defaultUnnamedTargetsMax = 5
	// roleIncidentsRule is the auto-stop of HLD-9 in spec/model/holds.yaml.
	roleIncidentsRule = "role_incidents_three"
)

// unnamedTargets are the targets of one step that the typed text does not name (TRN-10):
// machines and resources named neither by id nor by name, and a machine selector that the
// text does not name or that is wider than scripts.unnamed_targets_max.
type unnamedTargets struct {
	Resources []string       `json:"resources,omitempty"`
	Groups    []unnamedGroup `json:"groups,omitempty"`
}

type unnamedGroup struct {
	Selector string `json:"selector"`
	Machines int    `json:"machines"`
	NotNamed bool   `json:"not_named,omitempty"`
	Wide     bool   `json:"wide,omitempty"`
}

// textNames says whether text names name as a whole word, ignoring case.
func textNames(text, name string) bool {
	if name == "" {
		return false
	}
	text, name = strings.ToLower(text), strings.ToLower(name)
	part := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' }
	for from := 0; ; {
		i := strings.Index(text[from:], name)
		if i < 0 {
			return false
		}
		i += from
		end := i + len(name)
		before, after := rune(' '), rune(' ')
		if i > 0 {
			before = []rune(text[:i])[len([]rune(text[:i]))-1]
		}
		if end < len(text) {
			after = []rune(text[end:])[0]
		}
		if !part(before) && !part(after) {
			return true
		}
		from = i + 1
	}
}

// resourceTitles are the names of the catalog resources of the tenant by id.
func resourceTitles(cat *Catalog, tenant string) map[string]string {
	out := map[string]string{}
	spec, _ := tenantCatalog(cat, tenant)
	res, _ := spec["resources"].([]any)
	for _, r := range res {
		m, _ := r.(map[string]any)
		id, _ := m["id"].(string)
		title, _ := m["title"].(string)
		if id != "" && title != "" {
			out[id] = title
		}
	}
	return out
}

// stepTargets are the catalog machines and resources a step acts on: the list of a fanout, or
// the catalog values among the parameters and arguments of a single step.
func stepTargets(resources []string, s scriptStep, fanout []string) []string {
	if s.Kind == "fanout" {
		return fanout
	}
	var out []string
	for _, k := range sortedKeys(s.values()) {
		if v, ok := s.values()[k].(string); ok && slices.Contains(resources, v) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// runTargets are the targets of all steps of a run, sorted (TRN-7).
func runTargets(resources []string, steps []scriptStep, rec scriptRec) []string {
	out := []string{}
	for _, s := range steps {
		for _, t := range stepTargets(resources, s, rec.fanoutList(s.ID)) {
			if !slices.Contains(out, t) {
				out = append(out, t)
			}
		}
	}
	slices.Sort(out)
	return out
}

// unnamedOf marks the targets of a step that the assignment does not name (TRN-10); nil when
// it names them all.
func unnamedOf(cat *Catalog, tv *TenantView, resources []string, s scriptStep, fanout []string, text string) *unnamedTargets {
	if s.Kind == "confirm" {
		return nil
	}
	limit := defaultUnnamedTargetsMax
	if v := tv.Spec.Scripts.UnnamedTargetsMax; v > 0 {
		limit = v
	}
	u := &unnamedTargets{}
	if sel, ok := s.Items["selector"].(string); ok && s.Kind == "fanout" {
		g := unnamedGroup{Selector: sel, Machines: len(fanout), NotNamed: !textNames(text, sel), Wide: len(fanout) > limit}
		if g.NotNamed || g.Wide {
			u.Groups = append(u.Groups, g)
		}
	} else {
		titles := resourceTitles(cat, tv.Name)
		for _, r := range stepTargets(resources, s, fanout) {
			if !textNames(text, r) && !textNames(text, titles[r]) {
				u.Resources = append(u.Resources, r)
			}
		}
	}
	if len(u.Resources) == 0 && len(u.Groups) == 0 {
		return nil
	}
	return u
}

// scriptActs says whether a script changes the world: an action step, a fanout with an action,
// or a step whose bot may control services or run monitoring scripts by the current catalog
// (HLD-9).
func (c *Core) scriptActs(cat *Catalog, steps []scriptStep) bool {
	return slices.ContainsFunc(steps, func(s scriptStep) bool {
		return s.hasAction() || slices.Contains(c.stepMarks(cat, s, ""), "may_control_services")
	})
}

// createScriptLinks creates the candidates of the rule script_target (TRN-7): an incident of a
// change_sensitive class about a catalog resource among the targets a run fixed at "Run", or
// about an object that depends on one of them by one step in the catalog (catalogdeps.go, the
// step of dependency_1), opened from the start of the run until 14 days after it ended. On a
// core that claims v0.3 only runs
// started at or after the tenant's governance_v03_since are linked, none before it is known
// (GOV-36).
func (c *Core) createScriptLinks(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	var from *time.Time
	if claimsV03() {
		since, _, err := v03State(ctx, tx, tv.Name)
		if err != nil || since == nil {
			return err
		}
		from = since
	}
	rows, err := tx.Query(ctx, `SELECT i.id, i.class, i.subject_kind, i.subject_id, r.id, coalesce(r.targets, '{}') FROM incidents i JOIN script_runs r ON r.tenant = i.tenant
		AND r.started_at IS NOT NULL AND i.opened_at >= r.started_at AND (r.ended_at IS NULL OR i.opened_at <= r.ended_at + interval '14 days')
		AND cardinality(r.targets) > 0 AND ($3::timestamptz IS NULL OR r.started_at >= $3)
		WHERE i.tenant = $1 AND i.state <> 'dismissed' AND i.opened_at >= $2
		AND NOT EXISTS (SELECT 1 FROM attributions a WHERE a.incident = i.id AND a.script_run = r.id)
		ORDER BY i.opened_at, i.id, r.id`, tv.Name, now.Add(-attributionLookback), from)
	if err != nil {
		return err
	}
	type link struct {
		incident, class, kind, subject, run string
		targets                             []string
	}
	var links []link
	for rows.Next() {
		var l link
		if err := rows.Scan(&l.incident, &l.class, &l.kind, &l.subject, &l.run, &l.targets); err != nil {
			rows.Close()
			return err
		}
		if classByName(cat, l.class)["change_sensitive"] == true {
			links = append(links, l)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	deps := map[string][]subjectRef{}
	for _, l := range links {
		touched := l.kind == "resource" && slices.Contains(l.targets, l.subject)
		if !touched {
			d, ok := deps[l.incident]
			if !ok {
				if d, err = subjectDeps(ctx, tx, cat, tv.Name, l.kind, l.subject); err != nil {
					return err
				}
				deps[l.incident] = d
			}
			// A machine or a resource the run named is a resource of the catalog.
			touched = slices.ContainsFunc(d, func(r subjectRef) bool { return r.Kind == "resource" && slices.Contains(l.targets, r.ID) })
		}
		if !touched {
			continue
		}
		if err := c.createScriptLink(ctx, tx, now, tv, l.incident, l.run); err != nil {
			return err
		}
	}
	return nil
}

// createScriptLink writes a candidate of kind script_run once per incident and run and sends
// its card.
func (c *Core) createScriptLink(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, incident, run string) error {
	id := ids.New(ids.Attribution)
	rec := map[string]any{"id": id, "tenant": tv.Name, "kind": "script_run", "incident": incident, "script_run": run, "rule": "script_target",
		"created_by": "system", "created_at": fmtTime(now), "state": "candidate", "answer": nil, "reminders": 0, "cancel_reason": nil}
	if err := c.spec.Validate("urn:botaru:schema:attribution:2", rec); err != nil {
		return fmt.Errorf("script link record: %w", err)
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO attributions (id, tenant, incident, kind, script_run, state, record, nonce, presented_at, created_at, updated_at)
		VALUES ($1, $2, $3, 'script_run', $4, 'candidate', $5, $6, $7, $7, $7) ON CONFLICT DO NOTHING`, id, tv.Name, incident, run, raw, ids.Secret(16), now)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: "system", Via: "core"}, Command: "attribution.create",
		Target: &Target{Kind: "attribution", ID: id}, OK: true, After: map[string]any{"incident": incident, "script_run": run, "rule": "script_target", "state": "candidate"},
	}); err != nil {
		return err
	}
	return c.planScriptLinkCards(ctx, tx, now, tv, id, run, nil)
}

// runMeta is what a script link reads of its run from the columns, which retention leaves
// when it purges the record: the initiator, the role that wrote the draft, the data mark, the
// state, the times and the targets fixed at "Run".
type runMeta struct {
	ID, Initiator, Role, Mark, State string
	CreatedAt                        time.Time
	StartedAt, EndedAt               *time.Time
	Targets                          []string
}

func loadRunMeta(ctx context.Context, tx pgx.Tx, id string) (*runMeta, error) {
	m := &runMeta{ID: id}
	err := tx.QueryRow(ctx, `SELECT initiator, coalesce(role, record->>'role', ''), coalesce(data_mark, record->>'data_mark', 'restricted'), state,
		created_at, started_at, ended_at, coalesce(targets, '{}') FROM script_runs WHERE id = $1`, id).
		Scan(&m.Initiator, &m.Role, &m.Mark, &m.State, &m.CreatedAt, &m.StartedAt, &m.EndedAt, &m.Targets)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return m, err
}

// scriptLinkPeople are the people who decide a script link (TRN-7): those with
// governance.attribute whose data ceiling covers the run's mark, but the initiator, who pressed
// "Run"; when no such other person exists, the initiator if they hold governance.attribute,
// and self is true, so a link is never left without a decider.
func (c *Core) scriptLinkPeople(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, m *runMeta) (people []string, self bool, err error) {
	all, err := peopleWithCap(ctx, tx, now, tv, "governance.attribute")
	if err != nil {
		return nil, false, err
	}
	others := slices.DeleteFunc(slices.Clone(all), func(p string) bool { return p == m.Initiator })
	if others, err = c.withinCeiling(ctx, tx, now, tv, others, m.Mark); err != nil {
		return nil, false, err
	}
	if len(others) > 0 {
		return others, false, nil
	}
	if slices.Contains(all, m.Initiator) {
		return []string{m.Initiator}, true, nil
	}
	return nil, false, nil
}

// planScriptLinkCards plans script_link_card to the people who decide it.
func (c *Core) planScriptLinkCards(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, id, run string, extra map[string]any) error {
	m, err := loadRunMeta(ctx, tx, run)
	if err != nil || m == nil {
		return err
	}
	people, _, err := c.scriptLinkPeople(ctx, tx, now, tv, m)
	if err != nil {
		return err
	}
	_, err = c.planGovCardsTo(ctx, tx, now, tv, "attribution", id, scriptLinkCard, people, extra)
	return err
}

// scriptLinkCardFields draws script_link_card: the incident, the run with its targets and
// initiator, whether the initiator opened the step list, the self-assessment mark and the
// buttons "Yes", "No" and "Don't know" while the answer may still change (TRN-7).
func (c *Core) scriptLinkCardFields(ctx context.Context, tx pgx.Tx, cat *Catalog, a *attributionRow) (map[string]any, error) {
	var class, kind, subject string
	var opened time.Time
	if err := tx.QueryRow(ctx, `SELECT class, subject_kind, subject_id, opened_at FROM incidents WHERE id = $1`, a.Incident).Scan(&class, &kind, &subject, &opened); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	ctxt := map[string]any{"incident": a.Incident, "class": class, "subject": kind + ":" + subject, "opened_at": fmtTime(opened), "script_run": a.ScriptRun, "rule": a.Rec["rule"]}
	m, err := loadRunMeta(ctx, tx, a.ScriptRun)
	if err != nil {
		return nil, err
	}
	if m != nil {
		ctxt["initiator"], ctxt["role"], ctxt["run_state"], ctxt["targets"] = m.Initiator, m.Role, m.State, m.Targets
		// Whether the initiator opened the step list ("Full plan") before Run.
		var viewed bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM card_views WHERE subject_kind = 'script' AND subject_id = $1 AND principal = $2
			AND opened_at >= $3 AND ($4::timestamptz IS NULL OR opened_at <= $4))`, m.ID, m.Initiator, m.CreatedAt, m.StartedAt).Scan(&viewed); err != nil {
			return nil, err
		}
		ctxt["initiator_opened_steps"] = viewed
		if tv := cat.Tenants[a.Tenant]; tv != nil {
			var now time.Time
			if err := tx.QueryRow(ctx, `SELECT botaru_now()`).Scan(&now); err != nil {
				return nil, err
			}
			_, self, err := c.scriptLinkPeople(ctx, tx, now, tv, m)
			if err != nil {
				return nil, err
			}
			if self || a.Rec["self_assessment"] == true {
				ctxt["self_assessment"] = true
			}
		}
		if m.StartedAt != nil {
			ctxt["started_at"] = fmtTime(*m.StartedAt)
		}
		if m.EndedAt != nil {
			ctxt["ended_at"] = fmtTime(*m.EndedAt)
		}
	}
	if ans := mmapAny(a.Rec["answer"]); ans != nil {
		ctxt["answer"] = ans["decision"]
	}
	card := map[string]any{"card": scriptLinkCard, "subject_kind": "attribution", "id": a.ID, "nonce": a.Nonce,
		"title": cut(fmt.Sprintf("Did the script run cause the incident? %s on %s:%s", class, kind, subject), 200), "context": ctxt,
		"requested_by": map[string]any{"initiator": "system", "source": "script_step"}, "initiator_display": "the governance loop", "state": a.State,
		"footer": map[string]any{"why_you": "governance.attribute", "on_silence": "not decided; it does not count toward the role hold"}}
	if a.PresentedAt != nil {
		until := fmtTime(a.PresentedAt.Add(attributionTTL))
		card["expires_at"] = until
		card["footer"].(map[string]any)["until"] = until
	}
	buttons := []any{}
	if slices.Contains([]string{"candidate", "unknown", "rejected"}, a.State) && a.Nonce != "" {
		for _, code := range []string{"yes", "no", "unknown", "details"} {
			buttons = append(buttons, map[string]any{"code": code, "subject_kind": "attribution", "id": a.ID, "nonce": a.Nonce})
		}
	}
	card["buttons"] = buttons
	return card, nil
}

// scriptLinkDecider checks the person deciding a script link (TRN-7): not the one who pressed
// "Run" unless no other person may decide it; then the answer is a self-assessment.
func (c *Core) scriptLinkDecider(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, a *attributionRow, principal string) (self, ok bool, err error) {
	m, err := loadRunMeta(ctx, tx, a.ScriptRun)
	if err != nil || m == nil || principal != m.Initiator {
		return false, err == nil, err
	}
	_, self, err = c.scriptLinkPeople(ctx, tx, now, tv, m)
	return self, self, err
}

// scriptLinkWindowOpen: the answer on a script link may change while its run runs and for 14
// days after it ended.
func scriptLinkWindowOpen(ctx context.Context, tx pgx.Tx, now time.Time, run string) (bool, error) {
	var ended *time.Time
	if err := tx.QueryRow(ctx, `SELECT ended_at FROM script_runs WHERE id = $1`, run).Scan(&ended); errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return ended == nil || now.Before(ended.Add(defaultAttributionWindow)), nil
}

// roleIncidentLimits are the threshold and the window of HLD-9 from spec/model/holds.yaml; from
// v0.3 the tenant catalog's role_incident_hold may only make them stricter, fewer links or a
// longer window, so a looser value never applies even if one passed the schema.
func (c *Core) roleIncidentLimits(cat *Catalog, tenant string) (int, time.Duration, error) {
	links, window := 3, 90*24*time.Hour
	reg, err := c.spec.Registry("spec/model/holds.yaml")
	if err != nil {
		return 0, 0, err
	}
	if e, ok := reg.Entry(roleIncidentsRule); ok {
		th := mmapAny(e["threshold"])
		if n, ok := toFloat(th["links"]); ok && n >= 1 {
			links = int(n)
		}
		if d, ok := parseDuration(asString(th["window"])); ok {
			window = d
		}
	}
	if claimsV03() && cat != nil {
		spec, _ := tenantCatalog(cat, tenant)
		h := mmap(spec, "role_incident_hold")
		if n, ok := mnum(h, "links"); ok && n >= 1 {
			links = min(links, int(n))
		}
		if d, ok := parseDuration(mstr(h, "window")); ok {
			window = max(window, d)
		}
	}
	return links, window, nil
}

// linkRole is the role whose work a confirmed link names: the role whose bot wrote the
// proposal of a change, or the role that wrote the draft of a script run.
func linkRole(ctx context.Context, tx pgx.Tx, a *attributionRow) (string, error) {
	var role string
	var err error
	switch a.Kind {
	case "change":
		err = tx.QueryRow(ctx, `SELECT role FROM proposals WHERE id = $1`, a.Proposal).Scan(&role)
	case "script_run":
		err = tx.QueryRow(ctx, `SELECT coalesce(role, record->>'role', '') FROM script_runs WHERE id = $1`, a.ScriptRun).Scan(&role)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return role, err
}

// roleIncidents is the auto-stop of HLD-9 after a link is confirmed: per tenant and role, not
// per bot, the confirmed links of kinds change and script_run whose incidents opened within the
// window are counted; at the threshold the role gets a hold with no_action, the reason
// role_incidents, the author contain_auto and no expiry, and the alarm alarm.hold.auto. A link
// that is later disputed or canceled leaves the count but lifts nothing; only a person lifts
// the hold (HLD-2), and links confirmed before that release do not count again.
func (c *Core) roleIncidents(ctx context.Context, tx pgx.Tx, now time.Time, a *attributionRow) error {
	role, err := linkRole(ctx, tx, a)
	if err != nil || role == "" {
		return err
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	threshold, window, err := c.roleIncidentLimits(cat, a.Tenant)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300109, hashtext($1 || '/' || $2))`, a.Tenant, role); err != nil {
		return err
	}
	var active bool
	var released *time.Time
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM holds WHERE tenant = $1 AND scope_kind = 'role' AND scope_tenant = $1 AND scope_name = $2
			AND reason = 'role_incidents' AND state = 'active'),
		(SELECT max(ended_at) FROM holds WHERE tenant = $1 AND scope_kind = 'role' AND scope_tenant = $1 AND scope_name = $2
			AND reason = 'role_incidents' AND state <> 'active')`, a.Tenant, role).Scan(&active, &released); err != nil {
		return err
	}
	if active {
		return nil
	}
	since := time.Time{}
	if released != nil {
		since = *released
	}
	var n int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM attributions a JOIN incidents i ON i.id = a.incident
		LEFT JOIN proposals p ON a.kind = 'change' AND p.id = a.proposal
		LEFT JOIN script_runs r ON a.kind = 'script_run' AND r.id = a.script_run
		WHERE a.tenant = $1 AND a.state = 'confirmed' AND a.kind IN ('change', 'script_run')
		AND coalesce(p.role, r.role, r.record->>'role') = $2 AND i.opened_at > $3 AND (a.record->'answer'->>'at')::timestamptz > $4`,
		a.Tenant, role, now.Add(-window), since).Scan(&n); err != nil {
		return err
	}
	if n < threshold {
		return nil
	}
	actor := HoldActor{Principal: "system", Rank: "contain_auto", Tenant: a.Tenant, Via: "system", Source: roleIncidentsRule}
	if _, _, err := c.insertHold(ctx, tx, now, actor, HoldScope{Kind: "role", Tenant: a.Tenant, Name: role}, []string{"no_action"}, "role_incidents", nil); err != nil {
		return err
	}
	return c.fireAlarm(ctx, tx, now, Alarm{Tenant: a.Tenant, Name: "hold.auto", Subject: AlarmSubject{Kind: "role", ID: role}, Severity: "page",
		Values: map[string]any{"reason": "role_incidents", "links": n}})
}

// scriptDetails is P1 card.details on the run card ("Full plan"): every step with its
// parameters to the same chat; the opening is recorded, so a script link card shows whether
// the initiator opened the step list (TRN-7, APR-11).
func (c *Core) scriptDetails(ctx context.Context, w *Worker, ob OnBehalf, req CardOp) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, err := loadRun(ctx, tx, w.Tenant, req.ID, false)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such script run")
		}
		if err != nil {
			return err
		}
		if r.Nonce != req.Nonce {
			return refuse("forbidden", "the nonce is not the card's")
		}
		press, err := c.checkPress(ctx, tx, now, cat, w, ob, "script", r.ID, []string{scriptConfirmCard})
		if err != nil {
			return err
		}
		// The first opening stays: a script link card asks whether it came before Run.
		if _, err := tx.Exec(ctx, `INSERT INTO card_views (subject_kind, subject_id, principal, opened_at) VALUES ('script', $1, $2, $3)
			ON CONFLICT (subject_kind, subject_id, principal) DO NOTHING`, r.ID, press.principal, now); err != nil {
			return err
		}
		card, err := c.buildScriptCard(ctx, tx, cat, r, scriptConfirmCard)
		if err != nil {
			return err
		}
		fields := mmapAny(card["context"])
		fields["title"], fields["state"], fields["steps_list"] = card["title"], card["state"], mmapAny(card["args"])["steps"]
		out = map[string]any{"template": "card.details.script", "fields": fields, "deliver_to": "same_chat"}
		return nil
	})
	return out, err
}

// scriptLinkStep is the attribution part of the governance loop of a tenant without a
// commission: its script links, their dismissals and reminders (TRN-7, GOV-30).
func (c *Core) scriptLinkStep(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView) error {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300107, hashtext($1))`, tv.Name); err != nil {
		return err
	}
	if err := c.createScriptLinks(ctx, tx, now, cat, tv); err != nil {
		return err
	}
	if err := c.dismissedAttributions(ctx, tx, now, tv.Name, ""); err != nil {
		return err
	}
	return c.remindAttributions(ctx, tx, now, tv)
}
