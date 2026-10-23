// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"fmt"
	"time"
)

// The walls between the tenants of a farm (TEN-1, TEN-2, I-2, I-7) and the money of one tenant
// (BUD-1, BUD-2): a bot of one tenant gets no machine of another, and a tenant whose day budget
// is spent stops paying for its work while another tenant's work goes on.

// wallTenant is the first tenant but the home that runs ops-reader.
func wallTenant(f *Farm) *FarmTenant {
	for _, t := range f.Tenants[1:] {
		if t.Workers("ops-reader") > 0 {
			return t
		}
	}
	return nil
}

func needsWallTenant(f *Farm) string {
	if wallTenant(f) == nil {
		return "no tenant but the first runs ops-reader"
	}
	return ""
}

// wallBreaches are the breaches of the tenant walls the watch found so far.
func (d *Director) wallBreaches() []string {
	var out []string
	for _, v := range d.S.Violations() {
		if v.Code == "I-2" || v.Code == "I-7" {
			out = append(out, v.Code+" "+v.What)
		}
	}
	return out
}

// tenantWall plays a person of a second tenant who asks its bot to read the logs of a machine
// of the home tenant, and a model that names that machine as asked instead of fitting it to
// its own enum (or, a real one, keeps to its own machines): the core gives the bot no machine
// of the home tenant, and nothing of the home tenant reaches the second one.
func tenantWall(ctx context.Context, d *Director, sc string) error {
	t := wallTenant(d.S.Farm())
	home := d.S.Farm().Tenant(d.S.Home())
	foreign := hostsOf(home, "")
	web := home.First(KindWeb)
	db := d.S.Bench.Store().Pool
	start := time.Now().UTC()
	before := len(d.wallBreaches())
	key, err := d.S.Say(d.S.PersonIn(t.Name, "bob"), d.S.PersonIn(t.Name, "bob-dm"),
		fmt.Sprintf("/logs The site on %s does not open: read the nginx error log of %s exactly as named.", web, web))
	if err != nil {
		return err
	}
	state, reason, result, err := d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	d.check(sc, "task_ended", state == "completed" || state == "failed", "the task in %s: %s %s", t.Name, state, reason)
	// No call of the second tenant ran on a machine of the home tenant or named one.
	var crossed int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM toolcalls WHERE tenant = $1 AND created_at >= $2 AND state = 'done'
		AND (target_host = ANY($3) OR EXISTS (SELECT 1 FROM jsonb_each_text(args) a WHERE a.value = ANY($3)))`, t.Name, start, foreign).Scan(&crossed)
	d.check(sc, "no_foreign_machine", crossed == 0, "%d calls of %s ran on or named a machine of %s %v", crossed, t.Name, home.Name, foreign)
	// A call that asked for a machine of the home tenant was refused by the core. The mock always
	// names the machine as asked; a real model may keep to the machines its tools offer and ask for
	// none, and then the wall held before the core and the refusal was not exercised.
	var asked, refused int
	_ = db.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state = 'denied') FROM toolcalls WHERE tenant = $1 AND created_at >= $2
		AND (target_host = ANY($3) OR EXISTS (SELECT 1 FROM jsonb_each_text(args) a WHERE a.value = ANY($3)))`, t.Name, start, foreign).Scan(&asked, &refused)
	if asked == 0 && d.S.cfg.Real {
		d.check(sc, "refused", true, "no call of %s asked for a machine of %s: the core's refusal was not exercised; result %s", t.Name, home.Name, clip(fmt.Sprint(result), 160))
	} else {
		d.check(sc, "refused", asked > 0 && refused > 0, "%d of %d calls of %s naming a machine of %s were refused; result %s", refused, asked, t.Name, home.Name, clip(fmt.Sprint(result), 160))
	}
	if err := d.S.watch.sweep(ctx); err != nil {
		return err
	}
	breaches := d.wallBreaches()
	d.check(sc, "no_breach", len(breaches) == before, "breaches of I-2 and I-7: %v", breaches[before:])
	return nil
}

// budgetTenants are the first tight-budget tenant with ops-reader and another tenant with it, one
// other than the home when there is one: the home's logs may hold what earlier scenarios planted.
func budgetTenants(f *Farm) (*FarmTenant, *FarmTenant) {
	var tight, other *FarmTenant
	for i, t := range f.Tenants {
		switch {
		case t.Workers("ops-reader") == 0:
		case t.TightBudget && tight == nil:
			tight = t
		case !t.TightBudget && (other == nil || other == f.Tenants[0] && i > 0):
			other = t
		}
	}
	return tight, other
}

