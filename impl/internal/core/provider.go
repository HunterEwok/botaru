// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// The provider pairs (tenant workspace, model pool) of P4 (GW-10, GW-11, GW-13). P4 asks the
// gate before a provider call and reports the outcome after it; the state lives in the store,
// so the dispatcher sees it too and hands out no attempt of a pair that waits (dispatch
// conditions D18 and D19), and the runtime is not started for it.

const (
	// breakerFailures is the series of provider failures that opens the circuit.
	breakerFailures = 3
	// The pause of an open circuit starts at breakerPause and doubles after a failed probe up
	// to breakerPauseMax.
	breakerPause    = 30 * time.Second
	breakerPauseMax = 10 * time.Minute
	// probeLease is how long one probe call holds the half-open circuit.
	probeLease = 5 * time.Minute
	// rateWait is the wait of a pair when the provider asks to wait without saying how long.
	rateWait = 10 * time.Second
)

// pairPool is the model pool of a profile: its pool, else the profile name.
func pairPool(name string, p manifest.ModelProfile) string {
	if p.Pool != "" {
		return p.Pool
	}
	return name
}

// alarmID turns a name into an alarm subject id.
func alarmID(s string) string {
	s = strings.ToLower(s)
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("_.:-", r) {
			return r
		}
		return '-'
	}, s)
}

// ProviderGate is asked by P4 before a provider call: an open circuit answers
// provider_unavailable right away, a pair that waits for the rate limit answers
// provider_rate_limited with the time left, a pair with a configuration error answers
// provider_config until an apply. After the pause of an open circuit one call becomes the
// probe (half-open).
func (c *Core) ProviderGate(ctx context.Context, g *ModelGrant) error {
	_, err := c.ProviderGateProbe(ctx, g)
	return err
}

// ProviderGateProbe is ProviderGate that also returns the probe lease when this call took the
// probe of an open circuit, or nil; only that call gives the probe back (GW-11).
func (c *Core) ProviderGateProbe(ctx context.Context, g *ModelGrant) (*time.Time, error) {
	pool := pairPool(g.ProfileName, g.Profile)
	var probe *time.Time
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		probe = nil
		var state string
		var until *time.Time
		err := tx.QueryRow(ctx, `SELECT state, until FROM provider_pairs WHERE tenant = $1 AND pool = $2 FOR UPDATE`, g.Tenant, pool).Scan(&state, &until)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		left := 0
		if until != nil && until.After(now) {
			left = int(until.Sub(now)/time.Second) + 1
		}
		switch state {
		case "config":
			return refuse("provider_config", "the provider configuration of the pair waits for a fix")
		case "rate":
			if left > 0 {
				p := refuse("provider_rate_limited", "the pair waits for the provider rate limit")
				p.RetryAfterS = left
				return p
			}
		case "open", "probing":
			if left > 0 {
				p := refuse("provider_unavailable", "the circuit of the pair is open")
				p.RetryAfterS = left
				return p
			}
			until := now.Add(probeLease).Truncate(time.Microsecond)
			if _, err := tx.Exec(ctx, `UPDATE provider_pairs SET state = 'probing', until = $3, updated_at = $4 WHERE tenant = $1 AND pool = $2`,
				g.Tenant, pool, until, now); err != nil {
				return err
			}
			probe = &until
		}
		return nil
	})
	return probe, err
}

// ProviderProbeAbandoned reopens the circuit of a pair whose probe ended before the provider
// was called (a refused reserve, a tools mismatch): the next call probes at once instead of
// waiting out the probe lease, since the provider was never tried (GW-11).
func (c *Core) ProviderProbeAbandoned(ctx context.Context, g *ModelGrant, probe time.Time) error {
	pool := pairPool(g.ProfileName, g.Profile)
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `UPDATE provider_pairs SET state = 'open', until = $3, updated_at = $3 WHERE tenant = $1 AND pool = $2 AND state = 'probing' AND until = $4`,
			g.Tenant, pool, now, probe)
		return err
	})
}

