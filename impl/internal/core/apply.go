// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// ApplyRequest is the body of P2 apply (p2-apply-request).
type ApplyRequest struct {
	Commit    string           `json:"commit"`
	Manifests []map[string]any `json:"manifests"`
	DryRun    bool             `json:"dry_run"`
	Delete    []ApplyDelete    `json:"delete"`
}

// ApplyDelete names an object that apply deletes.
type ApplyDelete struct {
	Kind   string `json:"kind"`
	Tenant string `json:"tenant"`
	Name   string `json:"name"`
}

// ApplyError is one entry of the errors list of p2-apply-response.
type ApplyError struct {
	Kind   string `json:"kind,omitempty"`
	Tenant string `json:"tenant,omitempty"`
	Name   string `json:"name,omitempty"`
	Code   string `json:"code"`
	Path   string `json:"path,omitempty"`
}

// ApplyChange is one entry of the changes list.
type ApplyChange struct {
	Op     string `json:"op"`
	Kind   string `json:"kind"`
	Tenant string `json:"tenant,omitempty"`
	Name   string `json:"name"`
}

// ApplyResult is p2-apply-response.
type ApplyResult struct {
	Applied bool          `json:"applied"`
	Commit  string        `json:"commit,omitempty"`
	SetHash string        `json:"set_hash,omitempty"`
	Changes []ApplyChange `json:"changes"`
	Errors  []ApplyError  `json:"errors"`
}

type kindInfo struct {
	scope string
}

func (c *Core) manifestKinds() (map[string]kindInfo, error) {
	reg, err := c.spec.Registry("spec/manifests/kinds.yaml")
	if err != nil {
		return nil, err
	}
	out := map[string]kindInfo{}
	for _, e := range reg.Entries {
		id, _ := e["id"].(string)
		scope, _ := e["scope"].(string)
		out[id] = kindInfo{scope: scope}
	}
	return out, nil
}

