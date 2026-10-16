// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/ids"
)

// Worker is an enrolled worker as the core sees it after authentication.
type Worker struct {
	ID     string            `json:"id"`
	Tenant string            `json:"tenant"`
	Role   string            `json:"role"`
	Kind   string            `json:"kind"`
	Target string            `json:"target,omitempty"`
	State  string            `json:"state"`
	Labels map[string]string `json:"labels,omitempty"`
	// Plugins, Placement and Host are the enroll record of a toolhost (DEP-2, TEN-3a).
	Plugins   []string      `json:"plugins,omitempty"`
	Placement string        `json:"placement,omitempty"`
	Host      string        `json:"host,omitempty"`
	Hello     *HelloRequest `json:"-"`
	// Bot is the identity bound at enroll (IDN-8).
	Bot string `json:"bot,omitempty"`
}

// EnrollRequest is p2-worker-enroll-request.
type EnrollRequest struct {
	Tenant    string            `json:"tenant"`
	Role      string            `json:"role"`
	Kind      string            `json:"kind"`
	Labels    map[string]string `json:"labels"`
	Target    string            `json:"target"`
	RotateP2  bool              `json:"rotate_p2"`
	Bot       string            `json:"bot"`
	Plugins   []string          `json:"plugins"`
	Placement string            `json:"placement"`
	Host      string            `json:"host"`
}

// EnrollResult is p2-worker-enroll-response.
type EnrollResult struct {
	Worker  Worker `json:"worker"`
	Token   string `json:"token"`
	KeyID   string `json:"key_id,omitempty"`
	P2Token string `json:"p2_token,omitempty"`
}

