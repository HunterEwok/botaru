// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// ClaimRequest is p1-claim-request.
type ClaimRequest struct {
	Kinds []string `json:"kinds"`
	WaitS *int     `json:"wait_s,omitempty"`
}

// WorkItem is the work-item schema.
type WorkItem struct {
	WorkID          string         `json:"work_id"`
	Kind            string         `json:"kind"`
	LeaseNo         int            `json:"lease_no"`
	Epoch           int64          `json:"epoch"`
	Tenant          string         `json:"tenant"`
	LeaseUntil      string         `json:"lease_until"`
	LeaseRemainingS int            `json:"lease_remaining_s"`
	Token           LeaseToken     `json:"token"`
	Payload         map[string]any `json:"payload"`
}

// ClaimResult is p1-claim-response.
type ClaimResult struct {
	Work *WorkItem `json:"work"`
}

// maxLeaseS is the ceiling of a type lease (task-type schema, timeouts.lease).
const maxLeaseS = 120

// Claim hands out the next work item with long polling (DSP-1, FEN-1). The core decides what
// to hand out; the worker names only the kinds it takes.
func (c *Core) Claim(ctx context.Context, w *Worker, req ClaimRequest) (*ClaimResult, error) {
	switch w.State {
	case "draining":
		return nil, refuse("draining", "the worker is draining")
	case "blocked":
		return nil, refuse("blocked", "the worker is blocked")
	case "active":
	default:
		return nil, refuse("not_ready", "send hello first")
	}
	wait := 30
	if req.WaitS != nil {
		wait = *req.WaitS
	}
	if wait > 60 {
		wait = 60
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		woken := c.wake.wait()
		item, err := c.claimOnce(ctx, w, req.Kinds)
		if err != nil || item != nil {
			return &ClaimResult{Work: item}, err
		}
		left := time.Until(deadline)
		if left <= 0 {
			return &ClaimResult{}, nil
		}
		// A lost notification costs at most this pause.
		poll := min(left, 2*time.Second)
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-woken:
		case <-t.C:
		}
		t.Stop()
	}
}

func (c *Core) claimOnce(ctx context.Context, w *Worker, kinds []string) (*WorkItem, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var item *WorkItem
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		item = nil
		if _, err := tx.Exec(ctx, `UPDATE workers SET last_seen = $2 WHERE id = $1`, w.ID, now); err != nil {
			return err
		}
		for _, k := range kinds {
			var err error
			switch {
			case k == "delivery" && (w.Kind == "channel" || w.Kind == "sink" || w.Kind == "monitor"):
				item, err = c.claimDelivery(ctx, tx, now, w)
			case k == "attempt" && (w.Kind == "agent" || w.Kind == "job"):
				item, err = c.claimAttempt(ctx, tx, now, cat, w)
			}
			if err != nil || item != nil {
				return err
			}
		}
		return nil
	})
	return item, err
}

func (c *Core) newLease(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker, kind, workID string, leaseNo, leaseS int, runID string) (*WorkItem, error) {
	until := now.Add(time.Duration(leaseS) * time.Second)
	if _, err := tx.Exec(ctx, `INSERT INTO leases (work_id, lease_no, kind, tenant, worker_id, epoch, state, lease_until, run_id, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'active', $7, $8, $9)`,
		workID, leaseNo, kind, w.Tenant, w.ID, c.epoch, until, nullable(runID), now); err != nil {
		return nil, err
	}
	tok, err := c.issueToken(c.epoch, workID, leaseNo, w.ID)
	if err != nil {
		return nil, err
	}
	return &WorkItem{
		WorkID: workID, Kind: kind, LeaseNo: leaseNo, Epoch: c.epoch, Tenant: w.Tenant,
		LeaseUntil: fmtTime(until), LeaseRemainingS: leaseS, Token: tok,
	}, nil
}