// Apply brings the store to a manifest set atomically: the whole set or nothing (MAN-1, MAN-2).
// Objects of the store that are not in the set stay and are reported as orphaned; they are
// deleted only by the explicit delete list (MAN-6).
func (c *Core) Apply(ctx context.Context, op Operator, req ApplyRequest) (*ApplyResult, error) {
	res := &ApplyResult{Commit: req.Commit, Changes: []ApplyChange{}, Errors: []ApplyError{}}
	kinds, err := c.manifestKinds()
	if err != nil {
		return nil, err
	}
	var docs []manifest.Doc
	var index []int
	seen := map[manifest.Key]bool{}
	for i, v := range req.Manifests {
		d := manifest.FromValue(v, fmt.Sprintf("manifests/%d", i))
		e := ApplyError{Kind: d.Kind, Tenant: d.Tenant, Name: d.Name}
		// A kind or a field of a later skeleton version, or of a feature profile this core does
		// not claim, is refused rather than accepted and ignored (MAN-11). A later kind is
		// refused before the schema check, so its answer does not depend on the wrapper.
		later, err := c.spec.Unsupported(v)
		if err != nil {
			return nil, err
		}
		if len(later) == 1 && later[0] == "/kind" {
			e.Code, e.Path = "version_unsupported", fmt.Sprintf("/manifests/%d/kind", i)
			res.Errors = append(res.Errors, e)
			continue
		}
		// The envelope first, with the codes of the skeleton lint (MAN-1, TEN-6).
		ki, ok := kinds[d.Kind]
		md, _ := v["metadata"].(map[string]any)
		_, hasTenant := md["tenant"]
		switch {
		case v["status"] != nil:
			// The core keeps status; git has none (MAN-1).
			e.Code, e.Path = "status_in_git", fmt.Sprintf("/manifests/%d/status", i)
		case !ok:
			e.Code, e.Path = "unknown_kind", fmt.Sprintf("/manifests/%d/kind", i)
		case v["apiVersion"] != "botaru/v1alpha1":
			e.Code, e.Path = "api_version", fmt.Sprintf("/manifests/%d/apiVersion", i)
		case ki.scope == "tenant" && !hasTenant:
			e.Code, e.Path = "tenant_missing", fmt.Sprintf("/manifests/%d/metadata/tenant", i)
		case (ki.scope == "global" || ki.scope == "self") && hasTenant:
			e.Code, e.Path = "tenant_on_global", fmt.Sprintf("/manifests/%d/metadata/tenant", i)
		}
		if e.Code != "" {
			res.Errors = append(res.Errors, e)
			continue
		}
		if err := c.spec.Validate("manifest:manifest", v); err != nil {
			var ve *spec.ValidationError
			if errors.As(err, &ve) && len(ve.Violations) > 0 {
				e.Path = fmt.Sprintf("/manifests/%d%s", i, ve.Violations[0].Path)
			}
			e.Code = "schema_invalid"
			res.Errors = append(res.Errors, e)
			continue
		}
		if len(later) > 0 {
			for _, p := range later {
				e.Code, e.Path = "version_unsupported", fmt.Sprintf("/manifests/%d%s", i, p)
				res.Errors = append(res.Errors, e)
			}
			continue
		}
		if d.Kind == "Tenant" {
			spec, _ := v["spec"].(map[string]any)
			if c.cfg.TestBench && spec["kind"] != "test" {
				// A test bench store never holds a tenant other than kind: test (APR-9).
				e.Code = "test_bench_production"
				res.Errors = append(res.Errors, e)
				continue
			}
		}
		if ki.scope == "global" {
			d.Tenant = ""
		}
		if seen[d.Key()] {
			e.Code = "duplicate_object"
			res.Errors = append(res.Errors, e)
			continue
		}
		seen[d.Key()] = true
		if d.Kind == "ToolPlugin" {
			if errs, err := c.checkPlugin(d, i); err != nil {
				return nil, err
			} else if len(errs) > 0 {
				res.Errors = append(res.Errors, errs...)
				continue
			}
		}
		docs = append(docs, d)
		index = append(index, i)
	}
	if len(res.Errors) == 0 {
		errs, err := c.lintManifests(docs, index)
		if err != nil {
			return nil, err
		}
		res.Errors = append(res.Errors, errs...)
	}
	if len(res.Errors) == 0 {
		if _, err := buildCatalog(docs); err != nil {
			res.Errors = append(res.Errors, ApplyError{Code: "decode_failed"})
		}
	}
	if len(res.Errors) > 0 {
		return res, nil
	}

	sort.Slice(docs, func(i, j int) bool {
		a, b := docs[i].Key(), docs[j].Key()
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Tenant != b.Tenant {
			return a.Tenant < b.Tenant
		}
		return a.Name < b.Name
	})
	values := make([]any, len(docs))
	for i, d := range docs {
		values[i] = d.Value
	}
	setHash, err := jcs.Hash(values)
	if err != nil {
		return nil, err
	}
	res.SetHash = setHash

	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		existing := map[manifest.Key]string{}
		rows, err := tx.Query(ctx, `SELECT kind, tenant, name, hash FROM manifests`)
		if err != nil {
			return err
		}
		for rows.Next() {
			var k manifest.Key
			var h string
			if err := rows.Scan(&k.Kind, &k.Tenant, &k.Name, &h); err != nil {
				rows.Close()
				return err
			}
			existing[k] = h
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if p, err := c.privilegeRaised(ctx, tx, now, docs, req.Delete); err != nil || p != nil {
			if err != nil {
				return err
			}
			return p
		}
		for _, d := range docs {
			h, err := jcs.Hash(d.Value)
			if err != nil {
				return err
			}
			k := d.Key()
			ch := ApplyChange{Kind: k.Kind, Tenant: k.Tenant, Name: k.Name}
			switch old, ok := existing[k]; {
			case !ok:
				ch.Op = "create"
			case old == h:
				ch.Op = "unchanged"
			default:
				ch.Op = "update"
			}
			res.Changes = append(res.Changes, ch)
			if req.DryRun || ch.Op == "unchanged" {
				continue
			}
			raw, err := json.Marshal(d.Value)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO manifests (kind, tenant, name, doc, hash, commit, applied_at) VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (kind, tenant, name) DO UPDATE SET doc = EXCLUDED.doc, hash = EXCLUDED.hash, commit = EXCLUDED.commit, applied_at = EXCLUDED.applied_at`,
				k.Kind, k.Tenant, k.Name, raw, h, req.Commit, now); err != nil {
				return err
			}
			if k.Kind == "Tenant" {
				spec, _ := d.Value["spec"].(map[string]any)
				kind, _ := spec["kind"].(string)
				if _, err := tx.Exec(ctx, `INSERT INTO tenants (name, kind, created_at) VALUES ($1, $2, $3)
					ON CONFLICT (name) DO UPDATE SET kind = EXCLUDED.kind`, k.Name, kind, now); err != nil {
					return err
				}
			}
		}
		deleted := map[manifest.Key]bool{}
		for _, del := range req.Delete {
			k := manifest.Key{Kind: del.Kind, Tenant: del.Tenant, Name: del.Name}
			if _, ok := existing[k]; !ok || seen[k] {
				continue
			}
			deleted[k] = true
			res.Changes = append(res.Changes, ApplyChange{Op: "delete", Kind: k.Kind, Tenant: k.Tenant, Name: k.Name})
			if req.DryRun {
				continue
			}
			if k.Kind == "Tenant" {
				return refuse("forbidden", "a tenant is retired, not deleted")
			}
			if k.Kind == "TaskType" {
				var open bool
				if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE type = $1 AND state IN ('submitted', 'working'))`, k.Name).Scan(&open); err != nil {
					return err
				}
				if open {
					return refuse("forbidden", "task type %s still has tasks that are not finished", k.Name)
				}
			}
			if _, err := tx.Exec(ctx, `DELETE FROM manifests WHERE kind = $1 AND tenant = $2 AND name = $3`, k.Kind, k.Tenant, k.Name); err != nil {
				return err
			}
		}
		for k := range existing {
			if !seen[k] && !deleted[k] {
				res.Changes = append(res.Changes, ApplyChange{Op: "orphaned", Kind: k.Kind, Tenant: k.Tenant, Name: k.Name})
			}
		}
		if req.DryRun {
			return nil
		}
		// The set hash names what the store holds now, so that other core copies reload.
		if _, err := tx.Exec(ctx, `INSERT INTO apply_status (id, commit, set_hash, applied_at) VALUES (1, $1, $2, $3)
			ON CONFLICT (id) DO UPDATE SET commit = EXCLUDED.commit, set_hash = EXCLUDED.set_hash, applied_at = EXCLUDED.applied_at`,
			req.Commit, setHash+":"+fmtTime(now), now); err != nil {
			return err
		}
		// A new configuration may be the fix that tasks waiting on_fix wait for.
		if _, err := tx.Exec(ctx, `UPDATE tasks SET run_at = $1, reason = NULL, updated_at = $1 WHERE state = 'submitted' AND reason = 'waiting_on_fix'`, now); err != nil {
			return err
		}
		if err := notifyWork(ctx, tx); err != nil {
			return err
		}
		_, err = c.appendAudit(ctx, tx, now, Audit{
			Tenant: "farm", Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "apply", GitRev: req.Commit, OK: true,
			After: map[string]any{"set_hash": setHash, "objects": len(docs)},
		})
		return err
	})
	if err != nil {
		return nil, err
	}
	res.Applied = !req.DryRun
	if res.Applied {
		cat, err := c.Catalog(ctx)
		if err != nil {
			return nil, err
		}
		c.warnNoticeFallbacks(cat)
	}
	return res, nil
}

