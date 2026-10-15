// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// Reporters (REP-2, REP-3, REP-6, REP-7, REP-8, REP-9): a role of profile reporter reads the
// status.* views and its inbox and hands in at most a paragraph; the core builds the digest
// table from a snapshot of the views and delivers it to the address of the task, never one the
// reporter names.

const (
	statusTasksName     = "status.tasks"
	statusProposalsName = "status.proposals"
	// maxStatusRows is the most rows a view returns; the rest only count.
	maxStatusRows = 100
	// maxDigestText is the longest model paragraph of a digest (REP-2).
	maxDigestText = 800
)

// proposalViewStates are the proposal states status.proposals shows: recommended and later,
// never an audit sample item (REP-8, DIG-1, DIG-6).
var proposalViewStates = []string{"recommended", "presented", "declined", "noted", "realizing", "realized", "realization_failed", "reverted"}

// audience is whom a reporter task reports to, fixed at enqueue (REP-3, REP-7): the ceiling of
// its mark and the principals whose permissions filter its views. With no principals the views
// follow the groups of system.
type audience struct {
	Ceiling    string   `json:"ceiling,omitempty"`
	Principals []string `json:"principals"`
	Group      bool     `json:"group,omitempty"`
}

// dataClasses are the data classes in their order, lowest first.
func (c *Core) dataClasses() []string {
	reg, err := c.spec.Registry("spec/model/capabilities.yaml")
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range reg.Entries {
		if id, _ := e["id"].(string); e["kind"] == "data_class" {
			out = append(out, id)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, _ := c.markOrder(out[i])
		b, _ := c.markOrder(out[j])
		return a < b
	})
	return out
}

// lowerMark is the lower of two marks; an unknown mark gives way to the other.
func (c *Core) lowerMark(a, b string) string {
	if c.higherMark(a, b) == a {
		if _, ok := c.markOrder(b); ok {
			return b
		}
	}
	return a
}

// channelPrincipals are the active persons of the tenant with an identity in a channel: for a
// group chat whose members the core does not list, every one of them counts as a participant,
// so the intersection of their permissions is never wider than the members' (REP-7).
func (c *Core) channelPrincipals(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, channel string) ([]string, error) {
	chats, err := c.principalChats(ctx, tx, now, tv, "", func([]string) bool { return true })
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ch := range chats {
		if ch.Channel == channel && !slices.Contains(out, ch.Principal) {
			out = append(out, ch.Principal)
		}
	}
	return out, nil
}

// reportAudience computes the audience of a reporter task at enqueue (REP-3, REP-7, REP-9): the
// mark is no higher than the type's class, the ceiling of every target and the highest class
// the initiator holds; the principals are the person asking in a direct chat, the participants
// of a group chat, the members of a group of a schedule's deliver_to.
func (c *Core) reportAudience(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, in enqueueInput, tt manifest.TaskType, groups []string) (*audience, error) {
	a := &audience{Ceiling: tt.DataClass, Principals: []string{}}
	// The highest class the initiator holds (for a schedule, system within the tenant).
	top := ""
	for _, m := range c.dataClasses() {
		if hasData(tv, groups, m) {
			top = m
		}
	}
	a.Ceiling = c.lowerMark(a.Ceiling, top)
	addPrincipals := func(ps []string) {
		for _, p := range ps {
			if !slices.Contains(a.Principals, p) {
				a.Principals = append(a.Principals, p)
			}
		}
	}
	binding := func(b *chatBinding) error {
		if b == nil {
			return nil
		}
		a.Ceiling, a.Group = c.lowerMark(a.Ceiling, b.Ceiling), true
		ps, err := c.channelPrincipals(ctx, tx, now, tv, b.Channel)
		addPrincipals(ps)
		return err
	}
	switch {
	case in.Origin != nil && in.Origin.Chat.Kind == "group":
		b, err := loadBinding(ctx, tx, tv.Name, in.Origin.Channel, in.Origin.Chat.ID())
		if err != nil {
			return nil, err
		}
		if b == nil {
			// An unbound group gets nothing above the lowest class.
			a.Ceiling, a.Group = c.lowerMark(a.Ceiling, c.dataClasses()[0]), true
		}
		if err := binding(b); err != nil {
			return nil, err
		}
		addPrincipals([]string{in.Initiator})
	case in.Origin != nil:
		addPrincipals([]string{in.Initiator})
	case in.Source == "schedule":
		sc := tv.Schedules[in.SourceID]
		for _, x := range sc.DeliverTo {
			addr, _ := x.(string)
			kind, name, _ := strings.Cut(addr, ":")
			switch kind {
			case "chat":
				b, err := bindingByName(ctx, tx, tv.Name, name)
				if err != nil {
					return nil, err
				}
				if err := binding(b); err != nil {
					return nil, err
				}
			case "group":
				ms, err := groupMembers(ctx, tx, now, tv, name)
				if err != nil {
					return nil, err
				}
				addPrincipals(ms)
			case "sink":
				raw := cat.RawGlobals[manifest.Key{Kind: "Sink", Tenant: tv.Name, Name: name}]
				spec, _ := raw["spec"].(map[string]any)
				if ceiling, _ := spec["ceiling"].(string); ceiling != "" {
					a.Ceiling = c.lowerMark(a.Ceiling, ceiling)
				}
			}
		}
	}
	sort.Strings(a.Principals)
	return a, nil
}

