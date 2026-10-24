// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	crand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// The test-control operations beyond the clock and the loops (spec/ports/test-control.md,
// conformance/runner.md "Test bench"): process.stop, store.fault, store.snapshot,
// store.restore, authorize.eval, function.eval and event.inject. Like the clock they exist only
// on a test bench (APR-9) and refuse on any other core; they change time, order and environment
// failures only, and take no shortcut past a rule. authorize.eval and function.eval take their
// input in the request and write nothing, not even an audit record (runner.md); every other
// operation writes an audit record of the service principal into the farm chain (AUD-1).

// benchFaults are the faults of store.fault that the core itself keeps: the mock measurement of
// the store volume. The store keeps its own (store/fault.go).
type benchFaults struct {
	// lowSpace is 0 without the fault, 1 below the soft and 2 below the hard threshold (CAP-5).
	lowSpace atomic.Int32
}

// benchSpace is the measurement of the store volume under a low_space fault: a free share
// between the thresholds for soft and half the hard threshold for hard.
func (c *Core) benchSpace() (free, total uint64, ok bool) {
	const t = 1 << 30
	switch c.bench.lowSpace.Load() {
	case 1:
		return uint64(t * (c.cfg.Health.StoreFreeSoft + c.cfg.Health.StoreFreeHard) / 2), t, true
	case 2:
		return uint64(t * c.cfg.Health.StoreFreeHard / 2), t, true
	}
	return 0, 0, false
}

// benchAudit writes the audit record of a test-control operation into the farm chain.
func (c *Core) benchAudit(ctx context.Context, command string, target *Target, after map[string]any) (*TestClock, error) {
	var out TestClock
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		out.Now = fmtTime(now)
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: "system", Via: "system"},
			Command: command, Target: target, OK: true, After: after,
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ProcessStopRequest is test-process-stop-request.
type ProcessStopRequest struct {
	Process struct {
		Kind     string `json:"kind"`
		WorkerID string `json:"worker_id"`
	} `json:"process"`
	Mode string `json:"mode"`
}

// TestProcessStop stops a test bench process. A worker is a process of the bench, not of the
// core: the core checks that the worker is enrolled and records the stop, and the bench stops
// its process; a killed worker then goes silent and its leases end by the clock (FEN-1), a
// graceful one releases them over P1 before it exits. The core itself, kill or graceful, starts
// again over the same store: all its state is in the store, written per transaction, so a
// graceful stop flushes nothing a kill would lose, and the start does what a new process does
// (the epoch, the restore mark, the leases of the downtime, the end of a drain).
func (c *Core) TestProcessStop(ctx context.Context, req ProcessStopRequest) (*TestClock, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	if req.Mode != "kill" && req.Mode != "graceful" {
		return nil, refuse("input_schema_invalid", "mode is kill or graceful")
	}
	switch req.Process.Kind {
	case "worker":
		var id string
		err := c.st.Pool.QueryRow(ctx, `SELECT id FROM workers WHERE id = $1`, req.Process.WorkerID).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, refuse("not_found", "no such worker")
		}
		if err != nil {
			return nil, err
		}
		return c.benchAudit(ctx, "test/process.stop", &Target{Kind: "worker", ID: id}, map[string]any{"mode": req.Mode})
	case "core":
		if err := c.benchRestart(ctx); err != nil {
			return nil, err
		}
		return c.benchAudit(ctx, "test/process.stop", &Target{Kind: "core", ID: "core"}, map[string]any{"mode": req.Mode})
	}
	return nil, refuse("input_schema_invalid", "process.kind is worker or core")
}

// benchRestart is the start of a new core process over the store it already has: the
// in-memory state of a process starts empty (the epoch read from the store, no store-down mode,
// no slow pass done, a fresh volume measurement, the catalog read again), then New's startUp,
// and the start of cmd/botaru-core: the leases that overlapped the downtime and the end of a
// drain (RET-6, FEN-5, LCY-6).
func (c *Core) benchRestart(ctx context.Context) error {
	var epoch int64
	if err := c.st.Pool.QueryRow(ctx, `SELECT epoch FROM farm_meta WHERE id = 1`).Scan(&epoch); err != nil {
		return err
	}
	c.epoch.Store(epoch)
	c.storeDown.Store(false)
	c.budgetAt.Store(0)
	c.retentionAt.Store(0)
	c.measureCapacity()
	c.cat.reset()
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	if err := c.startUp(ctx, cat); err != nil {
		return err
	}
	if err := c.endLeasesOfDowntime(ctx); err != nil {
		return err
	}
	return c.ClearDrain(ctx)
}

