// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/redact"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// defaultOutputLimit is the output limit of a tool without output_limit_kb.
const defaultOutputLimit = 32 << 10

// toolCallRow is a row of the toolcalls table.
type toolCallRow struct {
	ID, Tenant, Task        string
	LeaseNo                 int
	Tool, Access            string
	Args                    map[string]any
	Plugin, Placement, Host string
	Resource                string
	Resolved                map[string]string
	Untrusted               bool
	DefHash                 string
	State, Reason           string
	StartBy                 time.Time
}

func (c *Core) insertToolCall(ctx context.Context, tx pgx.Tx, now time.Time, r toolCallRow) error {
	args, err := json.Marshal(r.Args)
	if err != nil {
		return err
	}
	if r.Resolved == nil {
		r.Resolved = map[string]string{}
	}
	resolved, _ := json.Marshal(r.Resolved)
	var startBy, ended *time.Time
	if !r.StartBy.IsZero() {
		startBy = &r.StartBy
	}
	if r.State != "queued" {
		ended = &now
	}
	_, err = tx.Exec(ctx, `INSERT INTO toolcalls (id, tenant, task, attempt_lease_no, tool, access, args, target_plugin, target_placement, target_host,
		resolved, untrusted_output, state, reason, start_by, created_at, updated_at, ended_at, target_resource, tool_def_hash)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16, $17, $18, $19)`,
		r.ID, r.Tenant, r.Task, r.LeaseNo, r.Tool, r.Access, args, r.Plugin, r.Placement, nullable(r.Host),
		resolved, r.Untrusted, r.State, nullable(r.Reason), startBy, now, ended, nullable(r.Resource), nullable(r.DefHash))
	return err
}