// Enroll registers a worker and issues its P1 token, shown once and stored as a keyed hash
// (DEP-2, IDN-4). The first channel process of a driver in a tenant also gets the P2 secret of
// the driver (D-87 (3)).
func (c *Core) Enroll(ctx context.Context, op Operator, req EnrollRequest) (*EnrollResult, error) {
	if req.Tenant != op.Tenant {
		return nil, refuse("forbidden", "the operator credential is for another tenant")
	}
	tv, cat, err := c.requireTenant(ctx, req.Tenant)
	if err != nil {
		return nil, err
	}
	driver := ""
	switch req.Kind {
	case "agent", "job":
		role, ok := cat.Roles[req.Role]
		if !ok {
			return nil, refuse("not_found", "no such role")
		}
		if role.WorkerKind != req.Kind {
			return nil, refuse("input_schema_invalid", "the role is for worker kind %s", role.WorkerKind)
		}
		// A seat role works only in a tenant with the farm manager (MGR-42).
		if slices.Contains(managerProfiles, profileOf(cat, req.Role)) && !managerEnabled(cat, req.Tenant) {
			return nil, refuse("manager_disabled", "the farm manager is disabled in the tenant")
		}
	case "channel":
		ch, ok := tv.Channels[req.Target]
		if !ok {
			return nil, refuse("not_found", "no such channel in the tenant")
		}
		driver = ch.Driver
	case "sink", "monitor":
		if req.Target == "" {
			return nil, refuse("input_schema_invalid", "target is required")
		}
	case "toolhost":
		if err := c.checkToolhostEnroll(cat, req); err != nil {
			return nil, err
		}
	}
	if req.Kind != "toolhost" && (len(req.Plugins) > 0 || req.Placement != "" || req.Host != "") {
		return nil, refuse("input_schema_invalid", "plugins, placement and host are only for a toolhost")
	}
	if req.Kind != "channel" && req.Kind != "sink" && req.Kind != "monitor" && req.Target != "" {
		return nil, refuse("input_schema_invalid", "target is only for a channel, a sink or a monitoring adapter")
	}
	token := "bw1_" + ids.Secret(32)
	res := &EnrollResult{Token: token}
	labels := req.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	plugins := slices.Clone(req.Plugins)
	slices.Sort(plugins)
	w := Worker{ID: ids.New(ids.Worker), Tenant: req.Tenant, Role: req.Role, Kind: req.Kind, Target: req.Target, State: "enrolled", Labels: labels,
		Plugins: plugins, Placement: req.Placement, Host: req.Host}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// An offboarding tenant gets only the sink worker of an export, as only export is
		// allowed; a deleted one gets none (LCY-1).
		switch state, _, err := tenantState(ctx, tx, req.Tenant); {
		case err != nil:
			return err
		case state == "deleted":
			return refuse("gone", "the tenant is deleted")
		case state == "offboarding":
			if _, _, err := exportSink(cat, req.Tenant, req.Target); req.Kind != "sink" || err != nil {
				return refuse("forbidden", "an offboarding tenant enrolls only the sink worker of an export")
			}
		}
		lb, _ := json.Marshal(labels)
		pb, _ := json.Marshal(nonNil(plugins))
		if _, err := tx.Exec(ctx, `INSERT INTO workers (id, tenant, role, kind, labels, target, plugins, placement, host, state, token_hash, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'enrolled', $10, $11)`,
			w.ID, w.Tenant, w.Role, w.Kind, lb, nullable(w.Target), pb, nullable(w.Placement), nullable(w.Host), c.hashSecret(token), now); err != nil {
			return err
		}
		// A worker of a model role is bound to a new identity, or to the unbound one named.
		if req.Bot != "" && req.Kind != "agent" && req.Kind != "job" {
			return refuse("input_schema_invalid", "bot is only for an agent or a job worker")
		}
		bot, err := c.enrollBot(ctx, tx, now, cat, w, req.Bot)
		if err != nil {
			return err
		}
		w.Bot = bot
		if driver != "" {
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM driver_secrets WHERE tenant = $1 AND driver = $2`, req.Tenant, driver).Scan(&n); err != nil {
				return err
			}
			if n == 0 || req.RotateP2 {
				p2 := "bc2_" + ids.Secret(32)
				if req.RotateP2 {
					// The previous secret stays valid for the rotation window of one day.
					if _, err := tx.Exec(ctx, `UPDATE driver_secrets SET valid_until = $3 WHERE tenant = $1 AND driver = $2 AND valid_until IS NULL`,
						req.Tenant, driver, now.Add(24*time.Hour)); err != nil {
						return err
					}
				}
				if _, err := tx.Exec(ctx, `INSERT INTO driver_secrets (tenant, driver, hash, created_at) VALUES ($1, $2, $3, $4)`,
					req.Tenant, driver, c.hashSecret(p2), now); err != nil {
					return err
				}
				res.P2Token = p2
			}
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: req.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "worker.enroll", Target: &Target{Kind: "worker", ID: w.ID}, OK: true,
			After: enrollAfter(w),
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	res.Worker = w
	return res, nil
}

// AuthenticateWorker finds the worker of a P1 token and updates its last_seen (STA-2).
func (c *Core) AuthenticateWorker(ctx context.Context, token string) (*Worker, error) {
	if token == "" {
		return nil, refuse("unauthenticated", "no worker token")
	}
	var w Worker
	var labels, hello, plugins []byte
	var target, placement, host *string
	// A token hashed under a previous key of the rotation window is rehashed under the current
	// one at its first use, so it outlives the window (SEC-5).
	hashes := c.secretHashes(token)
	err := c.st.Pool.QueryRow(ctx, `UPDATE workers SET last_seen = botaru_now(), token_hash = $2 WHERE token_hash = ANY($1)
		RETURNING id, tenant, role, kind, target, state, labels, hello, plugins, placement, host`, hashes, hashes[0]).
		Scan(&w.ID, &w.Tenant, &w.Role, &w.Kind, &target, &w.State, &labels, &hello, &plugins, &placement, &host)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, refuse("unauthenticated", "unknown worker token")
	}
	if err != nil {
		return nil, err
	}
	if target != nil {
		w.Target = *target
	}
	_ = json.Unmarshal(labels, &w.Labels)
	_ = json.Unmarshal(plugins, &w.Plugins)
	if placement != nil {
		w.Placement = *placement
	}
	if host != nil {
		w.Host = *host
	}
	if len(hello) > 0 {
		var h HelloRequest
		if json.Unmarshal(hello, &h) == nil {
			w.Hello = &h
		}
	}
	if w.State == "revoked" {
		return nil, refuse("unauthenticated", "the worker token is revoked")
	}
	return &w, nil
}

// HelloRequest is p1-hello-request.
type HelloRequest struct {
	Kind             string            `json:"kind"`
	Implementation   string            `json:"implementation,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	Capacity         int               `json:"capacity,omitempty"`
	ProtocolVersions []string          `json:"protocol_versions"`
	Driver           *DriverCaps       `json:"driver,omitempty"`
	Runtime          *RuntimeDecl      `json:"runtime,omitempty"`
	Plugins          []string          `json:"plugins,omitempty"`
	Placement        string            `json:"placement,omitempty"`
	ToolsFingerprint string            `json:"tools_fingerprint,omitempty"`
	Health           []ComponentHealth `json:"health,omitempty"`
}

// ComponentHealth is health-report: the state of one component by codes only (OBS-10, OBS-1).
type ComponentHealth struct {
	Component string `json:"component"`
	Name      string `json:"name,omitempty"`
	State     string `json:"state"`
	Checks    []struct {
		Code  string `json:"code"`
		State string `json:"state"`
	} `json:"checks,omitempty"`
	Time string `json:"time"`
}

// RuntimeDecl is how an agent's runtime performs the farm mechanisms (RT-1, RT-2, RT-5).
type RuntimeDecl struct {
	Session          string   `json:"session,omitempty"`
	Memory           string   `json:"memory,omitempty"`
	ModelAttribution string   `json:"model_attribution,omitempty"`
	Catalog          string   `json:"catalog,omitempty"`
	BuiltInTools     []string `json:"built_in_tools,omitempty"`
	Skills           string   `json:"skills,omitempty"`
	Erasure          []string `json:"erasure,omitempty"`
}

// runtimeRefusal says why a runtime declaration blocks the worker, or "" if it does not.
// A runtime that keeps sessions or memory in its own store must answer erase and match
// (RT-5); this version of the core sends no erasure work to runtimes yet, so it cannot
// reach such a store and blocks every runtime_internal declaration.
func runtimeRefusal(kind string, r *RuntimeDecl) string {
	if r == nil {
		return ""
	}
	if kind != "agent" {
		return "only an agent declares a runtime"
	}
	if len(r.BuiltInTools) > 0 {
		return "a runtime with built-in tools (INJ-9)"
	}
	if r.Session == "runtime_internal" || r.Memory == "runtime_internal" {
		if !slices.Contains(r.Erasure, "erase") || !slices.Contains(r.Erasure, "match") {
			return "runtime_internal without erase and match (RT-5)"
		}
		return "runtime_internal: this core does not yet send erasure work to a runtime (RT-5)"
	}
	return ""
}

// DriverCaps are the capabilities a channel declares at hello.
type DriverCaps struct {
	AIDisclosure        string `json:"ai_disclosure,omitempty"`
	Provenance          string `json:"provenance,omitempty"`
	MembershipAssurance string `json:"membership_assurance,omitempty"`
	StoresContent       string `json:"stores_content,omitempty"`
}

// HelloResult is p1-hello-response.
type HelloResult struct {
	WorkerID   string `json:"worker_id"`
	Tenant     string `json:"tenant"`
	Role       string `json:"role,omitempty"`
	LeaseS     int    `json:"lease_s"`
	HeartbeatS int    `json:"heartbeat_s"`
}

// Hello checks a worker's report against its enroll record (DEP-2, TEN-4, LCY-7). A mismatch
// moves the worker to blocked.
func (c *Core) Hello(ctx context.Context, w *Worker, req HelloRequest) (*HelloResult, error) {
	if !slices.Contains(req.ProtocolVersions, "v1") {
		return nil, refuse("version_unsupported", "the core speaks P1 v1")
	}
	if w.State == "blocked" {
		return nil, refuse("blocked", "the worker is blocked")
	}
	mismatch := req.Kind != w.Kind
	why := "hello does not match the enroll record"
	if r := runtimeRefusal(w.Kind, req.Runtime); r != "" {
		mismatch, why = true, r
	}
	for k, v := range req.Labels {
		if w.Labels[k] != v {
			mismatch = true
		}
	}
	// A toolhost reports the plugins and the placement of its enroll record and the fingerprint
	// of the live tools/list of its plugins, which must match the manifests (DEP-2, INJ-2).
	if w.Kind == "toolhost" && !mismatch {
		cat, err := c.Catalog(ctx)
		if err != nil {
			return nil, err
		}
		if r := toolhostMismatch(cat, w, req); r != "" {
			mismatch, why = true, r
		}
	}
	// A channel whose manifest relies on the messenger's own bot marking skips ai_notice, so its
	// driver must report that marking (DLV-12).
	if w.Kind == "channel" {
		cat, err := c.Catalog(ctx)
		if err != nil {
			return nil, err
		}
		if tv := cat.Tenants[w.Tenant]; tv != nil && tv.Channels[w.Target].AIDisclosure == "native" &&
			(req.Driver == nil || req.Driver.AIDisclosure != "native") {
			mismatch = true
		}
	}
	state := "active"
	if mismatch {
		state = "blocked"
	}
	raw, _ := json.Marshal(req)
	err := c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		// A draining worker stays so while a hold of its own scope keeps no_claim; after that a
		// hello returns it (spec/lifecycles/worker.md).
		drain, err := heldBy(ctx, tx, now, []string{"no_claim"}, scopeRef{kind: "worker", tenant: w.Tenant, name: strings.ToLower(w.ID)})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE workers SET hello = $2, state = CASE WHEN state = 'draining' AND $4 THEN state ELSE $3 END,
			drain_deadline = CASE WHEN state = 'draining' AND $4 THEN drain_deadline END WHERE id = $1`,
			w.ID, raw, state, drain != ""); err != nil {
			return err
		}
		// A hello reports every plugin: one not reported down is ok again.
		if w.Kind == "toolhost" && !mismatch {
			if err := c.pluginHealth(ctx, tx, now, w, req.Health, true); err != nil {
				return err
			}
		}
		if mismatch {
			_, err := c.appendAudit(ctx, tx, now, Audit{
				Tenant: w.Tenant, Kind: "command", Actor: Actor{Principal: workerPrincipal(w.ID), Via: "core"},
				Command: "worker.hello", Target: &Target{Kind: "worker", ID: w.ID}, OK: false, Reason: "blocked",
			})
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if mismatch {
		return nil, refuse("blocked", "%s", why)
	}
	return &HelloResult{WorkerID: w.ID, Tenant: w.Tenant, Role: w.Role, LeaseS: c.cfg.LeaseS, HeartbeatS: c.cfg.HeartbeatS}, nil
}
