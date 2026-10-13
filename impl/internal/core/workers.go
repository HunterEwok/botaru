// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
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
	Hello  *HelloRequest     `json:"-"`
}

// EnrollRequest is p2-worker-enroll-request.
type EnrollRequest struct {
	Tenant   string            `json:"tenant"`
	Role     string            `json:"role"`
	Kind     string            `json:"kind"`
	Labels   map[string]string `json:"labels"`
	Target   string            `json:"target"`
	RotateP2 bool              `json:"rotate_p2"`
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
	case "channel":
		ch, ok := tv.Channels[req.Target]
		if !ok {
			return nil, refuse("not_found", "no such channel in the tenant")
		}
		driver = ch.Driver
	case "sink", "monitor", "toolhost":
		if req.Kind != "toolhost" && req.Target == "" {
			return nil, refuse("input_schema_invalid", "target is required")
		}
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
	w := Worker{ID: ids.New(ids.Worker), Tenant: req.Tenant, Role: req.Role, Kind: req.Kind, Target: req.Target, State: "enrolled", Labels: labels}
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		lb, _ := json.Marshal(labels)
		if _, err := tx.Exec(ctx, `INSERT INTO workers (id, tenant, role, kind, labels, target, state, token_hash, created_at) VALUES ($1, $2, $3, $4, $5, $6, 'enrolled', $7, $8)`,
			w.ID, w.Tenant, w.Role, w.Kind, lb, nullable(w.Target), c.hashSecret(token), now); err != nil {
			return err
		}
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
		_, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: req.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "worker.enroll", Target: &Target{Kind: "worker", ID: w.ID}, OK: true,
			After: map[string]any{"kind": w.Kind, "role": w.Role},
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
	var labels, hello []byte
	var target *string
	err := c.st.Pool.QueryRow(ctx, `UPDATE workers SET last_seen = botaru_now() WHERE token_hash = $1
		RETURNING id, tenant, role, kind, target, state, labels, hello`, c.hashSecret(token)).
		Scan(&w.ID, &w.Tenant, &w.Role, &w.Kind, &target, &w.State, &labels, &hello)
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
	for k, v := range req.Labels {
		if w.Labels[k] != v {
			mismatch = true
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
		if _, err := tx.Exec(ctx, `UPDATE workers SET hello = $2, state = CASE WHEN state = 'draining' THEN state ELSE $3 END WHERE id = $1`,
			w.ID, raw, state); err != nil {
			return err
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
		return nil, refuse("blocked", "hello does not match the enroll record")
	}
	return &HelloResult{WorkerID: w.ID, Tenant: w.Tenant, Role: w.Role, LeaseS: c.cfg.LeaseS, HeartbeatS: c.cfg.HeartbeatS}, nil
}
