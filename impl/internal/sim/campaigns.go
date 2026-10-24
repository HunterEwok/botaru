// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// The campaign scenarios (R-11, R-12): ops-operator turns a person's request over many machines
// into one campaign over a catalog selector, and the core, not the model, releases its items
// (ORC-1..ORC-12).

// campaignOf waits for the campaign the task of a message key proposed and returns its id.
func (d *Director) campaignOf(ctx context.Context, tenant, key string) (string, error) {
	var id string
	err := d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT c.id FROM campaigns c JOIN tasks t ON t.id = c.parent_task
			WHERE c.tenant = $1 AND t.idempotency_key = $2 ORDER BY c.created_at LIMIT 1`, tenant, key).Scan(&id)
		return e == nil, nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("the task of the message proposed no campaign within %v", d.Timeout)
	}
	return id, err
}

// campaignState is a campaign's state, its pause and stop reasons and its items by state.
type campaignState struct {
	state, pause, stop string
	items              map[string]int
	resources          []string
}

func (d *Director) readCampaign(ctx context.Context, id string) (campaignState, error) {
	db := d.S.Bench.Store().Pool
	var c campaignState
	if err := db.QueryRow(ctx, `SELECT state, coalesce(pause_reason, ''), coalesce(stop_reason, '') FROM campaigns WHERE id = $1`, id).
		Scan(&c.state, &c.pause, &c.stop); err != nil {
		return c, err
	}
	c.items = map[string]int{}
	rows, err := db.Query(ctx, `SELECT resource, state FROM campaign_items WHERE campaign = $1 ORDER BY n`, id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var res, st string
		if err := rows.Scan(&res, &st); err != nil {
			return c, err
		}
		c.items[st]++
		c.resources = append(c.resources, res)
	}
	return c, rows.Err()
}

// waitCampaign waits until a campaign is in one of the states.
func (d *Director) waitCampaign(ctx context.Context, id string, timeout time.Duration, states ...string) (campaignState, error) {
	var c campaignState
	err := d.S.waitFor(ctx, timeout, func() (bool, error) {
		var err error
		c, err = d.readCampaign(ctx, id)
		return err == nil && slices.Contains(states, c.state), nil
	})
	if errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("campaign %s did not reach %v within %v (state %s, items %v)", id, states, timeout, c.state, c.items)
	}
	return c, err
}

// hostsOf are the machines of a tenant, of one kind or all ("").
func hostsOf(t *FarmTenant, kind string) []string {
	var out []string
	for _, h := range t.Hosts {
		if kind == "" || h.Kind == kind {
			out = append(out, h.ID)
		}
	}
	return out
}

// campaignRead plays a read campaign (R-11): bob asks the operator whether the services are up
// on all machines; the operator proposes a campaign of the availability summary over the
// selector of every machine, which runs without a card (below confirm_read_over), and each item
// is a task scoped to its machine.
func campaignRead(ctx context.Context, d *Director, sc string) error {
	t := d.S.Farm().TenantWith("ops-operator")
	bob, dm := d.S.PersonIn(t.Name, "bob"), d.S.PersonIn(t.Name, "bob-dm")
	key, err := d.S.Say(bob, dm, "/rollout Check whether the services are up on all machines.")
	if err != nil {
		return err
	}
	id, err := d.campaignOf(ctx, t.Name, key)
	d.check(sc, "campaign_proposed", err == nil, "campaign %s %v", id, err)
	if err != nil {
		return nil
	}
	var kind string
	var card bool
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT record->'template'->>'kind', card FROM campaigns WHERE id = $1`, id).Scan(&kind, &card)
	hosts := hostsOf(t, "")
	c, err := d.waitCampaign(ctx, id, 2*d.Timeout, "completed", "failed", "canceled", "rejected")
	slices.Sort(c.resources)
	slices.Sort(hosts)
	d.check(sc, "campaign_read", err == nil && kind == "task" && !card && c.state == "completed" && c.items["ok"] == len(hosts) && slices.Equal(c.resources, hosts),
		"template %s, card %v, state %s, items %v over %v (the tenant's machines %v) %v", kind, card, c.state, c.items, c.resources, hosts, err)
	// Each item ran as a task of the availability summary scoped to its machine.
	var items, scoped int
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE k.type = 'ops.health.v1' AND k.state = 'completed')
		FROM campaign_items i JOIN tasks k ON k.id = i.task WHERE i.campaign = $1`, id).Scan(&items, &scoped)
	d.check(sc, "items_tasks", items == len(hosts) && scoped == items, "%d item tasks, %d completed availability summaries", items, scoped)
	return nil
}

// campaignActionCanary plays an action campaign (R-12): the data volumes of the database
// machines fill and bob asks the operator to clean the pipeline temporary files there; the campaign of cleanup_pipeline_tmp waits
// for its card, the policy approves it for this scenario, the core runs the canary on the first
// machine and waits at the manual gate, the policy presses Continue, and the item of a machine
// without the pipeline plugin fails (DLV-16: no executor), which stops the campaign at its
// threshold of no failures (max_failures_max 0).
func campaignActionCanary(ctx context.Context, d *Director, sc string) error {
	t := campaignTenant(d.S.Farm())
	bob, dm := d.S.PersonIn(t.Name, "bob"), d.S.PersonIn(t.Name, "bob-dm")
	dbs := hostsOf(t, KindDB)
	// The action is proposed when the volume runs out of space and the code's numbers confirm
	// it (its description): the data volumes have filled over the last hour, still above the
	// alarm, and come back after. The person writes once the monitor shows it.
	if err := d.world("the pipeline temporary files have filled /data of the database machines to 18% free over the last hour (the director's fault, above the alarm)", func(w *World) {
		now := time.Now().UTC()
		for _, db := range dbs {
			if disk := w.Hosts[db].Disks["/data"]; disk != nil {
				disk.Fill(0.18)
				w.Ramp(seriesKey(db, diskItem("/data", "pused")), now.Add(-time.Hour), now, 100*(1-disk.FreeRatio))
				w.Ramp(seriesKey(db, diskItem("/data", "pfree")), now.Add(-time.Hour), now, 100*disk.FreeRatio)
			}
		}
	}); err != nil {
		return err
	}
	defer func() {
		_ = d.world("the operators clean up what is left on the database machines", func(w *World) {
			for _, db := range dbs {
				healDisk(w, db, "")
			}
		})
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-d.S.stopped:
		return errStopped
	case <-time.After(2 * d.S.cfg.Step):
	}
	key, err := d.S.Say(bob, dm, "/rollout The data volumes of the database machines are filling up: clean up the pipeline temporary files older than 7 days there.")
	if err != nil {
		return err
	}
	id, err := d.campaignOf(ctx, t.Name, key)
	d.check(sc, "campaign_proposed", err == nil, "campaign %s %v", id, err)
	if err != nil {
		return nil
	}
	var kind, action string
	var card bool
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT record->'template'->>'kind', coalesce(record->'template'->>'action', ''), card FROM campaigns WHERE id = $1`, id).
		Scan(&kind, &action, &card)
	c, err := d.readCampaign(ctx, id)
	d.check(sc, "waits_for_card", err == nil && kind == "action" && action == "cleanup_pipeline_tmp" && card && c.state == "proposed" && len(c.resources) == len(dbs),
		"template %s %s, card %v, state %s, items %v over %v %v", kind, action, card, c.state, c.items, c.resources, err)
	// The policy approves the card of this scenario's campaign, and its Continue once the check
	// has seen the gate.
	d.allow(id)
	c, err = d.waitCampaign(ctx, id, d.Timeout, "paused", "completed", "failed", "canceled", "rejected")
	d.check(sc, "canary_then_gate", err == nil && c.state == "paused" && c.pause == "gate" && c.items["ok"] == 1,
		"state %s, pause %q, items %v %v", c.state, c.pause, c.items, err)
	if err != nil || c.state != "paused" {
		return nil
	}
	d.allow(id + ":gate")
	c, err = d.waitCampaign(ctx, id, d.Timeout, "completed", "failed", "canceled")
	d.check(sc, "threshold_stop", err == nil && c.state == "failed" && c.items["ok"] == 1 && c.items["failed"] >= 1 && strings.Contains(c.stop, "threshold"),
		"state %s, stop %q, items %v %v", c.state, c.stop, c.items, err)
	// One write call per item, under the campaign as its approval (I-4 reads the rest).
	var calls int
	_ = d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE campaign_item IS NOT NULL AND approval_id = $1`, id).Scan(&calls)
	d.check(sc, "one_call_per_item", calls >= 1 && calls <= len(dbs), "%d write calls under the campaign for %d machines", calls, len(dbs))
	return nil
}

// campaignTenant is the first tenant with the operator and two database machines.
func campaignTenant(f *Farm) *FarmTenant {
	for _, t := range f.Tenants {
		if t.Workers("ops-operator") > 0 && len(hostsOf(t, KindDB)) >= 2 {
			return t
		}
	}
	return nil
}

// needsCampaignTenant is the need of campaignActionCanary.
func needsCampaignTenant(f *Farm) string {
	if campaignTenant(f) == nil {
		return "no tenant runs ops-operator on two database machines"
	}
	return ""
}