// StoreFaultRequest is the body of store.fault, which has no request schema in the skeleton
// (D-114): point is before_commit, unavailable, low_space or clear; count is the number of
// failed commits of before_commit (1 by default); level is soft or hard for low_space (hard
// by default).
type StoreFaultRequest struct {
	Point string `json:"point"`
	Count int64  `json:"count"`
	Level string `json:"level"`
}

// TestStoreFault injects a store fault (OBS-8, CAP-5). unavailable makes every transaction fail
// and puts the core in its store-down mode at once, as the loops would on their next tick: P1
// answers not_ready and health not alive. clear removes every fault and then runs one tick of
// every loop, the way Run leaves the mode only when the store answers and the loops ticked
// again. low_space sets the mock measurement of the store volume below a threshold; a later
// loop.tick of health raises the alarm.
func (c *Core) TestStoreFault(ctx context.Context, req StoreFaultRequest) (*TestClock, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	after := map[string]any{"point": req.Point}
	switch req.Point {
	case "before_commit":
		n := req.Count
		if n == 0 {
			n = 1
		}
		if n < 0 || n > 100 {
			return nil, refuse("input_schema_invalid", "count is 1 to 100")
		}
		// The audit record goes first: the fault applies to the transactions after it.
		out, err := c.benchAudit(ctx, "test/store.fault", &Target{Kind: "store", ID: "store"}, map[string]any{"point": req.Point, "count": n})
		if err != nil {
			return nil, err
		}
		c.st.FailCommits(n)
		return out, nil
	case "unavailable":
		out, err := c.benchAudit(ctx, "test/store.fault", &Target{Kind: "store", ID: "store"}, after)
		if err != nil {
			return nil, err
		}
		c.st.SetUnavailable(true)
		c.storeDown.Store(true)
		return out, nil
	case "low_space":
		level := int32(2)
		switch req.Level {
		case "", "hard":
		case "soft":
			level = 1
		default:
			return nil, refuse("input_schema_invalid", "level is soft or hard")
		}
		c.bench.lowSpace.Store(level)
		c.measureCapacity()
		after["level"] = map[int32]string{1: "soft", 2: "hard"}[level]
		return c.benchAudit(ctx, "test/store.fault", &Target{Kind: "store", ID: "store"}, after)
	case "clear":
		c.st.SetUnavailable(false)
		c.st.FailCommits(0)
		c.bench.lowSpace.Store(0)
		c.measureCapacity()
		if c.storeDown.Load() {
			// As Run leaves the mode: the leases of the outage are extended first (FEN-5).
			if err := c.endLeasesOfDowntime(ctx); err != nil {
				return nil, err
			}
			if err := c.Tick(ctx); err != nil {
				return nil, err
			}
			c.storeDown.Store(false)
		}
		return c.benchAudit(ctx, "test/store.fault", &Target{Kind: "store", ID: "store"}, after)
	}
	return nil, refuse("input_schema_invalid", "point is before_commit, unavailable, low_space or clear")
}

// snapshotSchema is the schema of the store that holds the test bench backup of a label.
func snapshotSchema(label string) string {
	h := sha256.Sum256([]byte(label))
	return "bench_snapshot_" + hex.EncodeToString(h[:8])
}

