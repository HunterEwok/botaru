// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// commitLines are the proposal lines of a commit message (PRP-19, PRP-20).
type commitLines struct {
	Proposals []string
	Reverts   map[string]string // proposal -> reason
}

func (l commitLines) ids() []string {
	out := slices.Clone(l.Proposals)
	for id := range l.Reverts {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}

var (
	reProposalLine = regexp.MustCompile(`(?m)^Botaru-Proposal:[ \t]*([a-z]+_[0-9A-Za-z]+)[ \t]*$`)
	reRevertLine   = regexp.MustCompile(`(?m)^Botaru-Revert:[ \t]*([a-z]+_[0-9A-Za-z]+)(?:[ \t]+([a-z_]+))?[ \t]*$`)
	revertReasons  = []string{"harm_observed", "superseded", "other"}
)

// parseCommitLines reads Botaru-Proposal: <id> and Botaru-Revert: <id> <reason>; a revert
// without a reason, or with one outside the closed list, is read as other (PRP-20).
func parseCommitLines(msg string) commitLines {
	l := commitLines{Reverts: map[string]string{}}
	for _, m := range reProposalLine.FindAllStringSubmatch(msg, -1) {
		if !slices.Contains(l.Proposals, m[1]) {
			l.Proposals = append(l.Proposals, m[1])
		}
	}
	for _, m := range reRevertLine.FindAllStringSubmatch(msg, -1) {
		reason := m[2]
		if !slices.Contains(revertReasons, reason) {
			reason = "other"
		}
		l.Reverts[m[1]] = reason
	}
	return l
}

// proposalScope refuses a set whose commit names a proposal while it changes an object
// outside that proposal's tenant (PRP-8, MAN-5): the whole apply is refused.
func (c *Core) proposalScope(ctx context.Context, l commitLines, docs []manifest.Doc, del []ApplyDelete, read map[manifest.Key]string) (*Problem, error) {
	ids := l.ids()
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := c.st.Pool.Query(ctx, `SELECT DISTINCT tenant FROM proposals WHERE id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	tenants, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	if len(tenants) == 0 {
		return nil, nil
	}
	if len(tenants) > 1 {
		return refuse("proposal_scope", "the commit names proposals of several tenants"), nil
	}
	for _, d := range docs {
		h, err := jcs.Hash(d.Value)
		if err != nil {
			return nil, err
		}
		if read[d.Key()] != h && d.Tenant != tenants[0] {
			return refuse("proposal_scope", "the commit of a proposal of %s changes %s %s outside its folder", tenants[0], d.Kind, d.Name), nil
		}
	}
	for _, x := range del {
		if _, ok := read[manifest.Key{Kind: x.Kind, Tenant: x.Tenant, Name: x.Name}]; ok && x.Tenant != tenants[0] {
			return refuse("proposal_scope", "the commit of a proposal of %s deletes %s %s outside its folder", tenants[0], x.Kind, x.Name), nil
		}
	}
	return nil, nil
}

// keepVersion keeps the stored version of a manifest an apply replaces or deletes.
func keepVersion(ctx context.Context, tx pgx.Tx, now time.Time, k manifest.Key, commit string) error {
	_, err := tx.Exec(ctx, `INSERT INTO manifest_versions (kind, tenant, name, doc, commit, replaced_by, replaced_at)
		SELECT kind, tenant, name, doc, commit, $4, $5 FROM manifests WHERE kind = $1 AND tenant = $2 AND name = $3`, k.Kind, k.Tenant, k.Name, nullable(commit), now)
	return err
}

// storedDoc is the applied manifest of a key, and the version that the given commit replaced.
func storedDoc(ctx context.Context, tx pgx.Tx, kind, tenant, name string) (map[string]any, string, error) {
	var raw []byte
	var commit string
	err := tx.QueryRow(ctx, `SELECT doc, commit FROM manifests WHERE kind = $1 AND tenant = $2 AND name = $3`, kind, tenant, name).Scan(&raw, &commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", err
	}
	var doc map[string]any
	return doc, commit, json.Unmarshal(raw, &doc)
}

func replacedDoc(ctx context.Context, tx pgx.Tx, kind, tenant, name, commit string) (map[string]any, error) {
	var raw []byte
	err := tx.QueryRow(ctx, `SELECT doc FROM manifest_versions WHERE kind = $1 AND tenant = $2 AND name = $3 AND replaced_by = $4
		ORDER BY replaced_at DESC LIMIT 1`, kind, tenant, name, commit).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	return doc, json.Unmarshal(raw, &doc)
}

// linkCommit links an applied commit to the proposals its lines name (PRP-19, PRP-20).
func (c *Core) linkCommit(ctx context.Context, tx pgx.Tx, now time.Time, commit string, l commitLines) error {
	batch := len(l.Proposals) > 1
	for _, id := range l.Proposals {
		p, err := lockedProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		if p == nil {
			// An unknown id is skipped; the other lines still count.
			continue
		}
		if _, err := c.realizeByCommit(ctx, tx, now, p, commit, "commit_trailer", batch); err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(mapKeys(l.Reverts)) {
		p, err := lockedProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		if p == nil {
			continue
		}
		if err := c.revertByCommit(ctx, tx, now, p, commit, l.Reverts[id]); err != nil {
			return err
		}
	}
	return nil
}

func lockedProposal(ctx context.Context, tx pgx.Tx, id string) (*proposalRow, error) {
	p, err := loadProposal(ctx, tx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// realizeByCommit sets realized when the commit is the applied commit of every target object
// and every field of the change holds the accepted value; it keeps the reverse edit from the
// versions the commit replaced and starts the trial window. ok says whether it linked.
func (c *Core) realizeByCommit(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow, commit, link string, batch bool) (bool, error) {
	b := p.body()
	if p.State != "realizing" || b.Change.Kind != "manifest_change" || b.Change.ManifestChange == nil || commit == "" {
		return false, nil
	}
	var patch, absent []any
	for _, op := range b.Change.ManifestChange.Ops {
		doc, applied, err := storedDoc(ctx, tx, op.Target.Kind, p.Tenant, op.Target.Name)
		if err != nil {
			return false, err
		}
		cur, ok := pathValue(doc, op.Field)
		if doc == nil || applied != commit || !ok || fmt.Sprint(cur) != fmt.Sprint(op.Value) {
			return false, nil
		}
		prev, err := replacedDoc(ctx, tx, op.Target.Kind, p.Tenant, op.Target.Name, commit)
		if err != nil {
			return false, err
		}
		tg := map[string]any{"kind": op.Target.Kind, "name": op.Target.Name}
		if old, ok := pathValue(prev, op.Field); ok && scalar(old) {
			patch = append(patch, map[string]any{"target": tg, "field": op.Field, "previous": old})
		} else if !ok && prev != nil {
			// The change added the field: its revert takes it out (EVO-10).
			absent = append(absent, map[string]any{"target": tg, "field": op.Field})
		}
	}
	r := mmapAny(p.Rec["realization"])
	if r == nil {
		return false, nil
	}
	r["commit"], r["link"], r["realized_at"] = commit, link, fmtTime(now)
	if len(patch) > 0 {
		r["revert_patch"] = patch
	}
	if len(absent) > 0 {
		r["revert_absent"] = absent
	}
	if batch {
		r["batch"] = commit[:12]
	}
	if t := mmapAny(p.Rec["trial"]); t != nil {
		t["state"] = "running"
		mmapAny(t["window"])["starts_at"] = fmtTime(now)
	}
	if err := c.moveProposal(ctx, tx, now, p, "realized", ""); err != nil {
		return false, err
	}
	return true, c.planProposalCardUpdates(ctx, tx, now, p)
}

func scalar(v any) bool {
	switch x := v.(type) {
	case string:
		return len(x) <= 200
	case float64, int, int64, bool:
		return true
	}
	return false
}

// revertByCommit moves a realized proposal to reverted when the commit returns every field of
// its reverse edit; the trial ends worse after a broken guard or for harm_observed, else
// canceled (PRP-20, PRP-22).
func (c *Core) revertByCommit(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow, commit, reason string) error {
	r := mmapAny(p.Rec["realization"])
	patch, absent := asList(r["revert_patch"]), asList(r["revert_absent"])
	if p.State != "realized" || len(patch)+len(absent) == 0 {
		return nil
	}
	for _, e := range absent {
		em := mmapAny(e)
		tg := mmapAny(em["target"])
		doc, applied, err := storedDoc(ctx, tx, asString(tg["kind"]), p.Tenant, asString(tg["name"]))
		if err != nil {
			return err
		}
		if _, ok := pathValue(doc, asString(em["field"])); applied != commit || ok {
			return nil
		}
	}
	for _, e := range patch {
		em := mmapAny(e)
		tg := mmapAny(em["target"])
		doc, applied, err := storedDoc(ctx, tx, asString(tg["kind"]), p.Tenant, asString(tg["name"]))
		if err != nil {
			return err
		}
		cur, ok := pathValue(doc, asString(em["field"]))
		if applied != commit || !ok || fmt.Sprint(cur) != fmt.Sprint(em["previous"]) {
			return nil
		}
	}
	r["revert_reason"] = reason
	if t := mmapAny(p.Rec["trial"]); t != nil && t["outcome"] == nil {
		out := "canceled"
		if reason == "harm_observed" || guardBroken(t) {
			out = "worse"
		}
		t["state"], t["outcome"] = out, out
	}
	if err := c.moveProposal(ctx, tx, now, p, "reverted", reason); err != nil {
		return err
	}
	return c.planProposalCardUpdates(ctx, tx, now, p)
}

// guardBroken says whether a guard of the trial broke, a farm guard included: a change that
// improved its metric but tripped a farm guard is worse (PRP-22, PRP-26).
func guardBroken(t map[string]any) bool {
	for _, g := range append(asList(t["guards"]), asList(t["farm_guards"])...) {
		if mmapAny(g)["broken_at"] != nil {
			return true
		}
	}
	return false
}

// LinkCommitRequest is P2 proposal.link_commit.
type LinkCommitRequest struct {
	Proposal string `json:"proposal"`
	Commit   string `json:"commit"`
}

// LinkCommit is P2 proposal.link_commit: the executor or a person with
// proposal.decide:<category> links an applied commit to an accepted manifest change; the core
// checks that the commit touched only the tenant's objects and set the accepted values
// (PRP-19, PRP-8).
func (c *Core) LinkCommit(ctx context.Context, op Operator, req LinkCommitRequest) (map[string]any, error) {
	cat, err := c.Catalog(ctx)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		p, err := lockedProposal(ctx, tx, req.Proposal)
		if err != nil {
			return err
		}
		if p == nil || p.Tenant != op.Tenant && op.Tenant != "farm" {
			return refuse("not_found", "no such proposal")
		}
		tv := cat.Tenants[p.Tenant]
		if tv == nil {
			return refuse("not_found", "no such proposal")
		}
		realizer := "human:" + asString(mmapAny(p.Rec["realization"])["realizer"])
		allowed := op.Principal == realizer
		if !allowed {
			info, err := loadPrincipal(ctx, tx, now, p.Tenant, op.Principal)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			allowed = err == nil && info.State == "active" && hasCap(tv, groupClosure(tv, info.Groups), "proposal.decide:"+p.body().Category)
		}
		if !allowed {
			return refuse("forbidden", "only the executor or a person who may decide links a commit")
		}
		// The commit's objects are the current manifests it wrote and the versions it wrote or
		// replaced that a later apply has replaced since.
		var other int
		if err := tx.QueryRow(ctx, `SELECT (SELECT count(*) FROM manifests WHERE commit = $1 AND tenant <> $2)
			+ (SELECT count(*) FROM manifest_versions WHERE (commit = $1 OR replaced_by = $1) AND tenant <> $2)`, req.Commit, p.Tenant).Scan(&other); err != nil {
			return err
		}
		if other > 0 {
			return refuse("proposal_scope", "the commit changed objects outside the tenant's folder")
		}
		ok, err := c.realizeByCommit(ctx, tx, now, p, req.Commit, "link_commit", false)
		if err != nil {
			return err
		}
		if !ok {
			return refuse("forbidden", "the commit is not the applied commit of the change, or a field is not at the accepted value")
		}
		if _, err := c.appendAudit(ctx, tx, now, Audit{
			Tenant: p.Tenant, Kind: "command", Actor: Actor{Principal: op.Principal, Via: "farmctl"},
			Command: "proposal.link_commit", Target: &Target{Kind: "proposal", ID: p.ID}, OK: true, GitRev: req.Commit,
		}); err != nil {
			return err
		}
		out = map[string]any{"proposal": p.ID, "state": p.State}
		return nil
	})
	return out, err
}

// lockKeys are the fields a manifest change sets, as kind/name/field (PRP-9).
func lockKeys(b proposalBody) []string {
	if b.Change.Kind != "manifest_change" || b.Change.ManifestChange == nil {
		return nil
	}
	var out []string
	for _, op := range b.Change.ManifestChange.Ops {
		out = append(out, op.Target.Kind+"/"+op.Target.Name+"/"+op.Field)
	}
	return out
}

// keysOverlap: the same field, or one a section of the other.
func keysOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y || len(x) < len(y) && y[:len(x)+1] == x+"." || len(y) < len(x) && x[:len(y)+1] == y+"." {
				return true
			}
		}
	}
	return false
}

// lockHolders are the proposals of the tenant that hold a lock on one of keys: a manifest
// change in realizing, or realized while its trial runs (PRP-9). A lock also holds the direct
// neighbours of the locked objects in the output coupling graph (EVO-1).
func lockHolders(ctx context.Context, tx pgx.Tx, tenant, except string, keys []string) ([]any, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	rows, err := tx.Query(ctx, `SELECT id, record FROM proposals WHERE tenant = $1 AND id <> $2
		AND (state = 'realizing' OR state = 'realized' AND record->'trial'->>'outcome' IS NULL)
		AND record->'body'->'change'->>'kind' = 'manifest_change' ORDER BY id`, tenant, except)
	if err != nil {
		return nil, err
	}
	var held []*proposalRow
	for rows.Next() {
		q := &proposalRow{}
		var raw []byte
		if err := rows.Scan(&q.ID, &raw); err != nil {
			rows.Close()
			return nil, err
		}
		if err := json.Unmarshal(raw, &q.Rec); err != nil {
			rows.Close()
			return nil, err
		}
		held = append(held, q)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	cg := &couplingOnce{}
	var out []any
	for _, q := range held {
		hit, err := cg.conflict(ctx, tx, tenant, keys, lockKeys(q.body()))
		if err != nil {
			return nil, err
		}
		if hit {
			out = append(out, q.ID)
		}
	}
	return out, nil
}

// evolutionOn says whether the applied Tenant manifest turns on the learning part of the
// scaffold evolution (insights.evolution.enabled; EVO-1, EVO-3, EVO-5).
func evolutionOn(ctx context.Context, tx pgx.Tx, tenant string) (bool, error) {
	doc, _, err := storedDoc(ctx, tx, "Tenant", tenant, tenant)
	if err != nil || doc == nil {
		return false, err
	}
	v, _ := pathValue(doc, "spec.insights.evolution.enabled")
	return v == true, nil
}

// couplingOnce loads the output coupling graph of the applied set at most once per check, and
// only for a tenant with the evolution on.
type couplingOnce struct {
	g       *manifest.Graph
	checked bool
	on      bool
}

// conflict: two manifest changes conflict when they set the same field or one a section of the
// other (PRP-9), or when an object one of them changes is a direct caller or callee of an object
// the other changes (EVO-1).
func (o *couplingOnce) conflict(ctx context.Context, tx pgx.Tx, tenant string, a, b []string) (bool, error) {
	if len(a) == 0 || len(b) == 0 {
		return false, nil
	}
	if keysOverlap(a, b) {
		return true, nil
	}
	if !o.checked {
		on, err := evolutionOn(ctx, tx, tenant)
		if err != nil {
			return false, err
		}
		o.checked, o.on = true, on
	}
	if !o.on {
		return false, nil
	}
	if o.g == nil {
		docs, _, err := loadManifests(ctx, tx)
		if err != nil {
			return false, err
		}
		o.g = manifest.Coupling(docs)
	}
	nb := lockNodes(tenant, b)
	for _, n := range lockNodes(tenant, a) {
		for _, m := range o.g.Neighbours(n) {
			if slices.Contains(nb, m) {
				return true, nil
			}
		}
	}
	return false, nil
}

// lockNodes are the coupling graph nodes the kind/name/field keys of a change touch.
func lockNodes(tenant string, keys []string) []manifest.Node {
	var out []manifest.Node
	for _, k := range keys {
		kind, rest, _ := strings.Cut(k, "/")
		name, field, _ := strings.Cut(rest, "/")
		out = append(out, manifest.FieldNode(kind, tenant, name, field))
	}
	return out
}

// parkConflicts parks the open proposals of the tenant that change a field the realizing
// proposal p now locks, or an object coupled to it (EVO-1); their cards go dark (PRP-9).
func (c *Core) parkConflicts(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	keys := lockKeys(p.body())
	if len(keys) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND id <> $2 AND state IN ('screened', 'recommended', 'presented')
		AND record->'body'->'change'->>'kind' = 'manifest_change' ORDER BY id`, p.Tenant, p.ID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	cg := &couplingOnce{}
	for _, id := range ids {
		q, err := loadProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		// A proposal in review finishes its round; the digest parks it if the lock still holds.
		if !slices.Contains([]string{"screened", "recommended", "presented"}, q.State) {
			continue
		}
		hit, err := cg.conflict(ctx, tx, p.Tenant, keys, lockKeys(q.body()))
		if err != nil {
			return err
		}
		if !hit {
			continue
		}
		q.Rec["conflicts_with"] = []any{p.ID}
		was := q.State
		if err := c.moveProposal(ctx, tx, now, q, "parked", "conflict"); err != nil {
			return err
		}
		if was == "presented" {
			if _, err := tx.Exec(ctx, `UPDATE proposals SET presented_at = NULL WHERE id = $1`, q.ID); err != nil {
				return err
			}
			if err := c.planProposalCardUpdates(ctx, tx, now, q); err != nil {
				return err
			}
		}
	}
	return nil
}
