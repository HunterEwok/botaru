// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

// ApplyRequest is the body of P2 apply (p2-apply-request).
type ApplyRequest struct {
	Commit    string           `json:"commit"`
	Message   string           `json:"message"`
	Manifests []map[string]any `json:"manifests"`
	DryRun    bool             `json:"dry_run"`
	Delete    []ApplyDelete    `json:"delete"`
	// Personas are the texts of the persona files the charters pin, by their SHA-256 (GOV-4).
	Personas map[string]string `json:"personas,omitempty"`
	// Skills are the files of the farm git's skills/<name>/ folders by skill name, and
	// Knowledge the documents of tenants/<t>/knowledge/<corpus>/ (profile knowledge: SKL-1,
	// SKL-2, KNW-2).
	Skills    map[string][]SkillFile `json:"skills,omitempty"`
	Knowledge []KnowledgeFolder      `json:"knowledge,omitempty"`
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
	// Detail says more about some codes, such as the expected fingerprint of eval_report_missing.
	Detail string `json:"detail,omitempty"`
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
	// Warnings are findings that do not fail the set, such as eval_report_missing in a test
	// tenant (PRV-3); an extension field of the answer (MAN-3).
	Warnings []ApplyError `json:"warnings,omitempty"`
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
	// The lint reads the store before the transaction; an apply that lands in between makes
	// this one start again, so that two applies never pass the lint on each other's old state.
	for try := 0; ; try++ {
		res, err := c.applyOnce(ctx, op, req)
		if !errors.Is(err, errStoreChanged) {
			return res, err
		}
		if try == 4 {
			return nil, fmt.Errorf("%w five times in a row; apply again", err)
		}
	}
}

// errStoreChanged: another apply changed the store after this one read it for the lint.
var errStoreChanged = errors.New("store changed during apply")

// storeHashes are the hashes of the stored manifests by key.
func storeHashes(ctx context.Context, q store.Querier) (map[manifest.Key]string, error) {
	out := map[manifest.Key]string{}
	rows, err := q.Query(ctx, `SELECT kind, tenant, name, hash FROM manifests`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k manifest.Key
		var h string
		if err := rows.Scan(&k.Kind, &k.Tenant, &k.Name, &h); err != nil {
			return nil, err
		}
		out[k] = h
	}
	return out, rows.Err()
}