// benchTables are the tables of the store that a test bench backup holds: every table of the
// schema except the migration list and farm_meta, which carries the epoch, the clock and the
// test bench flag of the running store (a restore never moves them back, TIM-1, RET-6).
func benchTables(ctx context.Context, q store.Querier) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'
		AND table_name NOT IN ('schema_migrations', 'farm_meta') ORDER BY table_name`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// TestStoreSnapshot takes a backup of the test bench store under a label, as the external backup
// process takes one (RET-5): a consistent copy of every table at one instant. The bench keeps it
// in a schema of the same database, which only this port reads; a new snapshot with the same
// label replaces the old one.
func (c *Core) TestStoreSnapshot(ctx context.Context, label string) (*TestClock, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	if !benchLabel(label) {
		return nil, refuse("input_schema_invalid", "label")
	}
	schema := pgx.Identifier{snapshotSchema(label)}.Sanitize()
	var out TestClock
	err := c.st.Tx(ctx, func(tx pgx.Tx) error {
		// A repeatable read sees every table at the same instant, as a dump does.
		if _, err := tx.Exec(ctx, `SET TRANSACTION ISOLATION LEVEL REPEATABLE READ`); err != nil {
			return err
		}
		now, err := store.Now(ctx, tx)
		if err != nil {
			return err
		}
		out.Now = fmtTime(now)
		// The record of the snapshot is in the backup, as the last record before it.
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: "system", Via: "system"},
			Command: "test/store.snapshot", Target: &Target{Kind: "store", ID: label}, OK: true,
		}); err != nil {
			return err
		}
		tables, err := benchTables(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
			return err
		}
		for _, t := range tables {
			id := pgx.Identifier{t}.Sanitize()
			if _, err := tx.Exec(ctx, `CREATE TABLE `+schema+`.`+id+` AS SELECT * FROM `+id); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

func benchLabel(label string) bool {
	if label == "" || len(label) > 64 {
		return false
	}
	for i, r := range label {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || i > 0 && (r == '_' || r == '.' || r == '-')
		if !ok {
			return false
		}
	}
	return true
}

// TestStoreRestore restores the test bench store from the backup of a label by the real restore
// path (RET-6, AUD-6): the separate erasure log is exported first, as farmctl erasure-log export
// keeps it apart from the store backups (RET-5); in one transaction the tables are loaded from
// the backup and RestoreBegin grows the epoch past the running one, writes the restore record
// into the farm chain and replays the erasure log; then the core starts again over the restored
// store, which settles the approvals and leaves it not ready until P2 restore.complete.
func (c *Core) TestStoreRestore(ctx context.Context, label string) (*TestClock, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	if !benchLabel(label) {
		return nil, refuse("input_schema_invalid", "label")
	}
	schema := snapshotSchema(label)
	var exists bool
	if err := c.st.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`, schema).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, refuse("not_found", "no test bench backup with this label")
	}
	log, err := c.ErasureLogExport(ctx)
	if err != nil {
		return nil, err
	}
	// P1 is not ready from the first table loaded: the core answers not_ready while a restore
	// runs (RET-6 (d)). The load and RestoreBegin are one transaction, so a failure leaves the
	// store as it was, and the mark of the store again decides readiness.
	c.restoring.Store(true)
	var epoch int64
	err = c.st.Tx(ctx, func(tx pgx.Tx) error {
		tables, err := benchTables(ctx, tx)
		if err != nil {
			return err
		}
		order, err := parentsFirst(ctx, tx, tables)
		if err != nil {
			return err
		}
		ids := make([]string, len(tables))
		for i, t := range tables {
			ids[i] = pgx.Identifier{t}.Sanitize()
		}
		if _, err := tx.Exec(ctx, `TRUNCATE `+strings.Join(ids, ", ")); err != nil {
			return err
		}
		snap := pgx.Identifier{schema}.Sanitize()
		for _, t := range order {
			var saved bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2)`, schema, t).Scan(&saved); err != nil {
				return err
			}
			if !saved {
				// A table that a later migration added has no rows in the older backup.
				continue
			}
			rows, err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = $1 AND table_name = $2 ORDER BY ordinal_position`, schema, t)
			if err != nil {
				return err
			}
			cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				return err
			}
			for i, col := range cols {
				cols[i] = pgx.Identifier{col}.Sanitize()
			}
			list := strings.Join(cols, ", ")
			id := pgx.Identifier{t}.Sanitize()
			if _, err := tx.Exec(ctx, `INSERT INTO `+id+` (`+list+`) SELECT `+list+` FROM `+snap+`.`+id); err != nil {
				return fmt.Errorf("restore %s: %w", t, err)
			}
		}
		now, err := store.Now(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := c.restoreBegin(ctx, tx, now, log); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `SELECT epoch FROM farm_meta WHERE id = 1`).Scan(&epoch)
	})
	if err != nil {
		_ = c.loadRestoring(ctx)
		return nil, err
	}
	// The new epoch fences the old tokens from the commit on, before the start of the core
	// reloads the rest (FEN-1).
	c.epoch.Store(epoch)
	if err := c.benchRestart(ctx); err != nil {
		return nil, err
	}
	return c.benchAudit(ctx, "test/store.restore", &Target{Kind: "store", ID: label}, map[string]any{"epoch": c.epoch.Load()})
}