// viewFilter is how a view filters for a reporter attempt: the groups of each principal of the
// audience (or of system) for the permission filter, and the mark for the hidden count.
type viewFilter struct {
	tv     *TenantView
	groups [][]string
	mark   string
}

// readable says whether every principal of the audience may read a task of the type and mark
// (REP-7, PRM-9); a row they may not read is left out and not counted.
func (f viewFilter) readable(typ, mark string) bool {
	for _, gs := range f.groups {
		if !hasCap(f.tv, gs, "task.read:"+typ) || mark != "" && !hasData(f.tv, gs, mark) {
			return false
		}
	}
	return true
}

func (c *Core) viewFilterOf(ctx context.Context, tx pgx.Tx, now time.Time, tv *TenantView, rec []byte) (*viewFilter, *audience, error) {
	var r struct {
		DataMark string    `json:"data_mark"`
		Audience *audience `json:"audience"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return nil, nil, err
	}
	a := r.Audience
	if a == nil {
		a = &audience{}
	}
	f := &viewFilter{tv: tv, mark: r.DataMark}
	if len(a.Principals) == 0 {
		f.groups = [][]string{groupClosure(tv, []string{"system-ops"})}
	}
	for _, p := range a.Principals {
		gs, err := principalGroups(ctx, tx, now, tv, p)
		if err != nil {
			return nil, nil, err
		}
		f.groups = append(f.groups, groupClosure(tv, gs))
	}
	return f, a, nil
}

// reportWindow is the start of the window of a reporter task: its params.window, since_last
// being the end of the last completed task of the type from the same source (at most 7 days).
func reportWindow(ctx context.Context, tx pgx.Tx, now time.Time, task, typ string, body, rec []byte) (time.Time, string, error) {
	var b struct {
		Params struct {
			Window string `json:"window"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &b)
	switch b.Params.Window {
	case "1h":
		return now.Add(-time.Hour), "1h", nil
	case "since_last":
		var by string
		var r struct {
			EnqueuedBy string `json:"enqueued_by"`
		}
		_ = json.Unmarshal(rec, &r)
		by = r.EnqueuedBy
		var last *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(updated_at) FROM tasks WHERE type = $1 AND id <> $2 AND state = 'completed' AND record->>'enqueued_by' = $3`,
			typ, task, by).Scan(&last); err != nil {
			return time.Time{}, "", err
		}
		if last != nil && now.Sub(*last) < 7*24*time.Hour {
			return *last, "since_last", nil
		}
		return now.Add(-7 * 24 * time.Hour), "since_last", nil
	}
	return now.Add(-24 * time.Hour), "24h", nil
}

// viewSnapshot is the snapshot of the views of a reporter task: taken once, at the first read
// or at the end of the task, so the model and the digest table see the same numbers (REP-2).
type viewSnapshot struct {
	ID     string           `json:"snapshot_id"`
	Window string           `json:"window"`
	From   string           `json:"from"`
	Tasks  []map[string]any `json:"tasks"`
	// Counts are the visible tasks by type and state: the digest table.
	Counts        []map[string]any `json:"counts"`
	TasksHidden   int              `json:"tasks_hidden"`
	Proposals     []map[string]any `json:"proposals"`
	PropsHidden   int              `json:"proposals_hidden"`
	TasksMore     int              `json:"tasks_more"`
	ProposalsMore int              `json:"proposals_more"`
}

// snapshotOf returns the snapshot of the views of a task, taking it if there is none.
func (c *Core) snapshotOf(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task string) (*viewSnapshot, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT body FROM view_snapshots WHERE task = $1`, task).Scan(&raw)
	if err == nil {
		var s viewSnapshot
		return &s, json.Unmarshal(raw, &s)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	tv := cat.Tenants[tenant]
	var typ string
	var body, rec []byte
	if err := tx.QueryRow(ctx, `SELECT type, body, record FROM tasks WHERE id = $1`, task).Scan(&typ, &body, &rec); err != nil {
		return nil, err
	}
	f, _, err := c.viewFilterOf(ctx, tx, now, tv, rec)
	if err != nil {
		return nil, err
	}
	from, window, err := reportWindow(ctx, tx, now, task, typ, body, rec)
	if err != nil {
		return nil, err
	}
	var b struct {
		Params struct {
			Scope struct {
				Kind string `json:"kind"`
				ID   string `json:"id"`
			} `json:"scope"`
		} `json:"params"`
	}
	_ = json.Unmarshal(body, &b)
	snap := &viewSnapshot{ID: task + ":" + fmtTime(now), Window: window, From: fmtTime(from), Tasks: []map[string]any{}, Counts: []map[string]any{}, Proposals: []map[string]any{}}
	// The tasks of the window: metadata and closed fields only (REP-8).
	root := ""
	switch b.Params.Scope.Kind {
	case "root":
		root = b.Params.Scope.ID
	case "campaign":
		root = "-" // campaigns are not built yet: nothing is in such a scope
	}
	rows, err := tx.Query(ctx, `SELECT id, type, state, coalesce(reason, ''), coalesce(record->>'data_mark', ''), updated_at FROM tasks
		WHERE tenant = $1 AND id <> $2 AND updated_at >= $3 AND ($4 = '' OR record->>'root' = $4) ORDER BY updated_at DESC, id LIMIT 5000`, tenant, task, from, root)
	if err != nil {
		return nil, err
	}
	type trow struct {
		id, typ, state, reason, mark string
		at                           time.Time
	}
	var trows []trow
	for rows.Next() {
		var r trow
		if err := rows.Scan(&r.id, &r.typ, &r.state, &r.reason, &r.mark, &r.at); err != nil {
			rows.Close()
			return nil, err
		}
		trows = append(trows, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	counts := map[[2]string]int{}
	for _, r := range trows {
		if !f.readable(r.typ, r.mark) {
			continue
		}
		if r.mark != "" && !c.markWithin(r.mark, f.mark) {
			snap.TasksHidden++
			continue
		}
		counts[[2]string{r.typ, r.state}]++
		if len(snap.Tasks) >= maxStatusRows {
			snap.TasksMore++
			continue
		}
		row := map[string]any{"task": r.id, "type": r.typ, "title": cat.TaskTypes[r.typ].Title, "state": r.state, "updated_at": fmtTime(r.at)}
		if r.reason != "" {
			row["reason"] = r.reason
		}
		snap.Tasks = append(snap.Tasks, row)
	}
	keys := slices.SortedFunc(mapKeys(counts), func(a, b [2]string) int { return strings.Compare(a[0]+"\x00"+a[1], b[0]+"\x00"+b[1]) })
	for _, k := range keys {
		title := cat.TaskTypes[k[0]].Title
		if title == "" {
			title = k[0]
		}
		snap.Counts = append(snap.Counts, map[string]any{"type": k[0], "title": title, "state": k[1], "count": counts[k]})
	}
	// The proposals from recommended on, never an audit sample item (REP-8, DIG-6).
	prows, err := tx.Query(ctx, `SELECT id, kind, state, coalesce(record->>'category', ''), coalesce(record->>'data_mark', ''), state_at FROM proposals
		WHERE tenant = $1 AND state = ANY($2) AND state_at >= $3 ORDER BY state_at DESC, id LIMIT 1000`, tenant, proposalViewStates, from)
	if err != nil {
		return nil, err
	}
	for prows.Next() {
		var id, kind, state, category, mark string
		var at time.Time
		if err := prows.Scan(&id, &kind, &state, &category, &mark, &at); err != nil {
			prows.Close()
			return nil, err
		}
		if mark != "" && !c.markWithin(mark, f.mark) {
			snap.PropsHidden++
			continue
		}
		if len(snap.Proposals) >= maxStatusRows {
			snap.ProposalsMore++
			continue
		}
		snap.Proposals = append(snap.Proposals, map[string]any{"proposal": id, "kind": kind, "state": state, "category": category, "state_at": fmtTime(at)})
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}
	raw, err = json.Marshal(snap)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `INSERT INTO view_snapshots (task, body, created_at) VALUES ($1, $2, $3)`, task, raw, now)
	return snap, err
}

// statusViews are the status.* views an attempt gets: the role's views of a profile and
// purpose that have them (PRF-1, REP-8). status.workers and status.campaigns are not built yet.
func statusViews(cat *Catalog, g *toolsGrant) []string {
	role, ok := cat.Roles[g.Role]
	if !ok || g.Purpose != "" && g.Purpose != "work" && g.Purpose != "report" {
		return nil
	}
	if p := role.Profile; p != "" && p != "worker" && p != "manager" && p != "reporter" {
		return nil
	}
	var out []string
	for _, v := range role.Views {
		if (v == statusTasksName || v == statusProposalsName) && !slices.Contains(out, v) {
			out = append(out, v)
		}
	}
	return out
}

// statusTools are the catalog entries of the views (PRF-1).
func statusTools(cat *Catalog, g *toolsGrant) []catalogTool {
	var out []catalogTool
	empty := map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}}
	for _, v := range statusViews(cat, g) {
		desc := "Tasks of the tenant in the report window: type, title, state and reason codes only; rows above your class only count in hidden."
		if v == statusProposalsName {
			desc = "Proposals of the tenant from recommended on: kind, state and category only; rows above your class only count in hidden."
		}
		out = append(out, catalogTool{Name: v, Builtin: true, Tool: toolspec.Tool{Description: desc}, Schema: empty})
	}
	return out
}

