// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// HealthReport is p2-health-response: only numbers, no tenant data (MON-2).
type HealthReport struct {
	Alive             bool           `json:"alive"`
	Reason            string         `json:"reason,omitempty"`
	StoreFreeBytes    *int64         `json:"store_free_bytes,omitempty"`
	StoreFreeRatio    *float64       `json:"store_free_ratio,omitempty"`
	OldestUrgentAgeS  *int64         `json:"oldest_urgent_age_s,omitempty"`
	Delivered10mRatio *float64       `json:"delivered_10m_ratio,omitempty"`
	LoopTickAgeS      map[string]int `json:"loop_tick_age_s,omitempty"`
}

// HealthConfig holds the thresholds of the deployment binding for the health indicators and
// the store headroom (OBS-9, CAP-5).
type HealthConfig struct {
	// UrgentAgeS is the threshold of the age of the oldest class 1-2 task in submitted.
	UrgentAgeS int `yaml:"urgent_age_s"`
	// DeliveryWindowS is the window of the delivered within 10 minutes share.
	DeliveryWindowS int `yaml:"delivery_window_s"`
	// DeliveryShareMin is the threshold of that share.
	DeliveryShareMin float64 `yaml:"delivery_share_min"`
	// LoopTickS is the threshold of the tick age of a loop.
	LoopTickS int `yaml:"loop_tick_s"`
	// StoreFreeSoft and StoreFreeHard are the shares of free space of the store volume below
	// which enqueues of classes 3-5 are refused, and below which P1 answers not_ready.
	StoreFreeSoft float64 `yaml:"store_free_soft"`
	StoreFreeHard float64 `yaml:"store_free_hard"`
	// StoreVolume is a path on the store volume; the core measures its free space when the
	// store runs on the core machine (D-88). Empty: no measurement.
	StoreVolume string `yaml:"store_volume"`
}

func (h *HealthConfig) defaults() {
	if h.UrgentAgeS == 0 {
		h.UrgentAgeS = 600
	}
	if h.DeliveryWindowS == 0 {
		h.DeliveryWindowS = 3600
	}
	if h.DeliveryShareMin == 0 {
		h.DeliveryShareMin = 0.9
	}
	if h.LoopTickS == 0 {
		h.LoopTickS = 30
	}
	if h.StoreFreeSoft == 0 {
		h.StoreFreeSoft = 0.15
	}
	if h.StoreFreeHard == 0 {
		h.StoreFreeHard = 0.05
	}
}

// The capacity states of the store volume (CAP-5).
const (
	capacityOK int32 = iota
	capacitySoft
	capacityHard
)

// freeSpace measures the store volume: by Config.FreeSpace when set (a mock measurement on a
// test bench), else by the path of the binding. ok is false without a measurement.
func (c *Core) freeSpace() (free, total uint64, ok bool) {
	measure := c.cfg.FreeSpace
	if measure == nil {
		if c.cfg.Health.StoreVolume == "" {
			return 0, 0, false
		}
		measure = func() (uint64, uint64, error) { return volumeSpace(c.cfg.Health.StoreVolume) }
	}
	free, total, err := measure()
	if err != nil || total == 0 {
		if err != nil {
			c.log.Warn("measuring the store volume failed", "err", err)
		}
		return 0, 0, false
	}
	return free, total, true
}

// measureCapacity measures the store volume and keeps the capacity state that enqueue and P1
// read; without a measurement the state is ok.
func (c *Core) measureCapacity() (free, total uint64, ok bool) {
	free, total, ok = c.freeSpace()
	state := capacityOK
	if ok {
		ratio := float64(free) / float64(total)
		switch {
		case ratio < c.cfg.Health.StoreFreeHard:
			state = capacityHard
		case ratio < c.cfg.Health.StoreFreeSoft:
			state = capacitySoft
		}
	}
	c.capacity.Store(state)
	return free, total, ok
}