// claimToolcall hands a queued call to a toolhost whose enroll record matches its target: the
// tenant, a plugin, the placement and the machine (TEN-3a). A call whose attempt has lost its
// lease is abandoned, one under a no_tool hold denied (HLD-3).
func (c *Core) claimToolcall(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, w *Worker) (*WorkItem, error) {
	if len(w.Plugins) == 0 {
		return nil, nil
	}
	// A plugin manifest applied after the toolhost's hello must match its live tools again
	// before it takes a call (INJ-2).
	if w.Hello == nil || toolhostMismatch(cat, w, *w.Hello) != "" {
		return nil, nil
	}
	for {
		var (
			id, task, tool, plugin, placement string
			host, resource                    *string
			leaseNo, attemptLease             int
			args, resolved                    []byte
			startBy                           time.Time
		)
		err := tx.QueryRow(ctx, `SELECT id, task, attempt_lease_no, tool, target_plugin, target_placement, target_host, target_resource, args, resolved, start_by, lease_no
			FROM toolcalls WHERE tenant = $1 AND state = 'queued' AND target_plugin = ANY($2) AND target_placement = $3
			AND (target_host IS NULL OR target_host = $4) AND start_by > $5
			ORDER BY created_at, id LIMIT 1 FOR UPDATE SKIP LOCKED`, w.Tenant, w.Plugins, w.Placement, w.Host, now).
			Scan(&id, &task, &attemptLease, &tool, &plugin, &placement, &host, &resource, &args, &resolved, &startBy, &leaseNo)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		alive, err := attemptAlive(ctx, tx, task, attemptLease)
		if err != nil {
			return nil, err
		}
		if !alive {
			if err := endToolCall(ctx, tx, now, id, "abandoned", "fenced", nil); err != nil {
				return nil, err
			}
			continue
		}
		var typ, role, agent string
		if err := tx.QueryRow(ctx, `SELECT t.type, w.role, w.id FROM tasks t, leases l JOIN workers w ON w.id = l.worker_id
			WHERE t.id = $1 AND l.work_id = $1 AND l.lease_no = $2`, task, attemptLease).Scan(&typ, &role, &agent); err != nil {
			return nil, err
		}
		// The holds of the P3 check again, a hold set meanwhile counts (HLD-3): the agent, the
		// toolhost, the target machine and resource.
		scopes := append(tenantScopes(w.Tenant),
			scopeRef{kind: "role", tenant: w.Tenant, name: role}, scopeRef{kind: "tasktype", tenant: w.Tenant, name: typ},
			scopeRef{kind: "plugin", tenant: w.Tenant, name: plugin}, scopeRef{kind: "worker", tenant: w.Tenant, name: strings.ToLower(agent)},
			scopeRef{kind: "worker", tenant: w.Tenant, name: strings.ToLower(w.ID)})
		scopes = append(scopes, targetScopes(w.Tenant, callTargetRef{host: deref(host), resource: deref(resource)})...)
		held, err := heldBy(ctx, tx, now, []string{"no_tool"}, scopes...)
		if err != nil {
			return nil, err
		}
		if held != "" {
			if err := endToolCall(ctx, tx, now, id, "denied", "held", nil); err != nil {
				return nil, err
			}
			continue
		}
		leaseNo++
		item, err := c.newLease(ctx, tx, now, w, "toolcall", id, leaseNo, c.cfg.LeaseS, "")
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `UPDATE toolcalls SET state = 'leased', lease_no = $2, worker_id = $3, updated_at = $4 WHERE id = $1`, id, leaseNo, w.ID, now); err != nil {
			return nil, err
		}
		_, name, _ := cutTool(tool)
		var a map[string]any
		if err := json.Unmarshal(args, &a); err != nil {
			return nil, err
		}
		target := map[string]any{"plugin": plugin, "placement": placement}
		if host != nil {
			target["host"] = *host
		}
		payload := map[string]any{"target": target, "tool": name, "args": a, "approval_id": nil, "grant_id": nil, "args_hash": nil}
		// Implementation extensions (MAN-3): the catalog fields a plugin needs, and the limits of
		// the tool from the manifest, so the toolhost redacts and cuts without reading manifests.
		var res map[string]string
		_ = json.Unmarshal(resolved, &res)
		if len(res) > 0 {
			payload["resolved"] = res
		}
		if t, ok := cat.Plugins[plugin].Tool(name); ok {
			limit := t.OutputLimitKB * 1024
			if limit == 0 {
				limit = defaultOutputLimit
			}
			lim := map[string]any{"redact": nonNil(t.Redact), "output_limit_bytes": limit}
			if t.TimeoutS > 0 {
				lim["timeout_s"] = t.TimeoutS
			}
			payload["limits"] = lim
		}
		item.Payload = payload
		item.StartBy = fmtTime(startBy)
		return item, nil
	}
}