// privilegeRaised refuses a set that makes a group privileged while it has members: they would
// gain the privilege without the verification that grant asks for (PRM-13). Such a group is
// emptied first and granted again after the change.
func (c *Core) privilegeRaised(ctx context.Context, tx pgx.Tx, now time.Time, docs []manifest.Doc, del []ApplyDelete) (*Problem, error) {
	cur, _, err := loadManifests(ctx, tx)
	if err != nil {
		return nil, err
	}
	next := map[manifest.Key]manifest.Doc{}
	for _, d := range cur {
		next[d.Key()] = d
	}
	for _, d := range docs {
		next[d.Key()] = d
	}
	for _, d := range del {
		delete(next, manifest.Key{Kind: d.Kind, Tenant: d.Tenant, Name: d.Name})
	}
	var nextDocs []manifest.Doc
	for _, d := range next {
		nextDocs = append(nextDocs, d)
	}
	before, err := buildCatalog(cur)
	if err != nil {
		return nil, err
	}
	after, err := buildCatalog(nextDocs)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `SELECT DISTINCT tenant, grp FROM memberships WHERE state = 'active' AND (until IS NULL OR until > $1) ORDER BY tenant, grp`, now)
	if err != nil {
		return nil, err
	}
	var held [][2]string
	for rows.Next() {
		var t, g string
		if err := rows.Scan(&t, &g); err != nil {
			rows.Close()
			return nil, err
		}
		held = append(held, [2]string{t, g})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, h := range held {
		tb, ta := before.Tenants[h[0]], after.Tenants[h[0]]
		if tb == nil || ta == nil {
			continue
		}
		if !privilegedGroup(tb, before, h[1], map[string]bool{}) && privilegedGroup(ta, after, h[1], map[string]bool{}) {
			return refuse("privileged_group", "group %s of tenant %s becomes privileged while it has members", h[1], h[0]), nil
		}
	}
	return nil, nil
}