// claimAttempt takes the oldest task of the highest priority class among the types the role
// claims (DSP-2) and writes the lease and the transition to working in one transaction.
func (c *Core) claimAttempt(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker) (*WorkItem, error) {
	role, ok := cat.Roles[w.Role]
	if !ok || role.WorkerKind != w.Kind {
		return nil, nil
	}
	// Only the claimed types that this worker kind runs.
	var claims []string
	for _, t := range role.Claims {
		if tt, ok := cat.TaskTypes[t]; ok && tt.WorkerKind == w.Kind {
			claims = append(claims, t)
		}
	}
	if len(claims) == 0 {
		return nil, nil
	}
	var (
		id, typ   string
		body, rec []byte
		leaseNo   int
		initiator string
	)
	err := tx.QueryRow(ctx, `SELECT id, type, body, record, lease_no, initiator FROM tasks
		WHERE tenant = $1 AND state = 'submitted' AND type = ANY($2) AND (run_at IS NULL OR run_at <= $3) AND (deadline_at IS NULL OR deadline_at > $3)
		ORDER BY priority_class, created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, claims, now).
		Scan(&id, &typ, &body, &rec, &leaseNo, &initiator)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	tt := cat.TaskTypes[typ]
	leaseS := c.cfg.LeaseS
	if d, ok := parseDuration(tt.Timeouts.Lease); ok {
		leaseS = int(d / time.Second)
	}
	leaseS = min(leaseS, maxLeaseS)
	leaseNo++
	runID := id + ":" + strconv.Itoa(leaseNo)
	item, err := c.newLease(ctx, tx, now, w, "attempt", id, leaseNo, leaseS, runID)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE tasks SET state = 'working', lease_no = $2, run_at = NULL, updated_at = $3 WHERE id = $1`, id, leaseNo, now); err != nil {
		return nil, err
	}
	// The model handle of the role's main slot (SEC-3): bound to this lease and epoch.
	handles := map[string]any{"tools": nil, "model": nil, "session": nil, "memory": nil}
	if slot, profile := mainSlot(role); slot != "" {
		h := "bh_" + ids.Secret(32)
		if _, err := tx.Exec(ctx, `INSERT INTO handles (hash, kind, tenant, work_id, lease_no, epoch, worker_id, slot, profile, created_at)
			VALUES ($1, 'model', $2, $3, $4, $5, $6, $7, $8, $9)`,
			c.hashSecret(h), w.Tenant, id, leaseNo, c.epoch, w.ID, slot, profile, now); err != nil {
			return nil, err
		}
		handles["model"] = h
	}
	var record map[string]any
	if err := json.Unmarshal(rec, &record); err != nil {
		return nil, err
	}
	var taskBody map[string]any
	if err := json.Unmarshal(body, &taskBody); err != nil {
		return nil, err
	}
	payload := map[string]any{
		"task": map[string]any{
			"id": id, "type": typ, "body": taskBody,
			"record": map[string]any{"max_tier": record["max_tier"], "data_mark": record["data_mark"], "untrusted_seen": record["untrusted_seen"]},
		},
		"instruction": strings.TrimSpace(tt.Instruction),
		"handles":     handles,
	}
	// Implementation extension (MAN-3): the result schema, so that the runtime can ask the
	// model for a matching answer. The instruction itself stays the trusted text from git.
	if tt.ResultSchema != nil {
		payload["result_schema"] = tt.ResultSchema
	}
	item.Payload = payload
	if err := c.appendEvent(ctx, tx, now, w.Tenant, Event{Type: "task.working", Subject: id, Data: map[string]any{"task": id, "lease_no": leaseNo}}); err != nil {
		return nil, err
	}
	if _, err := c.appendAudit(ctx, tx, now, Audit{
		Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "bot", Role: w.Role, Task: id},
		Command: "task.claim", Target: &Target{Kind: "task", ID: id}, OK: true,
		After: map[string]any{"lease_no": leaseNo, "worker": w.ID},
	}); err != nil {
		return nil, err
	}
	return item, nil
}

// mainSlot picks the slot of the attempt's model handle: base, then analysis, then seat, then
// the first by name.
func mainSlot(role manifest.Role) (string, string) {
	for _, s := range []string{"base", "analysis", "seat"} {
		if p, ok := role.Models[s]; ok {
			return s, p
		}
	}
	names := make([]string, 0, len(role.Models))
	for s := range role.Models {
		names = append(names, s)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "", ""
	}
	return names[0], role.Models[names[0]]
}

// claimDelivery takes the oldest planned delivery for this channel, sink or adapter by its
// enroll name (TEN-3). Model text for a recipient waits until their ai_notice in this channel
// is delivered (DLV-13).
func (c *Core) claimDelivery(ctx context.Context, tx pgx.Tx, now time.Time, w *Worker) (*WorkItem, error) {
	var (
		id, kind, addr, targetKind, targetID string
		recipient, dataMark                  *string
		body                                 []byte
		leaseNo                              int
	)
	err := tx.QueryRow(ctx, `SELECT d.id, d.kind, d.addr, d.target_kind, d.target_id, d.recipient, d.data_mark, d.body, d.lease_no FROM deliveries d
		WHERE d.tenant = $1 AND d.exec_kind = $2 AND d.exec_name = $3 AND d.state = 'planned' AND (d.run_at IS NULL OR d.run_at <= $4)
		AND NOT (d.dataorigin = 'model' AND EXISTS (SELECT 1 FROM contacts ct WHERE ct.tenant = d.tenant AND ct.channel = d.exec_name
			AND ct.principal = d.recipient AND ct.ai_notice_delivery IS NOT NULL AND ct.ai_notice_at IS NULL))
		ORDER BY d.seq LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, w.Kind, w.Target, now).
		Scan(&id, &kind, &addr, &targetKind, &targetID, &recipient, &dataMark, &body, &leaseNo)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	leaseNo++
	item, err := c.newLease(ctx, tx, now, w, "delivery", id, leaseNo, c.cfg.LeaseS, "")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE deliveries SET state = 'sending', lease_no = $2, attempts = attempts + 1, run_at = NULL, updated_at = $3 WHERE id = $1`,
		id, leaseNo, now); err != nil {
		return nil, err
	}
	var b map[string]any
	if err := json.Unmarshal(body, &b); err != nil {
		return nil, err
	}
	d := map[string]any{"id": id, "kind": kind, "to": addr, "body": b}
	if targetKind == "chat" {
		k, ext, _ := strings.Cut(targetID, ":")
		d["recipient"] = map[string]any{"chat": map[string]any{"kind": k, "external_id": ext}}
	}
	payload := map[string]any{
		"target":   map[string]any{"kind": w.Kind, "name": w.Target},
		"delivery": d,
	}
	// Implementation extension (MAN-3): the recipient's locale for code texts.
	if recipient != nil {
		var locale *string
		_ = tx.QueryRow(ctx, `SELECT locale FROM principals WHERE tenant = $1 AND id = $2`, w.Tenant, *recipient).Scan(&locale)
		if locale != nil && *locale != "" {
			payload["locale"] = *locale
		}
	}
	if _, ok := payload["locale"]; !ok {
		if cat, err := c.Catalog(ctx); err == nil {
			if tv, ok := cat.Tenants[w.Tenant]; ok && tv.Spec.Locale != "" {
				payload["locale"] = tv.Spec.Locale
			}
		}
	}
	item.Payload = payload
	return item, nil
}
