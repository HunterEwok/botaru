// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// The P2 views and controls of campaigns, script runs, proposals and escalations (ORC-1,
// SCR-5, SCR-11, PRP-4, DIG-1, MGR-5). The person of the P2 credential sees the objects of its
// own tenant that it may read, as for a task (PRM-9): an object someone may not read answers
// exactly as a missing one, not_found. A view never shows a record whose data mark the person
// does not hold. The control operations check the rights the card buttons check and write an
// audit record of the command, also of a refusal of a person who may see the object (AUD-1).

// ObjectRequest is p2-object-request.
type ObjectRequest struct {
	Op string `json:"op"`
	ID string `json:"id"`
}

// p2viewer is the person of a P2 credential in its own tenant: the groups of an active
// principal; an unknown or inactive one has none and may read nothing.
type p2viewer struct {
	tv        *TenantView
	cat       *Catalog
	principal string
	groups    []string
	active    bool
}

func (c *Core) p2viewer(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, principal string) (*p2viewer, error) {
	v := &p2viewer{tv: tv, cat: cat, principal: principal}
	if principal == "" || principal == "system" {
		return v, nil
	}
	p, err := loadPrincipal(ctx, tx, now, tv.Name, principal)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, nil
	}
	if err != nil {
		return nil, err
	}
	if p.State == "active" {
		v.active, v.groups = true, groupClosure(tv, p.Groups)
	}
	return v, nil
}

func (v *p2viewer) can(capability string) bool { return v.active && hasCap(v.tv, v.groups, capability) }

// holds says whether the person holds the data mark of a record (an empty mark is no class).
func (v *p2viewer) holds(mark string) bool {
	return v.active && (mark == "" || hasData(v.tv, v.groups, mark))
}

func (v *p2viewer) actor() Actor { return Actor{Principal: v.principal, Via: "farmctl"} }

// ownTenant is the tenant of a view of the person's records: its own; another one named is
// forbidden, also for an operator of farm, whose people hold no rights in another tenant (TEN-2).
func (c *Core) ownTenant(ctx context.Context, op Operator, named string) (*TenantView, *Catalog, error) {
	if named != "" && named != op.Tenant {
		return nil, nil, refuse("forbidden", "the credential is for another tenant")
	}
	return c.requireTenant(ctx, op.Tenant)
}

// listPage reads the limit and the cursor <created_at unix micros>/<id> of a newest-first list.
func listPage(limit int, after string) (int, time.Time, string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	if after == "" {
		return limit, time.Time{}, "", nil
	}
	us, id, ok := strings.Cut(after, "/")
	n, err := strconv.ParseInt(us, 10, 64)
	if !ok || err != nil || id == "" {
		return 0, time.Time{}, "", refuse("input_schema_invalid", "after is not a cursor of this operation")
	}
	return limit, time.UnixMicro(n).UTC(), id, nil
}

func listNext(at time.Time, id string) string { return fmt.Sprintf("%d/%s", at.UnixMicro(), id) }

// marks are the data marks the person holds.
func (v *p2viewer) marks() []string {
	var out []string
	if !v.active {
		return out
	}
	for _, g := range v.groups {
		for _, m := range v.tv.Groups[g].Data {
			if !slices.Contains(out, m) {
				out = append(out, m)
			}
		}
	}
	return append(out, "")
}

// capped lists the names whose capability prefix+name the person holds.
func (v *p2viewer) capped(prefix string, names []string) []string {
	var out []string
	for _, n := range names {
		if v.can(prefix + n) {
			out = append(out, n)
		}
	}
	return out
}

// itemTypes are the item types a campaign can have: the task types and action.<a>.
func itemTypes(cat *Catalog) []string {
	out := slices.Sorted(mapKeys(cat.TaskTypes))
	for _, a := range slices.Sorted(mapKeys(cat.Actions)) {
		out = append(out, "action."+a)
	}
	return out
}