// parentsFirst orders tables so that a table comes after every table its foreign keys name.
func parentsFirst(ctx context.Context, q store.Querier, tables []string) ([]string, error) {
	rows, err := q.Query(ctx, `SELECT c.conrelid::regclass::text, c.confrelid::regclass::text FROM pg_constraint c
		JOIN pg_namespace n ON n.oid = c.connamespace WHERE c.contype = 'f' AND n.nspname = current_schema()`)
	if err != nil {
		return nil, err
	}
	parents := map[string][]string{}
	for rows.Next() {
		var child, parent string
		if err := rows.Scan(&child, &parent); err != nil {
			rows.Close()
			return nil, err
		}
		if child != parent {
			parents[child] = append(parents[child], parent)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []string
	done := map[string]bool{}
	var visit func(t string, depth int) error
	visit = func(t string, depth int) error {
		if done[t] {
			return nil
		}
		if depth > len(tables) {
			return fmt.Errorf("a cycle of foreign keys at %s", t)
		}
		for _, p := range parents[t] {
			if slices.Contains(tables, p) {
				if err := visit(p, depth+1); err != nil {
					return err
				}
			}
		}
		done[t] = true
		out = append(out, t)
		return nil
	}
	for _, t := range tables {
		if err := visit(t, 0); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// AuthzDecision is authz-decision.
type AuthzDecision struct {
	Decision string   `json:"decision"`
	Reason   string   `json:"reason,omitempty"`
	Needs    []string `json:"needs,omitempty"`
	Hold     string   `json:"hold,omitempty"`
	Stage    string   `json:"stage,omitempty"`
}

// authzRequest is authz-request.
type authzRequest struct {
	Principal struct {
		Kind   string `json:"kind"`
		ID     string `json:"id"`
		Tenant string `json:"tenant"`
	} `json:"principal"`
	Action   string `json:"action"`
	Resource struct {
		Tenant string `json:"tenant"`
		Kind   string `json:"kind"`
		ID     string `json:"id"`
	} `json:"resource"`
	Context struct {
		Task struct {
			Type       string   `json:"type"`
			DataMark   string   `json:"data_mark"`
			Initiators []string `json:"initiators"`
		} `json:"task"`
		Chat struct {
			Kind    string `json:"kind"`
			Ceiling string `json:"ceiling"`
		} `json:"chat"`
		Budget struct {
			Exhausted bool `json:"exhausted"`
		} `json:"budget"`
	} `json:"context"`
}

// TestAuthorize answers one row of the permission table (PRM-2, PRM-1) by the decisions the core
// itself makes, not a copy of them: a task:<type> row runs the decision of enqueue (the type and
// its purpose bans, then authorizeEnqueue: holds, the initiator's and every other initiator's
// rights, the files, the chain and the money windows), an action.approve row the decision of an
// approval press, and a group chat the ceiling check of the handoff. tenantKind is the tenant
// kind of the row: a production row is checked without a production tenant on the bench
// (runner.md, APR-9). It writes nothing.
func (c *Core) TestAuthorize(ctx context.Context, body map[string]any, tenantKind string) (*AuthzDecision, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	if tenantKind == "" {
		tenantKind = "test"
	}
	if tenantKind != "test" && tenantKind != "production" {
		return nil, refuse("input_schema_invalid", "tenant_kind is test or production")
	}
	if err := c.spec.Validate("authz-request", body); err != nil {
		return nil, refuse("input_schema_invalid", "the request does not match authz-request")
	}
	raw, _ := json.Marshal(body)
	var req authzRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, refuse("input_schema_invalid", "the request could not be read")
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	tv := cat.Tenants[req.Resource.Tenant]
	if tv == nil || tv.Spec.Kind == "" {
		return nil, refuse("not_found", "the tenant of the resource is not applied")
	}
	deny := func(stage, reason string, needs ...string) *AuthzDecision {
		return &AuthzDecision{Decision: "deny", Stage: stage, Reason: reason, Needs: needs}
	}
	var out *AuthzDecision
	err = c.st.Tx(ctx, func(tx pgx.Tx) error {
		now, err := store.Now(ctx, tx)
		if err != nil {
			return err
		}
		if out, err = c.authorizeRow(ctx, tx, now, cat, tv, &req, tenantKind, deny); err != nil {
			return err
		}
		// The decision writes nothing: the transaction is rolled back.
		return errReadOnly
	})
	if err != nil && !errors.Is(err, errReadOnly) {
		return nil, err
	}
	return out, nil
}

// errReadOnly rolls back a transaction that only read.
var errReadOnly = errors.New("read only")

func (c *Core) authorizeRow(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, req *authzRequest, tenantKind string,
	deny func(stage, reason string, needs ...string) *AuthzDecision) (*AuthzDecision, error) {
	if (req.Principal.Kind == "system") != (req.Principal.ID == "system") {
		return nil, refuse("input_schema_invalid", "the principal kind does not match its id")
	}
	d, err := c.authorizeDecide(ctx, tx, now, cat, tv, req, tenantKind, deny)
	if err != nil {
		return nil, err
	}
	// A request names one tenant: a principal of another tenant reaches no decision of this one
	// (TEN-1). Holds come first in the decision order, so a hold still answers.
	if req.Principal.Tenant != req.Resource.Tenant && d.Stage != "hold" {
		return deny("ban", "tenant_mismatch"), nil
	}
	return d, nil
}

// authorizeDecide is the decision of authorizeRow for the resource's tenant.
func (c *Core) authorizeDecide(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, req *authzRequest, tenantKind string,
	deny func(stage, reason string, needs ...string) *AuthzDecision) (*AuthzDecision, error) {
	form, name, _ := strings.Cut(req.Action, ":")
	var others []string
	for _, id := range req.Context.Task.Initiators {
		if id != req.Principal.ID {
			others = append(others, id)
		}
	}
	mark := req.Context.Task.DataMark
	switch form {
	case "task":
		// The decision of enqueue itself (PRM-2): a direct request of the principal, as system
		// from its schedules (PRM-10), with the other initiators of what the tree read and the
		// mark the task carries over.
		in := enqueueInput{Tenant: tv.Name, Type: name, Initiator: req.Principal.ID, EnqueuedBy: req.Principal.ID,
			Source: "interactive", CoInitiators: others, ContinuesMark: mark}
		if req.Principal.ID == "system" {
			in.Source = "schedule"
		}
		tt, pr := c.enqueueType(cat, in)
		if pr != nil {
			return deny("ban", pr.Reason), nil
		}
		g, denial, err := c.authorizeEnqueue(ctx, tx, now, cat, tv, tt, in)
		if err != nil {
			return nil, err
		}
		if denial != nil {
			d := deny(denial.stage, denial.p.Reason, denial.p.Needs...)
			d.Hold = denial.hold
			return d, nil
		}
		mark = g.mark
	case "action.approve":
		// The decision of an approval press (APR-4): the approver's groups and the self
		// approval the tenant allows; a production tenant never allows it (APR-9). The holds
		// of the action come after the permission, as at a press.
		groups, err := principalGroups(ctx, tx, now, tv, req.Principal.ID)
		if err != nil {
			return nil, err
		}
		// The number of approvers is the action's, as insertApproval sets it (APR-16).
		a := &approvalRow{Tenant: tv.Name, Action: name, Approvals: 1}
		if act, ok := c.actionOf(cat, name); ok && act.Approvals == 2 {
			a.Approvals = 2
		}
		if len(req.Context.Task.Initiators) > 0 {
			a.Initiator = req.Context.Task.Initiators[0]
		}
		view := tv
		if tenantKind == "production" && tv.Spec.SelfApprove {
			cp := *tv
			cp.Spec.SelfApprove = false
			view = &cp
		}
		if approveRefusal(view, groups, req.Principal.ID, a) != "" {
			return deny("intersection", "forbidden", req.Action), nil
		}
		scopes, err := c.approvalHoldScopes(ctx, tx, cat, a)
		if err != nil {
			return nil, err
		}
		// In press order: no_action cancels, then pause_action pauses (HLD-8).
		for _, effect := range []string{"no_action", "pause_action"} {
			if held, err := heldBy(ctx, tx, now, []string{effect}, scopes...); err != nil {
				return nil, err
			} else if held != "" {
				d := deny("hold", "held")
				d.Hold = held
				return d, nil
			}
		}
	default:
		// Any other capability: the principal's groups and those of every other initiator,
		// by the functions every decision point uses (PRM-1, PRM-6).
		capName := req.Action
		if form == "data" {
			capName, mark = "", name
		}
		for _, id := range append([]string{req.Principal.ID}, others...) {
			// An unknown or inactive person gets person_inactive, as at enqueue.
			if id != "system" {
				p, err := loadPrincipal(ctx, tx, now, tv.Name, id)
				if errors.Is(err, pgx.ErrNoRows) || err == nil && p.State != "active" {
					return deny("intersection", "person_inactive"), nil
				}
				if err != nil {
					return nil, err
				}
			}
			groups, err := principalGroups(ctx, tx, now, tv, id)
			if err != nil {
				return nil, err
			}
			if capName != "" && !hasCap(tv, groups, capName) {
				return deny("intersection", "no_group_cap", capName), nil
			}
			if mark != "" && !hasData(tv, groups, mark) {
				return deny("intersection", "no_data_class", "data:"+mark), nil
			}
		}
	}
	// In a group chat the mark stays within the chat's ceiling, as the handoff checks it
	// (PRM-5, claim of a delivery).
	if req.Context.Chat.Kind == "group" && req.Context.Chat.Ceiling != "" && mark != "" && !c.markWithin(mark, req.Context.Chat.Ceiling) {
		return deny("intersection", "channel_ceiling"), nil
	}
	// The row's own budget state, beside the money windows the decision of enqueue read (BUD-1).
	if req.Context.Budget.Exhausted {
		return deny("intersection", "budget_exhausted"), nil
	}
	return &AuthzDecision{Decision: "allow", Stage: "intersection"}, nil
}

// FunctionEvalRequest is test-function-eval-request.
type FunctionEvalRequest struct {
	Function string         `json:"function"`
	Input    map[string]any `json:"input"`
}

// FunctionEval is the answer of function.eval: the function's output and the store clock.
type FunctionEval struct {
	Now    string         `json:"now"`
	Output map[string]any `json:"output"`
}

// TestFunctionEval calls one pure core function by name with the input of a decision table
// case (runner.md, "Decision table"); it writes nothing. The functions are the ones the core
// itself runs: digest.sample (DIG-5), reporter.number_check (REP-4) and schedule.check (TIM-5).
// commission.accuracy (COM-17) reads the scored forecasts of the store and is no pure function
// in this build, so it is refused as not built.
func (c *Core) TestFunctionEval(ctx context.Context, req FunctionEvalRequest) (*FunctionEval, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	var now time.Time
	if err := c.st.Tx(ctx, func(tx pgx.Tx) error {
		var err error
		now, err = store.Now(ctx, tx)
		return err
	}); err != nil {
		return nil, err
	}
	in := req.Input
	if in == nil {
		in = map[string]any{}
	}
	var out map[string]any
	var err error
	switch req.Function {
	case "digest.sample":
		out, err = evalDigestSample(in)
	case "reporter.number_check":
		out, err = c.evalNumberCheck(in)
	case "schedule.check":
		out, err = evalScheduleCheck(in, now)
	case "commission.accuracy":
		return nil, refuse("not_found", "this build has no pure function commission.accuracy: the accuracy is computed from the scored forecasts of the store")
	default:
		return nil, refuse("not_found", "this build has no function %s", req.Function)
	}
	if err != nil {
		return nil, err
	}
	return &FunctionEval{Now: fmtTime(now), Output: out}, nil
}

// evalDigestSample is the monthly audit sample (DIG-5) from commission.audit_sample, the counts
// of verdicts by <state>_<reason> (dismissed_veto, parked_score, parked_quorum, ...), a seed of
// 64 hex digits (a fresh one without it) and the blind answers over 180 days.
func evalDigestSample(in map[string]any) (map[string]any, error) {
	minPer, share, perDigest := sampleConfigFrom(mmap(mmap(in, "commission"), "audit_sample"))
	var pop []string
	excluded := []string{}
	keys := slices.Sorted(mapKeys(mmap(in, "verdicts")))
	for _, k := range keys {
		state, reason, ok := strings.Cut(k, "_")
		count, okn := toFloat(mmap(in, "verdicts")[k])
		if !ok || !okn || count < 0 || count > 10000 {
			return nil, refuse("input_schema_invalid", "verdicts.%s", k)
		}
		if reason == "minimum" {
			reason = "below_min"
		}
		if !sampleEligible(state, reason) {
			excluded = append(excluded, k)
			continue
		}
		for i := range int(count) {
			pop = append(pop, fmt.Sprintf("%s_%04d", k, i+1))
		}
	}
	sort.Strings(pop)
	var seed [32]byte
	if s := mstr(in, "seed"); s != "" {
		b, err := hex.DecodeString(s)
		if err != nil || len(b) != 32 {
			return nil, refuse("input_schema_invalid", "seed is 64 hex digits")
		}
		copy(seed[:], b)
	} else if _, err := crand.Read(seed[:]); err != nil {
		return nil, err
	}
	n := sampleSize(len(pop), minPer, share)
	out := map[string]any{
		"population": len(pop), "size": n, "seed": hex.EncodeToString(seed[:]),
		"drawn": strAny(sampleDraw(pop, n, seed)), "excluded_states": strAny(excluded), "max_per_digest": perDigest,
	}
	if len(pop) > 0 {
		out["inclusion_prob"] = math.Round(float64(n)/float64(len(pop))*1e6) / 1e6
	}
	if list, ok := in["answers_180d"].([]any); ok {
		var answers []sampleAnswer
		for _, x := range list {
			m := asMap(x)
			prob, _ := toFloat(m["inclusion_prob"])
			yes, _ := toFloat(m["would_accept"])
			no, _ := toFloat(m["would_not_accept"])
			if yes < 0 || no < 0 || yes+no > 10000 {
				return nil, refuse("input_schema_invalid", "answers_180d")
			}
			for range int(yes) {
				answers = append(answers, sampleAnswer{answer: "would_accept", prob: prob})
			}
			for range int(no) {
				answers = append(answers, sampleAnswer{answer: "would_not_accept", prob: prob})
			}
		}
		st := sampleShare(answers)
		out["audit_sample"] = st
		out["commission_underrates"] = st["commission_underrates"]
	}
	return out, nil
}

// evalNumberCheck is the number check of a model paragraph (REP-4) by the check a digest
// builds (digestNumberCheck): text is the paragraph; snapshot.rows are the rows of the digest
// table (their type, title and state are its names), snapshot.taken_at the snapshot time and
// snapshot.window.from the window start (the window ends at the snapshot); catalog_names are
// the names of the tenant catalog; binding.locale chooses number.words; timezone is the
// tenant's zone (UTC by default).
func (c *Core) evalNumberCheck(in map[string]any) (map[string]any, error) {
	const own = "demo"
	snap := mmap(in, "snapshot")
	vs := &viewSnapshot{From: mstr(mmap(snap, "window"), "from")}
	for _, at := range []string{mstr(snap, "taken_at"), vs.From} {
		if _, err := time.Parse(time.RFC3339Nano, at); at != "" && err != nil {
			return nil, refuse("input_schema_invalid", "snapshot times")
		}
	}
	if at := mstr(snap, "taken_at"); at != "" {
		vs.ID = "snap:" + at
	}
	rows, _ := snap["rows"].([]any)
	for _, r := range rows {
		vs.Counts = append(vs.Counts, asMap(r))
	}
	var entries []any
	names, _ := in["catalog_names"].([]any)
	for _, v := range names {
		if s, ok := v.(string); ok {
			entries = append(entries, map[string]any{"name": s})
		}
	}
	tv := &TenantView{Name: own}
	if z := mstr(in, "timezone"); z != "" {
		if _, err := time.LoadLocation(z); err != nil || z == "Local" {
			return nil, refuse("input_schema_invalid", "timezone")
		}
		tv.Spec.Timezone = z
	}
	cat := &Catalog{Tenants: map[string]*TenantView{own: tv}, RawGlobals: map[manifest.Key]map[string]any{
		{Kind: "Catalog", Tenant: own, Name: "catalog"}: {"spec": map[string]any{"resources": entries}},
	}}
	n := digestNumberCheck(cat, own, vs)
	locale := mstr(mmap(in, "binding"), "locale")
	if locale == "" {
		locale = "en"
	}
	text := mstr(in, "text")
	if len(text) > 8192 {
		return nil, refuse("input_schema_invalid", "text is too long")
	}
	checked, rejected := n.paragraphNumbers(text, c.texts.List(locale, "number.words"))
	paragraph := "present"
	if rejected > 0 {
		paragraph = "absent"
	}
	return map[string]any{"paragraph": paragraph, "numbers_checked": checked, "numbers_unmatched": rejected}, nil
}

// evalScheduleCheck is the check of a schedule change before "Accept" (TIM-5): cron, timezone,
// makes_backup of its task type and at, the card time (the store clock by default). The answer
// says whether the card gets the "Accept" button, the refusal when it does not, and the local
// times that disappear or repeat within a year.
func evalScheduleCheck(in map[string]any, now time.Time) (map[string]any, error) {
	if at := mstr(in, "at"); at != "" {
		t, err := time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, refuse("input_schema_invalid", "at")
		}
		now = t
	}
	backup, _ := in["makes_backup"].(bool)
	fields, err := scheduleCheck(mstr(in, "cron"), mstr(in, "timezone"), backup, now)
	var rej rejection
	if errors.As(err, &rej) {
		return map[string]any{"accept_button": false, "reason": string(rej)}, nil
	}
	if err != nil {
		return nil, err
	}
	out := map[string]any{"accept_button": true, "times_disappear": []any{}, "times_repeat": []any{}}
	for k, v := range fields {
		if l, ok := v.([]string); ok {
			out[k] = strAny(l)
		}
	}
	return out, nil
}

// EventInjectRequest is the body of event.inject, which has no request schema in the skeleton
// (D-114): event is an incoming Activity and via the id of the enrolled channel or monitoring
// adapter that submits it.
type EventInjectRequest struct {
	Event map[string]any `json:"event"`
	Via   string         `json:"via"`
}

// EventInject is the answer of event.inject: the answer submit_activity gave and the clock.
type EventInject struct {
	Now    string          `json:"now"`
	Result *ActivityResult `json:"result"`
}

// TestEventInject submits an incoming activity by the same path as P1 submit_activity of the
// named process (IDN-3): the activity schema, the process's own checks and the tenant's rules
// apply as if the process had sent it, so an unverified sender or a forward is as powerless as
// on the wire.
func (c *Core) TestEventInject(ctx context.Context, req EventInjectRequest) (*EventInject, error) {
	if err := c.requireTestBench(); err != nil {
		return nil, err
	}
	if err := c.spec.Validate("activity", req.Event); err != nil {
		return nil, refuse("input_schema_invalid", "the event does not match activity")
	}
	w, err := c.benchWorker(ctx, req.Via)
	if err != nil {
		return nil, err
	}
	if err := c.P1Ready("submit_activity"); err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(req.Event)
	var a Activity
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, refuse("input_schema_invalid", "the event could not be read")
	}
	res, err := c.SubmitActivity(ctx, w, a)
	if err != nil {
		return nil, err
	}
	clock, err := c.benchAudit(ctx, "test/event.inject", &Target{Kind: "worker", ID: w.ID}, map[string]any{"result": res.Result})
	if err != nil {
		return nil, err
	}
	return &EventInject{Now: clock.Now, Result: res}, nil
}

// benchWorker loads an enrolled worker by id as AuthenticateWorker loads it by its token.
func (c *Core) benchWorker(ctx context.Context, id string) (*Worker, error) {
	var w Worker
	var labels, hello, plugins []byte
	var target, placement, host *string
	err := c.st.Pool.QueryRow(ctx, `SELECT id, tenant, role, kind, target, state, labels, hello, plugins, placement, host FROM workers WHERE id = $1`, id).
		Scan(&w.ID, &w.Tenant, &w.Role, &w.Kind, &target, &w.State, &labels, &hello, &plugins, &placement, &host)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("not_found", "no such worker")
	}
	if err != nil {
		return nil, err
	}
	if w.State == "revoked" {
		return nil, refuse("unauthenticated", "the worker is revoked")
	}
	if target != nil {
		w.Target = *target
	}
	if placement != nil {
		w.Placement = *placement
	}
	if host != nil {
		w.Host = *host
	}
	_ = json.Unmarshal(labels, &w.Labels)
	_ = json.Unmarshal(plugins, &w.Plugins)
	if len(hello) > 0 {
		var h HelloRequest
		if json.Unmarshal(hello, &h) == nil {
			w.Hello = &h
		}
	}
	return &w, nil
}