func statusToolName(name string) bool { return name == statusTasksName || name == statusProposalsName }

// statusToolCall answers a status.* view from the task's snapshot.
func (c *Core) statusToolCall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, g *toolsGrant, name string, offered bool) (*mcp.CallResult, error) {
	if !offered || !slices.Contains(statusViews(cat, g), name) {
		return &mcp.CallResult{IsError: true, Content: []mcp.Content{{Type: "text", Text: noPermission}}}, nil
	}
	snap, err := c.snapshotOf(ctx, tx, now, cat, g.Tenant, g.Task)
	if err != nil {
		return nil, err
	}
	out := map[string]any{"snapshot_id": snap.ID, "window": snap.Window, "from": snap.From}
	if name == statusTasksName {
		out["tasks"], out["counts"], out["hidden"], out["more"] = snap.Tasks, snap.Counts, snap.TasksHidden, snap.TasksMore
	} else {
		out["proposals"], out["hidden"], out["more"] = snap.Proposals, snap.PropsHidden, snap.ProposalsMore
	}
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	return &mcp.CallResult{StructuredContent: out, Content: []mcp.Content{{Type: "text", Text: string(b)}}}, nil
}

// paragraphBad is what code does not let through in a model paragraph of a digest: links,
// mentions, markup, channel commands and object ids (holds, approvals and the rest) (REP-2).
var paragraphBad = regexp.MustCompile(`(?i)https?://|www\.|@\w|[*_` + "`" + `\[\]<>|~#]|(^|\s)/[a-z]|\b[a-z]{1,8}_[0-9a-hjkmnp-tv-z]{26}\b`)