func (c *Core) applyOnce(ctx context.Context, op Operator, req ApplyRequest) (*ApplyResult, error) {
	read, err := storeHashes(ctx, c.st.Pool)
	if err != nil {
		return nil, err
	}
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
			// The id of a deleted tenant is a tombstone and is not used again (LCY-1, RET-3).
			var dead int
			if err := c.st.Pool.QueryRow(ctx, `SELECT count(*) FROM tenant_states WHERE tenant = $1 AND state = 'deleted'`, d.Name).Scan(&dead); err != nil {
				return nil, err
			}
			if dead > 0 {
				e.Code, e.Path = "gone", fmt.Sprintf("/manifests/%d/metadata/name", i)
				res.Errors = append(res.Errors, e)
				continue
			}
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
	// The lint and the catalog check see the store as it will be after this apply: the
	// objects of the store that stay (orphaned ones included) overlaid with this set and its
	// deletions, so that a check across objects does not miss an object of an earlier apply.
	var after []manifest.Doc
	var skillHashes map[string]string
	var kerrs, kwarns []ApplyError
	if len(res.Errors) == 0 {
		stored, _, err := loadManifests(ctx, c.st.Pool)
		if err != nil {
			return nil, err
		}
		gone := map[manifest.Key]bool{}
		for _, del := range req.Delete {
			gone[manifest.Key{Kind: del.Kind, Tenant: del.Tenant, Name: del.Name}] = true
		}
		after = append(after, docs...)
		lintIndex := append([]int(nil), index...)
		var prior []manifest.Doc
		for _, d := range stored {
			if ki := kinds[d.Kind]; ki.scope == "global" {
				d.Tenant = ""
			}
			prior = append(prior, d)
			if k := d.Key(); seen[k] || gone[k] {
				continue
			}
			after = append(after, d)
			lintIndex = append(lintIndex, -1)
		}
		lookup, perrs, err := c.personaTexts(ctx, req.Personas)
		if err != nil {
			return nil, err
		}
		res.Errors = append(res.Errors, perrs...)
		errs, err := c.lintManifests(after, lintIndex, lookup, prior)
		if err != nil {
			return nil, err
		}
		res.Errors = append(res.Errors, errs...)
		// The quality gate on a static fingerprint change (PRV-3).
		gate, warns, err := c.evalGate(after, lintIndex, stored)
		if err != nil {
			return nil, err
		}
		res.Errors = append(res.Errors, gate...)
		res.Warnings = append(res.Warnings, warns...)
		// The skill folders and the corpus folders of the git (profile knowledge: SKL-1, SKL-2,
		// KNW-2).
		skillHashes, kerrs, kwarns, err = c.knowledgeLint(ctx, req, after, lintIndex)
		if err != nil {
			return nil, err
		}
		res.Errors = append(res.Errors, kerrs...)
		res.Warnings = append(res.Warnings, kwarns...)
		// accountability on needs every seat held with enough scored forecasts, on a core that
		// claims v0.3 (GOV-36).
		if claimsV03() {
			current, err := c.Catalog(ctx)
			if err != nil {
				return nil, err
			}
			errs, err = c.accountabilityLint(ctx, docs, index, current)
			if err != nil {
				return nil, err
			}
			res.Errors = append(res.Errors, errs...)
		}
	}
	if len(res.Errors) == 0 {
		if _, err := buildCatalog(after); err != nil {
			res.Errors = append(res.Errors, ApplyError{Code: "decode_failed"})
		}
	}
	if len(res.Errors) > 0 {
		return res, nil
	}
	// The lines Botaru-Proposal and Botaru-Revert of the commit message link the commit to
	// proposals; such a commit changes only the objects of the proposal's tenant (PRP-8).
	links := parseCommitLines(req.Message)
	if !req.DryRun {
		if p, err := c.proposalScope(ctx, links, docs, req.Delete, read); err != nil || p != nil {
			if err != nil {
				return nil, err
			}
			return nil, p
		}
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
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(7300111)`); err != nil {
			return err
		}
		existing, err := storeHashes(ctx, tx)
		if err != nil {
			return err
		}
		if !maps.Equal(existing, read) {
			return errStoreChanged
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
			if ch.Op == "update" {
				if err := keepVersion(ctx, tx, now, k, req.Commit); err != nil {
					return err
				}
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
			if err := keepVersion(ctx, tx, now, k, req.Commit); err != nil {
				return err
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
		// A new configuration may be the fix that tasks waiting on_fix wait for, and that a
		// pair closed by a provider configuration error waits for (GW-13): its next call is
		// the test.
		if _, err := tx.Exec(ctx, `DELETE FROM provider_pairs WHERE state = 'config'`); err != nil {
			return err
		}
		// A new configuration may be the fix that tasks waiting on_fix wait for.
		if _, err := tx.Exec(ctx, `UPDATE tasks SET run_at = $1, reason = NULL, updated_at = $1 WHERE state = 'submitted' AND reason = 'waiting_on_fix'`, now); err != nil {
			return err
		}
		if err := c.linkCommit(ctx, tx, now, req.Commit, links); err != nil {
			return err
		}
		// The commission seats follow the applied charters (GOV-33).
		applied, _, err := loadManifests(ctx, tx)
		if err != nil {
			return err
		}
		next, err := buildCatalog(applied)
		if err != nil {
			return err
		}
		if err := c.syncSeats(ctx, tx, now, next); err != nil {
			return err
		}
		if err := c.applyV03(ctx, tx, now, next); err != nil {
			return err
		}
		// The core copies of the skills and the corpus documents of the set (SKL-2, KNW-2, KNW-5).
		if err := storeSkillTrees(ctx, tx, now, req.Skills, skillHashes); err != nil {
			return err
		}
		if err := c.syncKnowledge(ctx, tx, now, op, next, req); err != nil {
			return err
		}
		if err := recordMetadataPeriods(ctx, tx, now, next); err != nil {
			return err
		}
		// A removed schedule loses its state, so one recreated later starts from its apply
		// and not from the slot its old self handled (TIM-3).
		var schedTenants, schedNames []string
		for t, tv := range next.Tenants {
			for n := range tv.Schedules {
				schedTenants, schedNames = append(schedTenants, t), append(schedNames, n)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schedule_state s WHERE NOT EXISTS (SELECT 1 FROM unnest($1::text[], $2::text[]) k(tenant, name)
			WHERE k.tenant = s.tenant AND k.name = s.name)`, schedTenants, schedNames); err != nil {
			return err
		}
		if err := c.inspectPurposeOff(ctx, tx, now, next); err != nil {
			return err
		}
		if err := c.managerDisabled(ctx, tx, now, next); err != nil {
			return err
		}
		// An inbox the new set no longer has loses its live entries (recipient_gone).
		if err := c.inboxesGone(ctx, tx, now, next); err != nil {
			return err
		}
		// The texts go to every charter of the store that pins them, the ones applied earlier
		// included.
		if err := storePersonas(ctx, tx, now, after, req.Personas, claimsV03()); err != nil {
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

// personaTexts checks the persona texts of an apply against their hashes and gives the lint
// these texts with the ones the store keeps (GOV-4).
func (c *Core) personaTexts(ctx context.Context, sent map[string]string) (personaLookup, []ApplyError, error) {
	var errs []ApplyError
	for sha, text := range sent {
		if personaHash(text) != sha {
			errs = append(errs, ApplyError{Code: "persona_hash", Path: "/personas/" + sha})
		}
	}
	stored := map[[2]string]string{}
	rows, err := c.st.Pool.Query(ctx, `SELECT tenant, sha256, body FROM personas`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t, sha, body string
		if err := rows.Scan(&t, &sha, &body); err != nil {
			return nil, nil, err
		}
		stored[[2]string{t, sha}] = body
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return func(tenant, sha string) (string, bool) {
		if t, ok := sent[sha]; ok && personaHash(t) == sha {
			return t, true
		}
		t, ok := stored[[2]string{tenant, sha}]
		return t, ok
	}, errs, nil
}

// storePersonas keeps the texts of the personas the applied charters pin, each in its tenant.
func storePersonas(ctx context.Context, tx pgx.Tx, now time.Time, docs []manifest.Doc, sent map[string]string, v03 bool) error {
	for _, d := range docs {
		if d.Kind != "Governance" {
			continue
		}
		spec, _ := d.Value["spec"].(map[string]any)
		for _, r := range charterPersonas(spec, v03) {
			text, ok := sent[r.SHA256]
			if !r.pinned() || !ok {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO personas (tenant, sha256, body, created_at) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING`,
				d.Tenant, r.SHA256, text, now); err != nil {
				return err
			}
		}
	}
	return nil
}