// P1Ready refuses a P1 operation with not_ready while the core waits for the store and its
// loops after the store was unavailable (OBS-8), and below the hard threshold of the store
// volume except heartbeat, complete, fail and release (CAP-5).
func (c *Core) P1Ready(op string) error {
	if c.storeDown.Load() {
		p := refuse("not_ready", "the store is unavailable")
		p.RetryAfterS = 5
		return p
	}
	if c.capacity.Load() == capacityHard {
		switch op {
		case "heartbeat", "complete", "fail", "release":
		default:
			p := refuse("not_ready", "the store volume is below its hard threshold")
			p.RetryAfterS = 60
			return p
		}
	}
	return nil
}

// indicators are the three health indicators of OBS-9 at now.
type indicators struct {
	urgentAge *int64
	share     *float64
	ticks     map[string]int
}

func (c *Core) indicators(ctx context.Context, tx pgx.Tx, now time.Time) (*indicators, error) {
	in := &indicators{ticks: map[string]int{}}
	var oldest *time.Time
	if err := tx.QueryRow(ctx, `SELECT min(created_at) FROM tasks WHERE state = 'submitted' AND priority_class <= 2`).Scan(&oldest); err != nil {
		return nil, err
	}
	if oldest != nil {
		age := max(int64(now.Sub(*oldest)/time.Second), 0)
		in.urgentAge = &age
	}
	// The deliveries that had their 10 minutes within the window; a withheld delivery is a
	// decision of the policy, not a stuck farm, and is not counted.
	var all, onTime int64
	if err := tx.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE state = 'delivered' AND updated_at <= created_at + interval '10 minutes')
		FROM deliveries WHERE created_at > $1 AND created_at <= $2 AND state <> 'withheld'`,
		now.Add(-time.Duration(c.cfg.Health.DeliveryWindowS)*time.Second-10*time.Minute), now.Add(-10*time.Minute)).Scan(&all, &onTime); err != nil {
		return nil, err
	}
	if all > 0 {
		share := float64(onTime) / float64(all)
		in.share = &share
	}
	rows, err := tx.Query(ctx, `SELECT loop, ticked_at FROM loop_ticks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var loop string
		var at time.Time
		if err := rows.Scan(&loop, &at); err != nil {
			return nil, err
		}
		in.ticks[loop] = max(int(now.Sub(at)/time.Second), 0)
	}
	return in, rows.Err()
}

// markTick records the tick of a loop in the store (OBS-4).
func (c *Core) markTick(ctx context.Context, loop string) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		_, err := tx.Exec(ctx, `INSERT INTO loop_ticks (loop, ticked_at) VALUES ($1, $2)
			ON CONFLICT (loop) DO UPDATE SET ticked_at = greatest(loop_ticks.ticked_at, excluded.ticked_at)`, loop, now)
		return err
	})
}