// attemptAlive says whether the attempt lease that made a call is still active.
func attemptAlive(ctx context.Context, tx pgx.Tx, task string, leaseNo int) (bool, error) {
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM leases WHERE work_id = $1 AND lease_no = $2`, task, leaseNo).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return state == "active", err
}

// endToolCall moves a call to a terminal state.
func endToolCall(ctx context.Context, tx pgx.Tx, now time.Time, id, state, reason string, output []byte) error {
	_, err := tx.Exec(ctx, `UPDATE toolcalls SET state = $2, reason = $3, output = $4, updated_at = $5, ended_at = $5 WHERE id = $1`,
		id, state, nullable(reason), output, now)
	if err != nil {
		return err
	}
	return notifyWork(ctx, tx)
}

// completeToolcall accepts the outcome of a call: the core masks secrets once more, cuts the
// output at the tool limit, checks structured output against the tool's output_schema and
// marks the task untrusted_seen for a tool with untrusted output (INJ-4, INJ-13). The outcome
// of a call whose attempt is gone goes to nobody (FEN-4).
func (c *Core) completeToolcall(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, output map[string]any) error {
	var state, tool, task string
	var attemptLease int
	var defHash *string
	var untrusted bool
	if err := tx.QueryRow(ctx, `SELECT state, tool, task, attempt_lease_no, tool_def_hash, untrusted_output FROM toolcalls WHERE id = $1 FOR UPDATE`, l.WorkID).
		Scan(&state, &tool, &task, &attemptLease, &defHash, &untrusted); err != nil {
		return err
	}
	if state != "leased" {
		return nil // abandoned or ended meanwhile: the outcome is dropped
	}
	if alive, err := attemptAlive(ctx, tx, task, attemptLease); err != nil {
		return err
	} else if !alive {
		return endToolCall(ctx, tx, now, l.WorkID, "abandoned", "fenced", nil)
	}
	var res mcp.CallResult
	b, err := json.Marshal(output)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, &res); err != nil {
		return refuse("input_schema_invalid", "the output is not a tool result")
	}
	for _, x := range res.Content {
		if x.Type != "text" {
			return refuse("input_schema_invalid", "a tool result holds only text blocks")
		}
	}
	if res.IsError || res.InputRequired {
		// A plugin error, and an "input needed" answer, are call errors (INJ-5).
		return endToolCall(ctx, tx, now, l.WorkID, "error", "exec_failed", nil)
	}
	cat, err := c.Catalog(ctx)
	if err != nil {
		return err
	}
	plugin, name, _ := cutTool(tool)
	pl := cat.Plugins[plugin]
	t, ok := pl.Tool(name)
	// An apply while the call ran may have changed or removed the tool: the output is checked
	// only against the definition the call was queued under, and is dropped otherwise.
	if !ok || defHash == nil || toolDefHash(pl.Placement, t) != *defHash || untrusted != !t.TrustedOutput() {
		return endToolCall(ctx, tx, now, l.WorkID, "error", "plugin_unavailable", nil)
	}
	clean, err := c.cleanOutput(ctx, tx, cat, task, t, res)
	if err != nil {
		var p *Problem
		if errors.As(err, &p) {
			return endToolCall(ctx, tx, now, l.WorkID, "error", p.Reason, nil)
		}
		return err
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	if err := endToolCall(ctx, tx, now, l.WorkID, "done", "", raw); err != nil {
		return err
	}
	if untrusted {
		// The first untrusted output sets untrusted_seen on the attempt; it never goes back
		// (INJ-13).
		// The first source stays.
		if _, err := tx.Exec(ctx, `UPDATE tasks SET record = record || jsonb_build_object('untrusted_seen', true,
			'untrusted_source', coalesce(record->>'untrusted_source', $2::text)) WHERE id = $1`, task, tool); err != nil {
			return err
		}
	}
	return nil
}

// toolDefHash fingerprints the definition a call is checked against: the plugin placement and
// the whole tool entry of the manifest.
func toolDefHash(placement string, t toolspec.Tool) string {
	b, _ := json.Marshal(struct {
		Placement string        `json:"placement"`
		Tool      toolspec.Tool `json:"tool"`
	}{placement, t})
	return jcs.HashBytes(b)
}

// cleanOutput is the core's second check of a tool output (INJ-4, SEC-1).
func (c *Core) cleanOutput(ctx context.Context, tx pgx.Tx, cat *Catalog, task string, t toolspec.Tool, res mcp.CallResult) (*mcp.CallResult, error) {
	limit := t.OutputLimitKB * 1024
	if limit == 0 {
		limit = defaultOutputLimit
	}
	// The text blocks are joined before the secret check, so a secret split across blocks is
	// still found; the model sees them joined anyway.
	var joined strings.Builder
	for _, x := range res.Content {
		joined.WriteString(x.Text)
	}
	text, _ := redact.Cut(redact.Secrets(joined.String()), limit)
	out := &mcp.CallResult{Content: []mcp.Content{}}
	if text != "" {
		out.Content = append(out.Content, mcp.Content{Type: "text", Text: text})
	}
	if res.StructuredContent != nil {
		sc, _ := redact.Value(res.StructuredContent, []string{redact.Secret}).(map[string]any)
		// Structured output is bounded by the same tool limit; it cannot be cut and stay valid.
		if b, err := json.Marshal(sc); err != nil || len(b) > limit {
			return nil, refuse("output_schema_invalid", "the structured output is larger than the tool's output limit")
		}
		out.StructuredContent = sc
	}
	if t.OutputSchema != nil {
		if out.StructuredContent == nil {
			return nil, refuse("output_schema_invalid", "the tool declares an output schema")
		}
		var rec []byte
		var tenant string
		if err := tx.QueryRow(ctx, `SELECT tenant, record FROM tasks WHERE id = $1`, task).Scan(&tenant, &rec); err != nil {
			return nil, err
		}
		var r struct {
			DataMark string `json:"data_mark"`
		}
		_ = json.Unmarshal(rec, &r)
		names := map[string]bool{}
		enumFromNames(t.OutputSchema, names)
		full, err := c.fullSchema(t.OutputSchema, c.resolveEnumFrom(cat, tenant, r.DataMark, names))
		if err != nil {
			return nil, err
		}
		if err := c.rs.validate(full, out.StructuredContent); err != nil {
			return nil, refuse("output_schema_invalid", "the output does not match the tool's output_schema")
		}
	}
	return out, nil
}

// failToolcall ends a call with the toolhost's reason; the core does not retry it (DLV-1).
func failToolcall(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, reason string) error {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = $1 FOR UPDATE`, l.WorkID).Scan(&state); err != nil {
		return err
	}
	if state != "leased" {
		return nil
	}
	return endToolCall(ctx, tx, now, l.WorkID, "error", reason, nil)
}