// ProviderOutcome records the outcome of a provider call: "" is a success and closes the
// circuit; provider_unavailable counts toward the series that opens it, and a failed probe
// opens it again with a doubled pause; provider_rate_limited makes the pair wait; a
// configuration error opens the circuit until an apply and raises alarm.provider.config; a
// spend ceiling raises alarm.provider.spend_cap (GW-9, GW-10, GW-11, GW-13, CAP-2).
func (c *Core) ProviderOutcome(ctx context.Context, g *ModelGrant, reason string, retryAfterS int) error {
	pool := pairPool(g.ProfileName, g.Profile)
	subject := AlarmSubject{Kind: "provider", ID: alarmID(g.Profile.Provider + ":" + pool)}
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var state string
		var failures, pause int
		err := tx.QueryRow(ctx, `SELECT state, failures, pause_s FROM provider_pairs WHERE tenant = $1 AND pool = $2 FOR UPDATE`, g.Tenant, pool).
			Scan(&state, &failures, &pause)
		if errors.Is(err, pgx.ErrNoRows) {
			state, err = "closed", nil
		}
		if err != nil {
			return err
		}
		set := func(state string, failures, pause int, until *time.Time) error {
			_, err := tx.Exec(ctx, `INSERT INTO provider_pairs (tenant, pool, state, failures, pause_s, until, updated_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (tenant, pool) DO UPDATE SET state = $3, failures = $4, pause_s = $5, until = $6, updated_at = $7`,
				g.Tenant, pool, state, failures, pause, until, now)
			return err
		}
		switch reason {
		case "":
			// An apply removes the pair of a configuration error; the alarm resolves with the
			// first good call after it.
			if state != "closed" || failures > 0 {
				if _, err := tx.Exec(ctx, `DELETE FROM provider_pairs WHERE tenant = $1 AND pool = $2`, g.Tenant, pool); err != nil {
					return err
				}
				// The tasks that waited for the provider no longer do; the dispatcher marks
				// again those whose pair still waits (BUD-4).
				if _, err := tx.Exec(ctx, `UPDATE tasks SET reason = NULL, updated_at = $2 WHERE tenant = $1 AND state = 'submitted' AND reason IN ('provider_unavailable', 'provider_quota')`,
					g.Tenant, now); err != nil {
					return err
				}
			}
			return c.resolveOpen(ctx, tx, now, Alarm{Tenant: g.Tenant, Name: "provider.config", Subject: subject})
		case "provider_unavailable":
			// A call that started before the circuit opened and fails late does not shorten
			// the pause of the open circuit (GW-11).
			if state == "config" || state == "open" {
				return nil
			}
			failures++
			if state != "probing" && failures < breakerFailures {
				return set("closed", failures, pause, nil)
			}
			next := breakerPause
			if state == "probing" {
				next = min(2*time.Duration(pause)*time.Second, breakerPauseMax)
			}
			until := now.Add(next)
			return set("open", failures, int(next/time.Second), &until)
		case "provider_rate_limited", "provider_quota":
			if state == "config" {
				return nil
			}
			wait := rateWait
			if retryAfterS > 0 {
				wait = time.Duration(retryAfterS) * time.Second
			}
			until := now.Add(wait)
			return set("rate", 0, 0, &until)
		case "provider_config":
			if err := set("config", 0, 0, nil); err != nil {
				return err
			}
			return c.fireAlarm(ctx, tx, now, Alarm{Tenant: g.Tenant, Name: "provider.config", Subject: subject, Severity: "page",
				Values: map[string]any{"reason": "provider_config"}})
		case "provider_spend_cap":
			return c.fireAlarm(ctx, tx, now, Alarm{Tenant: g.Tenant, Name: "provider.spend_cap", Subject: subject, Severity: "page",
				Values: map[string]any{"reason": "provider_spend_cap"}})
		}
		return nil
	})
}