// healthLoop computes the indicators and raises or resolves their alarms in the farm tenant:
// alarm.health.urgent_age, alarm.health.delivery_share, alarm.health.loop_tick per loop
// (OBS-9) and alarm.store.free.low below the soft threshold of the store volume (CAP-5). A
// core that ticks its loops only by loop.tick on a test bench raises no loop tick alarms.
func (c *Core) healthLoop(ctx context.Context) error {
	free, total, measured := c.measureCapacity()
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		in, err := c.indicators(ctx, tx, now)
		if err != nil {
			return err
		}
		h := c.cfg.Health
		set := func(a Alarm, on bool) error {
			if on {
				return c.fireAlarm(ctx, tx, now, a)
			}
			var open bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM alarm_episodes WHERE tenant = $1 AND alarmkey = $2 AND state = 'firing')`,
				a.Tenant, a.key()).Scan(&open); err != nil || !open {
				return err
			}
			return c.resolveAlarm(ctx, tx, now, a)
		}
		urgent := Alarm{Tenant: "farm", Name: "health.urgent_age", Subject: AlarmSubject{Kind: "store", ID: "tasks"}, Severity: "warn"}
		if in.urgentAge != nil {
			urgent.Values = map[string]any{"age_s": *in.urgentAge, "threshold_s": int64(h.UrgentAgeS)}
		}
		if err := set(urgent, in.urgentAge != nil && *in.urgentAge > int64(h.UrgentAgeS)); err != nil {
			return err
		}
		share := Alarm{Tenant: "farm", Name: "health.delivery_share", Subject: AlarmSubject{Kind: "store", ID: "deliveries"}, Severity: "warn"}
		if in.share != nil {
			share.Values = map[string]any{"share": *in.share, "threshold": h.DeliveryShareMin}
		}
		if err := set(share, in.share != nil && *in.share < h.DeliveryShareMin); err != nil {
			return err
		}
		if !c.manual.Load() {
			for _, loop := range loopOrder {
				// This loop's own tick is marked after this pass; a stuck health loop shows
				// only in the health answer.
				if loop == "health" {
					continue
				}
				age, ok := in.ticks[loop]
				a := Alarm{Tenant: "farm", Name: "health.loop_tick", Subject: AlarmSubject{Kind: "loop", ID: loop}, Severity: "page",
					Values: map[string]any{"age_s": age, "threshold_s": h.LoopTickS}}
				if err := set(a, ok && age > h.LoopTickS); err != nil {
					return err
				}
			}
		}
		low := Alarm{Tenant: "farm", Name: "store.free.low", Subject: AlarmSubject{Kind: "store", ID: "c1"}, Severity: "page"}
		if measured {
			low.Values = map[string]any{"free_bytes": int64(free), "free_ratio": float64(free) / float64(total), "soft": h.StoreFreeSoft, "hard": h.StoreFreeHard}
		}
		return set(low, measured && c.capacity.Load() != capacityOK)
	})
}

// Health answers also when the store is unavailable: alive only when the store answers, the
// core is not waiting for its loops after the store returned, the store volume is above the
// hard threshold and every loop ticked in time (OBS-4, OBS-8, OBS-9, CAP-5).
func (c *Core) Health(ctx context.Context) HealthReport {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	r := HealthReport{Alive: true}
	if free, total, ok := c.measureCapacity(); ok {
		fb, fr := int64(free), float64(free)/float64(total)
		r.StoreFreeBytes, r.StoreFreeRatio = &fb, &fr
	}
	if err := c.st.Ping(ctx); err != nil {
		r.Alive, r.Reason = false, "store_unavailable"
		return r
	}
	var in *indicators
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		var err error
		in, err = c.indicators(ctx, tx, now)
		return err
	})
	if err != nil {
		r.Alive, r.Reason = false, "store_unavailable"
		return r
	}
	r.OldestUrgentAgeS, r.Delivered10mRatio = in.urgentAge, in.share
	if len(in.ticks) > 0 {
		r.LoopTickAgeS = in.ticks
	}
	switch {
	case c.storeDown.Load() || c.capacity.Load() == capacityHard:
		r.Alive, r.Reason = false, "not_ready"
	default:
		for _, loop := range loopOrder {
			if age, ok := in.ticks[loop]; ok && age > c.cfg.Health.LoopTickS {
				r.Alive, r.Reason = false, "loop_stale"
				break
			}
		}
	}
	return r
}

// AuditVerifyResult is p2-audit-verify-response.
type AuditVerifyResult struct {
	Tenant       string `json:"tenant"`
	OK           bool   `json:"ok"`
	CheckedToSeq int64  `json:"checked_to_seq"`
	BreakAtSeq   int64  `json:"break_at_seq,omitempty"`
}

// AuditVerify recomputes the audit chain of a tenant (AUD-6).
func (c *Core) AuditVerify(ctx context.Context, tenant string) (*AuditVerifyResult, error) {
	n, broken, err := c.VerifyAudit(ctx, tenant)
	if err != nil {
		return nil, err
	}
	return &AuditVerifyResult{Tenant: tenant, OK: broken == 0, CheckedToSeq: n, BreakAtSeq: broken}, nil
}