// digestParagraph is the model paragraph of a result as code lets it through, or "".
func digestParagraph(result map[string]any) string {
	text, _ := result["text"].(string)
	text = strings.TrimSpace(text)
	if text == "" || utf8.RuneCountInString(text) > maxDigestText || paragraphBad.MatchString(text) {
		return ""
	}
	return text
}

// deliverDigest delivers the digest of a completed reporter task: the code table from the
// snapshot, the hidden count and the model paragraph as a separate marked part, to the task's
// origin or its schedule's addresses only (REP-2, REP-9). A group chat gets the paragraph only
// from an attempt that saw nothing untrusted, or under model_text: labeled with the code line
// and the sources; a direct chat gets it with its sources.
func (c *Core) deliverDigest(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant, task string, rec []byte, result map[string]any) error {
	tv := cat.Tenants[tenant]
	if tv == nil {
		return nil
	}
	snap, err := c.snapshotOf(ctx, tx, now, cat, tenant, task)
	if err != nil {
		return err
	}
	var r struct {
		treeRecord
		DataMark        string   `json:"data_mark"`
		UntrustedSource string   `json:"untrusted_source"`
		UntrustedOf     []string `json:"untrusted_sources"`
		Initiator       string   `json:"-"`
	}
	if err := json.Unmarshal(rec, &r); err != nil {
		return err
	}
	var initiator string
	if err := tx.QueryRow(ctx, `SELECT initiator FROM tasks WHERE id = $1`, task).Scan(&initiator); err != nil {
		return err
	}
	sources := slices.Clone(r.UntrustedOf)
	if len(sources) == 0 && r.UntrustedSource != "" {
		sources = []string{r.UntrustedSource}
	}
	paragraph := digestParagraph(result)
	body := func(group bool, labeled bool) (map[string]any, string) {
		b := map[string]any{"form": "digest", "snapshot_id": snap.ID, "window": snap.Window, "rows": snap.Counts, "hidden": snap.TasksHidden + snap.PropsHidden}
		origin := "code"
		if paragraph != "" && (!r.UntrustedSeen || !group || labeled) {
			b["text"], origin = paragraph, "model"
			if r.UntrustedSeen {
				if sources == nil {
					sources = []string{}
				}
				b["sources"] = sources
				b["labeled"] = group
			}
		}
		return b, origin
	}
	plan := func(addr, targetKind, target, channel, recipient string, group, labeled bool) error {
		b, origin := body(group, labeled)
		_, err := c.planDelivery(ctx, tx, now, plannedDelivery{
			Tenant: tenant, Task: task, Kind: "digest", Addr: addr, TargetKind: targetKind, TargetID: target,
			ExecKind: "channel", ExecName: channel, Recipient: recipient, Body: b, DataMark: r.DataMark, DataOrigin: origin,
		})
		return err
	}
	name, err := scheduleOf(ctx, tx, r.treeRecord, task)
	if err != nil {
		return err
	}
	if name != "" {
		sc := tv.Schedules[name]
		for _, x := range sc.DeliverTo {
			addr, _ := x.(string)
			kind, target, _ := strings.Cut(addr, ":")
			switch kind {
			case "chat":
				b, err := bindingByName(ctx, tx, tenant, target)
				if err != nil {
					return err
				}
				if b == nil || !c.markWithin(r.DataMark, b.Ceiling) {
					continue
				}
				if err := plan(addr, "chat", b.Chat, b.Channel, "", true, b.ModelText == "labeled"); err != nil {
					return err
				}
			case "group":
				if _, ok := tv.Groups[target]; !ok {
					continue
				}
				chats, err := c.principalChats(ctx, tx, now, tv, "", func(groups []string) bool {
					return slices.Contains(groups, target) && hasData(tv, groups, r.DataMark)
				})
				if err != nil {
					return err
				}
				seen := map[string]bool{}
				for _, ch := range chats {
					to := Chat{Kind: "direct", ExternalID: ch.ExternalID}
					if seen[ch.Channel+"/"+to.ID()] {
						continue
					}
					seen[ch.Channel+"/"+to.ID()] = true
					if err := c.firstContact(ctx, tx, now, tv, ch.Channel, ch.Principal, to); err != nil {
						return err
					}
					if err := plan(addr, "chat", to.ID(), ch.Channel, ch.Principal, false, false); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if !r.reportsToOrigin() {
		return nil
	}
	o := r.Origin
	if o.Chat.Kind == "group" {
		b, err := loadBinding(ctx, tx, tenant, o.Channel, o.Chat.ID())
		if err != nil {
			return err
		}
		if b == nil || !c.markWithin(r.DataMark, b.Ceiling) {
			return nil
		}
		return plan("origin", "chat", o.Chat.ID(), o.Channel, "", true, b.ModelText == "labeled")
	}
	return plan("origin", "chat", o.Chat.ID(), o.Channel, initiator, false, false)
}