// mayReadCampaign: the initiator, a person who decides it or one with task.read of its item
// type, holding its data mark (PRM-9).
func (v *p2viewer) mayReadCampaign(cmp *campaignRow) bool {
	if !v.holds(cmp.Rec.DataMark) {
		return false
	}
	return v.principal == cmp.Initiator || mayDecideCampaign(v.tv, v.groups, cmp) || hasCap(v.tv, v.groups, "task.read:"+cmp.Rec.itemType())
}

// campaignTitle is the code title of a campaign: the title of its item type, or its action.
func campaignTitle(cat *Catalog, cmp *campaignRow) string {
	if a := cmp.Rec.action(); a != "" {
		return "action " + a
	}
	if tt, ok := cat.TaskTypes[cmp.Rec.itemType()]; ok && tt.Title != "" {
		return tt.Title
	}
	return cmp.Rec.itemType()
}

// campaignItemType is the item type of a campaign row in SQL: the template type or action.<a>.
const campaignItemType = `CASE WHEN record->'template'->>'kind' = 'action' THEN 'action.' || (record->'template'->>'action') ELSE record->'template'->>'type' END`

// activePrincipal is the person's id when it is active, else a value no initiator has.
func (v *p2viewer) activePrincipal() string {
	if !v.active {
		return "-"
	}
	return v.principal
}

// CampaignList is P2 campaign.list: the campaigns of the person's tenant it may read, newest
// first, as rows of p2-list-response (ORC-1, PRM-9).
func (c *Core) CampaignList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	tv, cat, err := c.ownTenant(ctx, op, req.Tenant)
	if err != nil {
		return nil, err
	}
	limit, from, fromID, err := listPage(req.Limit, req.After)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		// The read right of mayReadCampaign in SQL, so the page limit applies in the store.
		types := itemTypes(cat)
		rows, err := tx.Query(ctx, `SELECT `+campaignColumns+` FROM campaigns c WHERE tenant = $1 AND ($2 = '' OR state = $2)
			AND ($3 = '' OR (created_at, id) < ($4, $3)) AND coalesce(record->>'data_mark', '') = ANY($6)
			AND (initiator = $7 OR `+campaignItemType+` = ANY($8) OR record->'template'->>'kind' = 'task' AND record->'template'->>'type' = ANY($9)
				OR record->'template'->>'kind' = 'action' AND record->'template'->>'action' = ANY($10))
			ORDER BY created_at DESC, id DESC LIMIT $5`, tv.Name, req.State, fromID, from, limit+1, v.marks(), v.activePrincipal(),
			nonNil(v.capped("task.read:", types)), nonNil(v.capped("task:", slices.Sorted(mapKeys(cat.TaskTypes)))),
			nonNil(v.capped("action.approve:", slices.Sorted(mapKeys(cat.Actions)))))
		if err != nil {
			return err
		}
		defer rows.Close()
		items, next := []any{}, ""
		var last *campaignRow
		for rows.Next() {
			cmp, err := scanCampaign(rows)
			if err != nil {
				return err
			}
			if !v.mayReadCampaign(cmp) {
				continue
			}
			if len(items) == limit {
				next = listNext(last.CreatedAt, last.ID)
				break
			}
			items = append(items, map[string]any{"id": cmp.ID, "state": cmp.State, "created_at": fmtTime(cmp.CreatedAt), "title": campaignTitle(cat, cmp)})
			last = cmp
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = map[string]any{"items": items}
		if next != "" {
			out["next"] = next
		}
		return nil
	})
	return out, err
}

// loadOwnCampaign reads a campaign of the person's tenant; nil when there is none.
func loadOwnCampaign(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*campaignRow, error) {
	cmp, err := loadCampaign(ctx, tx, tenant, id, lock)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return cmp, err
}

// CampaignShow is P2 campaign.show: the frozen record of spec/model/campaign.schema.json for a
// person who may read it; the read goes into the audit (ORC-1, PRM-9, AUD-1).
func (c *Core) CampaignShow(ctx context.Context, op Operator, req ObjectRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cmp, err := loadOwnCampaign(ctx, tx, tv.Name, req.ID, false)
		if err != nil {
			return err
		}
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		if cmp == nil || !v.mayReadCampaign(cmp) {
			return refuse("not_found", "no such campaign")
		}
		if out, err = cmp.Record(ctx, tx); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "campaign.show", Target: &Target{Kind: "campaign", ID: cmp.ID}, OK: true,
			After: map[string]any{"data_mark": cmp.Rec.DataMark},
		})
		return err
	})
	return out, err
}