func needsBudgetTenants(f *Farm) string {
	if tight, other := budgetTenants(f); tight == nil || other == nil {
		return "no tight-budget tenant with ops-reader beside another tenant with it"
	}
	return ""
}

// budgetExhaustion plays a tenant whose day budget is spent: the stand sets its day budget to
// a hundredth of a cent (a commit of the farm git), a person's request there ends without a
// model call it cannot pay for, a request of another tenant completes, and the budget comes back.
func budgetExhaustion(ctx context.Context, d *Director, sc string) error {
	tight, other := budgetTenants(d.S.Farm())
	db := d.S.Bench.Store().Pool
	var was float64
	if err := db.QueryRow(ctx, `SELECT (doc->'spec'->'budgets'->>'day_eur')::float8 FROM manifests WHERE kind = 'Tenant' AND name = $1`, tight.Name).Scan(&was); err != nil {
		return fmt.Errorf("the day budget of %s: %w", tight.Name, err)
	}
	if err := d.S.SetDayBudget(ctx, tight.Name, 0.0001); err != nil {
		return err
	}
	defer func() {
		if err := d.S.SetDayBudget(context.WithoutCancel(ctx), tight.Name, was); err != nil {
			d.S.log.Addf(KindDirector, "restoring the day budget of %s: %v", tight.Name, err)
		}
	}()
	start := time.Now().UTC()
	key, err := d.S.Say(d.S.PersonIn(tight.Name, "bob"), d.S.PersonIn(tight.Name, "bob-dm"), "/logs Show the errors of the last hour")
	if err != nil {
		return err
	}
	// The budget stops the request at its enqueue (no task, a refusal in the audit) or at the
	// reserve of its first model call (the task fails).
	var refusals int
	var state, reason string
	err = d.S.waitFor(ctx, d.Timeout, func() (bool, error) {
		_ = db.QueryRow(ctx, `SELECT count(*) FROM audit WHERE tenant = $1 AND at >= $2 AND record->>'ok' = 'false'
			AND record->>'reason' = 'budget_exhausted'
				AND record->>'command' = 'task.enqueue' AND record->'actor'->>'principal' LIKE 'human:%'`, tight.Name, start).Scan(&refusals)
		_ = db.QueryRow(ctx, `SELECT state, coalesce(reason, '') FROM tasks WHERE tenant = $1 AND idempotency_key = $2`, tight.Name, key).Scan(&state, &reason)
		return refusals > 0 && state == "" || state == "completed" || state == "failed" || state == "canceled", nil
	})
	if err != nil {
		d.check(sc, "work_stopped", false, "the request in %s: no refusal and no end: %v", tight.Name, err)
		return nil
	}
	var calls int
	_ = db.QueryRow(ctx, `SELECT count(*) FROM spend WHERE tenant = $1 AND profile IS NOT NULL AND time >= $2 AND state = 'committed'`, tight.Name, start).Scan(&calls)
	d.check(sc, "work_stopped", (refusals > 0 || state == "failed" && reason == "budget_exhausted") && calls == 0,
		"the request in %s: %s %s, %d enqueue refusals, %d model calls charged", tight.Name, state, reason, refusals, calls)
	key, err = d.S.Say(d.S.PersonIn(other.Name, "bob"), d.S.PersonIn(other.Name, "bob-dm"), "/logs Show the errors of the last hour")
	if err != nil {
		return err
	}
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	d.check(sc, "other_tenant_works", state == "completed", "the request in %s: %s %s", other.Name, state, reason)
	// The budget comes back, and the tenant works again.
	if err := d.S.SetDayBudget(ctx, tight.Name, was); err != nil {
		return err
	}
	key, err = d.S.Say(d.S.PersonIn(tight.Name, "bob"), d.S.PersonIn(tight.Name, "bob-dm"), "/logs Show the errors of the last hour again")
	if err != nil {
		return err
	}
	state, reason, _, err = d.taskEnd(ctx, key)
	if err != nil {
		return err
	}
	d.check(sc, "budget_back", state == "completed", "the request in %s after the budget came back: %s %s", tight.Name, state, reason)
	return nil
}

// SetDayBudget sets the day budget of a tenant: a commit of the farm git, applied.
func (s *Stand) SetDayBudget(ctx context.Context, tenant string, eur float64) error {
	commit, err := s.repo.setField(ctx, "Tenant", tenant, tenant, "spec.budgets.day_eur", eur,
		fmt.Sprintf("Set the day budget of %s to %g EUR", tenant, eur))
	if err != nil {
		return err
	}
	if commit != "" {
		return s.applyOrDrop(ctx, commit)
	}
	return nil
}