// resolveOpen resolves an alarm only when an episode of it is open.
func (c *Core) resolveOpen(ctx context.Context, tx pgx.Tx, now time.Time, a Alarm) error {
	var open bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alarm_episodes WHERE tenant = $1 AND alarmkey = $2 AND state = 'firing')`,
		a.Tenant, a.key()).Scan(&open); err != nil || !open {
		return err
	}
	return c.resolveAlarm(ctx, tx, now, a)
}

// pairWait is the waiting reason of the pair a role's model needs, or "": provider_unavailable
// while its circuit is open or waits for a fix, provider_quota while it waits for the rate
// limit (dispatch conditions D18, D19). A circuit whose pause ended does not wait: one of its
// tasks makes the probe.
func pairWait(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tenant string, role manifest.Role) (string, error) {
	_, name := mainSlot(role)
	if name == "" {
		return "", nil
	}
	p, ok := cat.Profile(tenant, name)
	if !ok {
		return "", nil
	}
	var state string
	var until *time.Time
	err := tx.QueryRow(ctx, `SELECT state, until FROM provider_pairs WHERE tenant = $1 AND pool = $2`, tenant, pairPool(name, p)).Scan(&state, &until)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	waiting := until != nil && until.After(now)
	switch {
	case state == "config", (state == "open" || state == "probing") && waiting:
		return "provider_unavailable", nil
	case state == "rate" && waiting:
		return "provider_quota", nil
	}
	return "", nil
}

// waitingReasons are the waiting reasons the dispatcher writes on a submitted task; another
// reason on it (waiting_on_fix) is kept.
var waitingReasons = []string{"provider_unavailable", "provider_quota", "type_quota", "seat_eval_missing"}

// markWaiting writes the waiting reason on the submitted tasks of types, or on ids.
func markWaiting(ctx context.Context, tx pgx.Tx, now time.Time, tenant string, types, ids []string, reason string) error {
	if types == nil {
		types = []string{}
	}
	if ids == nil {
		ids = []string{}
	}
	_, err := tx.Exec(ctx, `UPDATE tasks SET reason = $4, updated_at = $5 WHERE tenant = $1 AND state = 'submitted'
		AND (type = ANY($2) OR id = ANY($3)) AND (reason IS NULL OR reason = ANY($6)) AND reason IS DISTINCT FROM $4`,
		tenant, types, ids, reason, now, waitingReasons)
	return err
}

func hasRunningCeiling(d manifest.TenantDispatch) bool {
	for _, r := range d.Rules {
		if r.MaxRunning > 0 {
			return true
		}
	}
	return false
}

// ruleMatches says whether a dispatch rule matches a task by its type and the source kind of
// its fairness key (DSP-2, DSP-3).
func ruleMatches(r manifest.DispatchRule, typ, fairness string) bool {
	if r.Match.Type != "" && r.Match.Type != typ {
		return false
	}
	if r.Match.TypePrefix != "" && !strings.HasPrefix(typ, r.Match.TypePrefix) {
		return false
	}
	return r.Match.Source == "" || strings.HasPrefix(fairness, r.Match.Source+":")
}

// runningCeilingReached says whether a rule that matches the task already has max_running of
// its tasks in working (DSP-7).
func runningCeilingReached(ctx context.Context, tx pgx.Tx, tenant string, d manifest.TenantDispatch, typ string, rec []byte) (bool, error) {
	var r struct {
		Fairness string `json:"fairness_key"`
	}
	_ = json.Unmarshal(rec, &r)
	for _, rule := range d.Rules {
		if rule.MaxRunning <= 0 || !ruleMatches(rule, typ, r.Fairness) {
			continue
		}
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM tasks WHERE tenant = $1 AND state = 'working'
			AND ($2 = '' OR type = $2) AND ($3 = '' OR starts_with(type, $3)) AND ($4 = '' OR starts_with(record->>'fairness_key', $4 || ':'))`,
			tenant, rule.Match.Type, rule.Match.TypePrefix, rule.Match.Source).Scan(&n); err != nil {
			return false, err
		}
		if n >= rule.MaxRunning {
			return true, nil
		}
	}
	return false, nil
}