// toolcallCanceled says whether the call under a lease was abandoned or ended, so the
// toolhost stops it (FEN-4, FEN-6).
func toolcallCanceled(ctx context.Context, tx pgx.Tx, id string) (bool, error) {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = $1`, id).Scan(&state); err != nil {
		return false, err
	}
	return state != "leased", nil
}

// toolcallDeadlines ends the calls nobody took before start_by with no_executor (DLV-16) and
// abandons the read calls whose attempt lost its lease (FEN-4).
func (c *Core) toolcallDeadlines(ctx context.Context) error {
	return c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		tag, err := tx.Exec(ctx, `UPDATE toolcalls SET state = 'error', reason = 'no_executor', updated_at = $1, ended_at = $1
			WHERE state = 'queued' AND start_by <= $1`, now)
		if err != nil {
			return err
		}
		tag2, err := tx.Exec(ctx, `UPDATE toolcalls tc SET state = 'abandoned', reason = 'fenced', updated_at = $1, ended_at = $1
			WHERE tc.state IN ('queued', 'leased') AND tc.access = 'read' AND NOT EXISTS (SELECT 1 FROM leases l
				WHERE l.work_id = tc.task AND l.lease_no = tc.attempt_lease_no AND l.state = 'active')`, now)
		if err != nil {
			return err
		}
		if tag.RowsAffected()+tag2.RowsAffected() > 0 {
			return notifyWork(ctx, tx)
		}
		return nil
	})
}

// reapToolcall ends a call whose toolhost lease expired: a read gets error(lease_expired), or
// error(core_unavailable) when the lease ran out during core downtime (DLV-1, FEN-5).
func reapToolcall(ctx context.Context, tx pgx.Tx, now time.Time, l *leaseRow, reason string) error {
	var state string
	if err := tx.QueryRow(ctx, `SELECT state FROM toolcalls WHERE id = $1 FOR UPDATE`, l.WorkID).Scan(&state); err != nil {
		return err
	}
	if state != "leased" {
		return nil
	}
	return endToolCall(ctx, tx, now, l.WorkID, "error", reason, nil)
}
