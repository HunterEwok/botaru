// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// The farm manager's work outside the home tenant (R-17, R-18, MGR-20, MGR-53) and the notice of
// an incident in a tenant without a manager (MGR-6). Both play a full data volume on the first
// database machine of their tenant, so they keep off db1 of incident-and-commission.

// managerTenant is the first tenant other than the home that runs the farm manager, its reader
// and the pipeline reader on a database machine.
func managerTenant(f *Farm) *FarmTenant {
	for _, t := range f.Tenants[1:] {
		if t.Manager && t.Workers("farm-manager") > 0 && t.Workers("farm-manager-reader") > 0 && t.Workers("pipeline-reader") > 0 && t.First(KindDB) != "" {
			return t
		}
	}
	return nil
}

// noManagerTenant is the first tenant without the farm manager that has a database machine.
func noManagerTenant(f *Farm) *FarmTenant {
	for _, t := range f.Tenants {
		if !t.Manager && t.First(KindDB) != "" {
			return t
		}
	}
	return nil
}

func needsNoManagerTenant(f *Farm) string {
	if noManagerTenant(f) == nil {
		return "every tenant with a database machine runs the farm manager"
	}
	return ""
}

// diskIncident fills the data volume of the tenant's first database machine to 3% free and waits
// for the disk_full incident it opens.
func (d *Director) diskIncident(ctx context.Context, sc, tenant string, start time.Time) (string, error) {
	db := d.S.Farm().Tenant(tenant).First(KindDB)
	key := FaultDiskFilling + ":" + db
	if err := d.world(fmt.Sprintf("the pipeline temporary files fill %s:/data to 3%% free (the director's fault)", db), func(w *World) {
		w.Hosts[db].Disks["/data"].Fill(0.03)
		w.Faults[key] = true
	}); err != nil {
		return "", err
	}
	if _, err := d.waitProblem(ctx, "disk:"+db+":/data"); err != nil {
		return "", fmt.Errorf("the monitoring problem of %s did not open: %w", db, err)
	}
	var incident string
	err := d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT id FROM incidents WHERE tenant = $1 AND class = 'disk_full' AND since >= $2
			ORDER BY since DESC LIMIT 1`, tenant, start).Scan(&incident)
		return e == nil, nil
	})
	d.check(sc, "incident_opened", err == nil, "incident %s in %s on %s", incident, tenant, db)
	if errors.Is(err, context.DeadlineExceeded) {
		err = nil // the check says it; the caller ends the scenario
	}
	return incident, err
}

// healIncident frees the volume of diskIncident and waits for the incident to resolve.
func (d *Director) healIncident(ctx context.Context, sc, tenant, incident string) error {
	db := d.S.Farm().Tenant(tenant).First(KindDB)
	if err := d.world("the operators clean up the pipeline temporary files on "+db, func(w *World) { healDisk(w, db, FaultDiskFilling+":"+db) }); err != nil {
		return err
	}
	var state string
	err := d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := d.S.Bench.Store().Pool.QueryRow(ctx, `SELECT state FROM incidents WHERE id = $1`, incident).Scan(&state)
		return e == nil && (state == "resolved" || state == "closed"), nil
	})
	d.check(sc, "incident_resolved", err == nil, "incident %s %s", incident, state)
	return nil
}

// managerWork plays the farm manager's three kinds of work in a second tenant: a person's
// directive (R-18), the answer to an incident with a reading of the reader role (MGR-20,
// MGR-25), and the analysis after the incident resolved (MGR-53). A farm of one tenant plays the
// directive in it and leaves the incident to incident-and-commission, whose db1 it would reopen.
func managerWork(ctx context.Context, d *Director, sc string) error {
	t := managerTenant(d.S.Farm())
	second := t != nil
	if !second {
		t = d.S.Farm().Tenant(d.S.Home())
	}
	db := d.S.Bench.Store().Pool
	start := time.Now().UTC()
	bob, dm := d.S.PersonIn(t.Name, "bob"), d.S.PersonIn(t.Name, "bob-dm")
	if _, err := d.S.Say(bob, dm, "/manager role pipeline-reader Look at why the last pipeline run failed and tell me."); err != nil {
		return err
	}
	state, reason, result, err := d.typeEnd(ctx, t.Name, "manager.directive.v1", start)
	if err != nil {
		return err
	}
	var via string
	_ = db.QueryRow(ctx, `SELECT coalesce(record->'directive'->>'via', '') FROM tasks WHERE tenant = $1 AND type = 'manager.directive.v1'
		AND created_at >= $2 ORDER BY created_at LIMIT 1`, t.Name, start).Scan(&via)
	d.check(sc, "directive_done", state == "completed" && via == "channel", "state %s %s, via %q: %v", state, reason, via, result)
	if !second {
		d.S.log.Addf(KindDirector, "no second tenant runs the farm manager: the incident part is incident-and-commission's")
		return nil
	}

	incident, err := d.diskIncident(ctx, sc, t.Name, start)
	if err != nil || incident == "" {
		return err
	}
	var mstate, mreason string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE tenant = $1 AND type = 'manager.incident.respond.v1'
			AND idempotency_key LIKE 'system:incident:' || $2 || ':%' ORDER BY created_at LIMIT 1`, t.Name, incident).Scan(&mstate, &mreason)
		return e == nil && (mstate == "completed" || mstate == "failed" || mstate == "canceled"), nil
	})
	d.check(sc, "manager_answered", err == nil && mstate == "completed", "farm manager task %s %s", mstate, mreason)
	rstate, rreason, _, err := d.typeEnd(ctx, t.Name, "manager.inspect.v1", start)
	var findings int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM findings WHERE tenant = $1 AND incident = $2`, t.Name, incident).Scan(&findings)
	d.check(sc, "manager_read", err == nil && rstate == "completed" && findings > 0, "reading %s %s, %d findings %v", rstate, rreason, findings, err)
	if err := d.healIncident(ctx, sc, t.Name, incident); err != nil {
		return err
	}
	var astate, areason string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE tenant = $1 AND type = 'manager.postmortem.v1'
			AND record->>'postmortem' = $2 ORDER BY created_at DESC LIMIT 1`, t.Name, incident).Scan(&astate, &areason)
		return e == nil && (astate == "completed" || astate == "failed" || astate == "canceled"), nil
	})
	d.check(sc, "postmortem", err == nil && astate == "completed", "analysis after the incident %s %s", astate, areason)
	return nil
}

