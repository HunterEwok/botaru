// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"sync/atomic"
	"time"
)

// HealthReport is p2-health-response: no tenant data (MON-2).
type HealthReport struct {
	Alive        bool           `json:"alive"`
	Reason       string         `json:"reason,omitempty"`
	LoopTickAgeS map[string]int `json:"loop_tick_age_s,omitempty"`
}

var lastTick atomic.Int64

func markTick() { lastTick.Store(time.Now().UnixNano()) }

// Health answers also when the store is unavailable.
func (c *Core) Health(ctx context.Context) HealthReport {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if err := c.st.Ping(ctx); err != nil {
		return HealthReport{Alive: false, Reason: "store_unavailable"}
	}
	r := HealthReport{Alive: true}
	if t := lastTick.Load(); t != 0 {
		age := int(time.Since(time.Unix(0, t)) / time.Second)
		r.LoopTickAgeS = map[string]int{}
		for _, loop := range loopOrder {
			r.LoopTickAgeS[loop] = age
		}
		if age > 30 {
			r.Alive, r.Reason = false, "loop_stale"
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