// P2CampaignControlRequest is p2-campaign-control-request.
type P2CampaignControlRequest struct {
	ID      string `json:"id"`
	Command string `json:"command"`
}

// p2Refusal writes the audit record of a refused command and returns the refusal with the
// record committed (AUD-1).
func (c *Core) p2Refusal(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, v *p2viewer, command string, target *Target, after map[string]any, reason, detail string) error {
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: tenant, Kind: "command", Actor: v.actor(), Command: command, Target: target, OK: false, Reason: reason, After: after,
	}); err != nil {
		return err
	}
	return &committed{p: refuse(reason, "%s", detail)}
}

// CampaignControlP2 is P2 campaign.control: only "Pause" and "Cancel", by the people the card
// buttons serve: the initiator, a person who decides the campaign, or one with admin:people.
// A cancellation by the initiator is canceled(initiator), by anyone else canceled(admin); a
// proposed campaign is rejected with that reason. A repeat gives the same answer (ORC-1,
// ORC-2, IDM-3).
func (c *Core) CampaignControlP2(ctx context.Context, op Operator, req P2CampaignControlRequest) (*CampaignControlResult, error) {
	if req.Command != "pause" && req.Command != "cancel" {
		return nil, refuse("input_schema_invalid", "P2 campaign.control is pause or cancel")
	}
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out *CampaignControlResult
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cmp, err := loadOwnCampaign(ctx, tx, tv.Name, req.ID, true)
		if err != nil {
			return err
		}
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		if cmp == nil || !v.mayReadCampaign(cmp) && !v.can("admin:people") && v.principal != cmp.Initiator {
			return refuse("not_found", "no such campaign")
		}
		target := &Target{Kind: "campaign", ID: cmp.ID}
		if !v.active || v.principal != cmp.Initiator && !mayDecideCampaign(tv, v.groups, cmp) && !v.can("admin:people") {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.control", target, map[string]any{"command": req.Command},
				"forbidden", "only the initiator, a person who decides the campaign or an admin")
		}
		reason := ""
		switch req.Command {
		case "pause":
			switch cmp.State {
			case "paused":
			case "running":
				if err := c.setCampaignState(ctx, tx, now, cmp, "paused", "human", ""); err != nil {
					return err
				}
			default:
				return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.control", target, map[string]any{"command": req.Command}, "already_terminal", "the campaign is not running")
			}
		case "cancel":
			reason = "admin"
			if v.principal == cmp.Initiator {
				reason = "initiator"
			}
			switch cmp.State {
			case "proposed":
				if err := c.rejectCampaign(ctx, tx, now, cmp, reason); err != nil {
					return err
				}
			case "running", "paused":
				if err := c.stopCampaign(ctx, tx, now, cat, cmp, reason); err != nil {
					return err
				}
			default:
				// The same cancellation again answers as the first did (IDM-3); a rejected
				// campaign keeps no stop reason, so its own audit record says it.
				same := cmp.StopReason == reason && (cmp.State == "stopping" || cmp.State == "canceled")
				if cmp.State == "rejected" {
					if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM audit WHERE tenant = $1 AND record->>'command' = 'campaign.control'
						AND record->'target'->>'id' = $2 AND (record->>'ok')::boolean AND record->>'reason' = $3)`, tv.Name, cmp.ID, reason).Scan(&same); err != nil {
						return err
					}
				}
				if !same {
					return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.control", target, map[string]any{"command": req.Command}, "already_terminal", "the campaign has ended")
				}
				out = &CampaignControlResult{ID: cmp.ID, State: cmp.State}
				return nil
			}
		}
		// An item that ends inside the transaction may already have moved the campaign on.
		if err := tx.QueryRow(ctx, `SELECT state FROM campaigns WHERE id = $1`, cmp.ID).Scan(&cmp.State); err != nil {
			return err
		}
		out = &CampaignControlResult{ID: cmp.ID, State: cmp.State}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "command", Actor: v.actor(), Command: "campaign.control", Target: target, OK: true, Reason: reason,
			After: map[string]any{"command": req.Command, "state": cmp.State},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CampaignRetryFailed is P2 campaign.retry_failed, the "Retry failed" of the summary: only the
// initiator, of an ended campaign whose content is kept; the core proposes a new campaign of
// its failed, unknown and skipped items with the same template and policy, its untrusted
// flag, a refs link, no continuation and a new approval card. The key system:retry:<id> makes
// a repeat give the same campaign (ORC-1, IDM-3).
func (c *Core) CampaignRetryFailed(ctx context.Context, op Operator, req ObjectRequest) (*Done, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out *Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		cmp, err := loadOwnCampaign(ctx, tx, tv.Name, req.ID, true)
		if err != nil {
			return err
		}
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		if cmp == nil || !v.mayReadCampaign(cmp) && v.principal != cmp.Initiator {
			return refuse("not_found", "no such campaign")
		}
		target := &Target{Kind: "campaign", ID: cmp.ID}
		if !v.active || v.principal != cmp.Initiator {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.retry_failed", target, nil, "forbidden", "only the initiator retries")
		}
		// A fanout of a script step belongs to its run: the run, not a person, decides it (SCR-6).
		if cmp.Rec.ScriptRun != "" {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.retry_failed", target, nil, "forbidden", "the campaign is a step of a script run")
		}
		if !slices.Contains([]string{"completed", "failed", "canceled"}, cmp.State) {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.retry_failed", target, nil, "already_decided", "the campaign has not ended")
		}
		// Its items kept no arguments past the content period or an erasure (RET-10).
		if cmp.Purged {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.retry_failed", target, nil, "already_terminal", "the campaign's content is gone")
		}
		res, err := c.retryCampaign(ctx, tx, now, cat, cmp)
		if err != nil {
			return err
		}
		if res.State == "rejected" {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "campaign.retry_failed", target, nil, res.Reason, "the retry does not pass the checks")
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "command", Actor: v.actor(), Command: "campaign.retry_failed", Target: target, OK: true,
			After: map[string]any{"campaign": res.Campaign, "state": res.State},
		})
		if err != nil {
			return err
		}
		out = &Done{OK: true, ID: res.Campaign, State: res.State, AuditSeq: seq}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// mayReadRun: the initiator, or a person with task.read of the anchor task's type, holding the
// run's data mark (PRM-9). A purged tombstone shows only in the list.
func (v *p2viewer) mayReadRun(r *scriptRun, anchorType string) bool {
	if !v.holds(r.Rec.DataMark) {
		return false
	}
	return v.principal == r.Initiator || anchorType != "" && hasCap(v.tv, v.groups, "task.read:"+anchorType)
}

const scriptListColumns = `r.id, r.tenant, coalesce(r.anchor_task, ''), r.initiator, r.state, coalesce(r.reason, ''), r.plan_hash, r.record, r.nonce,
	r.expires_at, r.deadline_at, r.stopped, r.tree_left, r.progress_at, r.created_at, coalesce(t.type, '')`

func scanRunWithAnchor(row pgx.Row) (*scriptRun, string, error) {
	var r scriptRun
	var rec []byte
	var anchorType string
	if err := row.Scan(&r.ID, &r.Tenant, &r.Anchor, &r.Initiator, &r.State, &r.Reason, &r.PlanHash, &rec, &r.Nonce, &r.ExpiresAt,
		&r.DeadlineAt, &r.Stopped, &r.TreeLeft, &r.ProgressAt, &r.CreatedAt, &anchorType); err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(rec, &r.Rec); err != nil {
		return nil, "", err
	}
	return &r, anchorType, nil
}

// loadOwnRun reads a run of the person's tenant with the type of its anchor task; nil when
// there is none.
func loadOwnRun(ctx context.Context, tx pgx.Tx, tenant, id string, lock bool) (*scriptRun, string, error) {
	q := `SELECT ` + scriptListColumns + ` FROM script_runs r LEFT JOIN tasks t ON t.id = r.anchor_task WHERE r.id = $1 AND r.tenant = $2`
	if lock {
		q += ` FOR UPDATE OF r`
	}
	r, typ, err := scanRunWithAnchor(tx.QueryRow(ctx, q, id, tenant))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	return r, typ, err
}

// ScriptList is P2 script.list: the runs of the person's tenant it may read, newest first
// (SCR-5, PRM-9).
func (c *Core) ScriptList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	tv, cat, err := c.ownTenant(ctx, op, req.Tenant)
	if err != nil {
		return nil, err
	}
	limit, from, fromID, err := listPage(req.Limit, req.After)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+scriptListColumns+` FROM script_runs r LEFT JOIN tasks t ON t.id = r.anchor_task
			WHERE r.tenant = $1 AND ($2 = '' OR r.state = $2) AND ($3 = '' OR (r.created_at, r.id) < ($4, $3))
			AND coalesce(r.record->>'data_mark', '') = ANY($6) AND (r.initiator = $7 OR t.type = ANY($8))
			ORDER BY r.created_at DESC, r.id DESC LIMIT $5`,
			tv.Name, req.State, fromID, from, limit+1, v.marks(), v.activePrincipal(), nonNil(v.capped("task.read:", slices.Sorted(mapKeys(cat.TaskTypes)))))
		if err != nil {
			return err
		}
		defer rows.Close()
		items, next := []any{}, ""
		var last *scriptRun
		for rows.Next() {
			r, typ, err := scanRunWithAnchor(rows)
			if err != nil {
				return err
			}
			if !v.mayReadRun(r, typ) {
				continue
			}
			if len(items) == limit {
				next = listNext(last.CreatedAt, last.ID)
				break
			}
			items = append(items, map[string]any{"id": r.ID, "state": r.State, "created_at": fmtTime(r.CreatedAt)})
			last = r
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = map[string]any{"items": items}
		if next != "" {
			out["next"] = next
		}
		return nil
	})
	return out, err
}

// ScriptShow is P2 script.show: the run record of spec/model/script-run.schema.json for a
// person who may read it, with the accepted draft, the frozen lists and the step states; the
// read goes into the audit (SCR-5, PRM-9, AUD-1).
func (c *Core) ScriptShow(ctx context.Context, op Operator, req ObjectRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, typ, err := loadOwnRun(ctx, tx, tv.Name, req.ID, false)
		if err != nil {
			return err
		}
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		if r == nil || r.Rec.Purged || !v.mayReadRun(r, typ) {
			return refuse("not_found", "no such script run")
		}
		if out, err = r.Record(ctx, tx); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "script.show", Target: &Target{Kind: "script", ID: r.ID}, OK: true,
			After: map[string]any{"data_mark": r.Rec.DataMark},
		})
		return err
	})
	return out, err
}

// ScriptCancel is P2 script.cancel by the initiator or a person with admin:people: a proposed
// run is rejected, a running or paused one is canceled in one transaction with its unreleased
// steps, the waiting approvals of its steps and the tasks and campaigns of released steps
// (SCR-11). The outcome is canceled(initiator) or canceled(admin); a repeat gives the same
// answer (IDM-3).
func (c *Core) ScriptCancel(ctx context.Context, op Operator, req ObjectRequest) (*Done, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out *Done
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		r, typ, err := loadOwnRun(ctx, tx, tv.Name, req.ID, true)
		if err != nil {
			return err
		}
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		if r == nil || !v.mayReadRun(r, typ) && !v.can("admin:people") && v.principal != r.Initiator {
			return refuse("not_found", "no such script run")
		}
		target := &Target{Kind: "script", ID: r.ID}
		if !v.active || v.principal != r.Initiator && !v.can("admin:people") {
			return c.p2Refusal(ctx, tx, now, tv.Name, v, "script.cancel", target, nil, "forbidden", "only the initiator or an admin cancels a run")
		}
		reason := "admin"
		if v.principal == r.Initiator {
			reason = "initiator"
		}
		if r.State == "proposed" || r.State == "rejected" {
			// Cancel of a proposed run is rejected(declined), as the card's button (script.md).
			reason = "declined"
		}
		switch r.State {
		case "proposed":
			if err := c.rejectRun(ctx, tx, now, r, reason); err != nil {
				return err
			}
		case "running", "paused":
			if err := c.cancelRun(ctx, tx, now, cat, r, reason); err != nil {
				return err
			}
		default:
			// The same cancellation again answers as the first did (IDM-3).
			if r.Reason != reason || r.State != "canceled" && r.State != "rejected" {
				return c.p2Refusal(ctx, tx, now, tv.Name, v, "script.cancel", target, nil, "already_terminal", "the run has ended")
			}
			out = &Done{OK: true, ID: r.ID, State: r.State}
			return nil
		}
		seq, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "command", Actor: v.actor(), Command: "script.cancel", Target: target, OK: true, Reason: reason,
			After: map[string]any{"state": r.State},
		})
		if err != nil {
			return err
		}
		out = &Done{OK: true, ID: r.ID, State: r.State, AuditSeq: seq}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// proposalShown are the states in which people see a proposal (DIG-1): recommended and every
// state after it, and the audit sample states; expired only of a proposal that was presented.
var proposalShown = []string{"recommended", "presented", "declined", "expired", "noted", "realizing", "realized", "realization_failed",
	"reverted", "sampled", "sample_presented", "sample_judged", "sample_closed"}

// proposalBlind says whether a sample item is blind: the scores, the veto and the reason stay
// hidden until the answer "Would accept", also after a close without it (DIG-6).
func proposalBlind(state, answer string) bool {
	return state == "sampled" || state == "sample_presented" || state == "sample_closed" && answer != "would_accept"
}

// ProposalListRequest is p2-proposal-list-request.
type ProposalListRequest struct {
	State    []string `json:"state,omitempty"`
	Category []string `json:"category,omitempty"`
	Limit    int      `json:"limit,omitempty"`
}

// ProposalShowRequest is p2-proposal-show-request.
type ProposalShowRequest struct {
	Proposal string `json:"proposal"`
}

// proposalCategories are the categories the person may decide on; none is forbidden.
func (v *p2viewer) proposalCategories() ([]string, error) {
	var out []string
	for _, cat := range proposalCategories {
		if v.can("proposal.decide:" + cat) {
			out = append(out, cat)
		}
	}
	if len(out) == 0 {
		pr := refuse("forbidden", "proposal.decide of a category is needed")
		pr.Needs = []string{"proposal.decide:<category>"}
		return nil, pr
	}
	return out, nil
}

// ProposalList is P2 proposal.list: short rows of the proposals of the person's own tenant in
// the categories it may decide, only those shown to people (DIG-1): recommended and later
// states, and the audit sample items, whose state marks them. A row is metadata only; a blind
// sample item has no reason (DIG-6). Decisions are card buttons, not P2 (APR-4, PRP-4).
func (c *Core) ProposalList(ctx context.Context, op Operator, req ProposalListRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		cats, err := v.proposalCategories()
		if err != nil {
			return err
		}
		if len(req.Category) > 0 {
			cats = slices.DeleteFunc(cats, func(s string) bool { return !slices.Contains(req.Category, s) })
		}
		states := proposalShown
		if len(req.State) > 0 {
			states = slices.DeleteFunc(slices.Clone(proposalShown), func(s string) bool { return !slices.Contains(req.State, s) })
		}
		rows, err := tx.Query(ctx, `SELECT id, state, record->'body'->>'category', record->>'state_reason', coalesce(record->>'data_mark', ''),
			coalesce(record->'sample_answer'->>'answer', '') FROM proposals
			WHERE tenant = $1 AND state = ANY($2) AND record->'body'->>'category' = ANY($3) AND (state <> 'expired' OR presented_at IS NOT NULL)
			ORDER BY created_at DESC, id DESC`, tv.Name, nonNil(states), nonNil(cats))
		if err != nil {
			return err
		}
		defer rows.Close()
		items := []any{}
		ceiling := c.personMark(tv, v.groups)
		for rows.Next() {
			var id, state, category, mark, answer string
			var reason *string
			if err := rows.Scan(&id, &state, &category, &reason, &mark, &answer); err != nil {
				return err
			}
			// Only what the person may show (DIG-2).
			if c.digestShow(mark, ceiling) != "full" {
				continue
			}
			if len(items) == limit {
				break
			}
			it := map[string]any{"id": id, "category": category, "state": state, "state_reason": nil}
			if reason != nil && !proposalBlind(state, answer) {
				it["state_reason"] = *reason
			}
			if mark != "" {
				it["data_mark"] = mark
			}
			items = append(items, it)
		}
		out = map[string]any{"items": items}
		return rows.Err()
	})
	return out, err
}

// ProposalShow is P2 proposal.show: the proposal record with its trial and the reverse edit
// (PRP-20), for a person who may decide its category, of a proposal shown to people (DIG-1)
// and of a data mark within the person's ceiling; anything else is not_found. A blind sample
// item shows no reviews, verdict or reason (DIG-6). The author's note stays the untrusted
// labeled field of the body. The read goes into the audit (AUD-1); it changes nothing (PRP-4).
func (c *Core) ProposalShow(ctx context.Context, op Operator, req ProposalShowRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		if _, err := v.proposalCategories(); err != nil {
			return err
		}
		p, err := readProposal(ctx, tx, req.Proposal)
		if errors.Is(err, pgx.ErrNoRows) {
			return refuse("not_found", "no such proposal")
		}
		if err != nil {
			return err
		}
		mark := asString(p.Rec["data_mark"])
		if p.Tenant != tv.Name || !slices.Contains(proposalShown, p.State) || p.State == "expired" && p.PresentedAt == nil ||
			!v.can("proposal.decide:"+p.body().Category) || c.digestShow(mark, c.personMark(tv, v.groups)) != "full" {
			return refuse("not_found", "no such proposal")
		}
		out = p.Rec
		if proposalBlind(p.State, asString(mmapAny(p.Rec["sample_answer"])["answer"])) {
			out = map[string]any{}
			for k, x := range p.Rec {
				out[k] = x
			}
			out["reviews"], out["verdict"], out["state_reason"] = []any{}, nil, nil
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "proposal.show", Target: &Target{Kind: "proposal", ID: p.ID}, OK: true,
			After: map[string]any{"data_mark": mark},
		})
		return err
	})
	return out, err
}

// escalationViewColumns are the columns of an escalation for the views, with the data mark of
// its source task.
const escalationViewColumns = `e.id, e.tenant, e.task, e.lease_no, e.state, coalesce(e.reason, ''), coalesce(e.merged_into, ''), e.dup_count, e.score,
	e.fields, e.untrusted, e.depth, e.note, coalesce(e.published_at, e.created_at), coalesce(t.record->>'data_mark', ''), t.id IS NOT NULL`

type escalationView struct {
	id, tenant, task, state, reason, merged, mark string
	leaseNo, dups, depth                          int
	score                                         *int
	fields                                        map[string]any
	untrusted, hasTask                            bool
	note                                          *string
	at                                            time.Time
}

func scanEscalationView(row pgx.Row) (*escalationView, error) {
	var e escalationView
	var raw []byte
	if err := row.Scan(&e.id, &e.tenant, &e.task, &e.leaseNo, &e.state, &e.reason, &e.merged, &e.dups, &e.score, &raw, &e.untrusted,
		&e.depth, &e.note, &e.at, &e.mark, &e.hasTask); err != nil {
		return nil, err
	}
	return &e, json.Unmarshal(raw, &e.fields)
}

// record is the escalation by spec/model/escalation.schema.json, without the note.
func (e *escalationView) record() map[string]any {
	r := map[string]any{"id": e.id, "tenant": e.tenant, "source_attempt": e.task + ":" + strconv.Itoa(e.leaseNo),
		"untrusted": e.untrusted, "state": e.state, "depth": e.depth, "dup_count": e.dups}
	for _, k := range []string{"category", "subject", "observed", "requested_response", "urgency"} {
		if x, ok := e.fields[k]; ok && x != nil {
			r[k] = x
		}
	}
	if e.reason != "" {
		r["reason"] = e.reason
	}
	if e.merged != "" {
		r["merged_into"] = e.merged
	}
	if e.score != nil {
		r["lens_score"] = *e.score
	}
	if e.note != nil && *e.note != "" {
		r["note_ref"] = e.id
	}
	return r
}

// EscalationList is P2 escalation.list: the published escalations of the person's own
// tenant, newest first, metadata only, of the source tasks whose data mark the person holds.
// An escalation never leaves its tenant: a tenant named otherwise is forbidden, also for farm
// (MGR-5, TEN-2).
func (c *Core) EscalationList(ctx context.Context, op Operator, req TenantRequest) (map[string]any, error) {
	tv, cat, err := c.ownTenant(ctx, op, req.Tenant)
	if err != nil {
		return nil, err
	}
	limit, from, fromID, err := listPage(req.Limit, req.After)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+escalationViewColumns+` FROM escalations e LEFT JOIN tasks t ON t.id = e.task
			WHERE e.tenant = $1 AND e.state <> 'staged' AND ($2 = '' OR e.state = $2)
			AND ($4 = '' OR (coalesce(e.published_at, e.created_at), e.id) < ($5, $4)) AND coalesce(t.record->>'data_mark', '') = ANY($6)
			ORDER BY coalesce(e.published_at, e.created_at) DESC, e.id DESC LIMIT $3`, tv.Name, req.State, limit+1, fromID, from, v.marks())
		if err != nil {
			return err
		}
		defer rows.Close()
		items, next := []any{}, ""
		var last *escalationView
		for rows.Next() {
			e, err := scanEscalationView(rows)
			if err != nil {
				return err
			}
			if len(items) == limit {
				next = listNext(last.at, last.id)
				break
			}
			items = append(items, map[string]any{"id": e.id, "state": e.state, "created_at": fmtTime(e.at), "title": "escalation " + mstr(e.fields, "category")})
			last = e
		}
		if err := rows.Err(); err != nil {
			return err
		}
		out = map[string]any{"items": items}
		if next != "" {
			out["next"] = next
		}
		return nil
	})
	return out, err
}

// EscalationShow is P2 escalation.show: the escalation record for an active person of its
// tenant who holds the data mark of its source task; anything else is not_found. The
// untrusted note goes only to such a person with audit.read, inside the untrusted wrapper,
// and never when the source task is gone; the read goes into the audit. The lens and the
// manager never see it (MGR-5).
func (c *Core) EscalationShow(ctx context.Context, op Operator, req ObjectRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		e, err := scanEscalationView(tx.QueryRow(ctx, `SELECT `+escalationViewColumns+` FROM escalations e LEFT JOIN tasks t ON t.id = e.task
			WHERE e.id = $1 AND e.tenant = $2 AND e.state <> 'staged'`, req.ID, tv.Name))
		if errors.Is(err, pgx.ErrNoRows) || err == nil && !v.holds(e.mark) {
			return refuse("not_found", "no such escalation")
		}
		if err != nil {
			return err
		}
		out = map[string]any{"escalation": e.record()}
		if e.note == nil || *e.note == "" || !e.hasTask || !v.can("audit.read") {
			return nil
		}
		mark := "untrusted-" + ids.Secret(9)
		out["note"] = "<" + mark + ">\n" + *e.note + "\n</" + mark + ">"
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: e.tenant, Kind: "read", Actor: v.actor(), Command: "escalation.show", Target: &Target{Kind: "escalation", ID: e.id}, OK: true,
			After: map[string]any{"note": true, "data_mark": e.mark},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