// noManagerNotice plays an incident in a tenant without the farm manager: no manager task
// opens, and the notice of the incident reaches the tenant's admin within 2 minutes (M-9).
func noManagerNotice(ctx context.Context, d *Director, sc string) error {
	t := noManagerTenant(d.S.Farm())
	db := d.S.Bench.Store().Pool
	start := time.Now().UTC()
	incident, err := d.diskIncident(ctx, sc, t.Name, start)
	if err != nil || incident == "" {
		return err
	}
	var opened time.Time
	_ = db.QueryRow(ctx, `SELECT opened_at FROM incidents WHERE id = $1`, incident).Scan(&opened)
	var delivered time.Time
	err = d.S.waitFor(ctx, 2*time.Minute+30*time.Second, func() (bool, error) {
		e := db.QueryRow(ctx, `SELECT min(updated_at) FROM deliveries WHERE tenant = $1 AND kind = 'manager_notice' AND subject_kind = 'incident'
			AND subject_id = $2 AND state = 'delivered' HAVING count(*) > 0`, t.Name, incident).Scan(&delivered)
		return e == nil, nil
	})
	d.check(sc, "notice_delivered", err == nil && delivered.Sub(opened) <= 2*time.Minute,
		"the notice of incident %s was delivered %v after the opening %v", incident, delivered.Sub(opened).Round(time.Second), err)
	var tasks int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND type LIKE 'manager.%' AND created_at >= $2`, t.Name, start).Scan(&tasks)
	d.check(sc, "no_manager_task", tasks == 0, "%d farm manager tasks in %s", tasks, t.Name)
	return d.healIncident(ctx, sc, t.Name, incident)
}
