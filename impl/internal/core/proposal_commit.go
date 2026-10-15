// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
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
		if err != nil || p == nil {
			return err
		}
		if _, err := c.realizeByCommit(ctx, tx, now, p, commit, "commit_trailer", batch); err != nil {
			return err
		}
	}
	for _, id := range slices.Sorted(mapKeys(l.Reverts)) {
		p, err := lockedProposal(ctx, tx, id)
		if err != nil || p == nil {
			return err
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
	var patch []any
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
		if old, ok := pathValue(prev, op.Field); ok && scalar(old) {
			patch = append(patch, map[string]any{"target": map[string]any{"kind": op.Target.Kind, "name": op.Target.Name}, "field": op.Field, "previous": old})
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
	patch := asList(r["revert_patch"])
	if p.State != "realized" || len(patch) == 0 {
		return nil
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
	if err := c.moveProposal(ctx, tx, now, p, "reverted", ""); err != nil {
		return err
	}
	return c.planProposalCardUpdates(ctx, tx, now, p)
}

func guardBroken(t map[string]any) bool {
	for _, g := range asList(t["guards"]) {
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
		if p == nil {
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
		var other int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM manifests WHERE commit = $1 AND tenant <> $2`, req.Commit, p.Tenant).Scan(&other); err != nil {
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
// change in realizing, or realized while its trial runs (PRP-9).
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
	defer rows.Close()
	var out []any
	for rows.Next() {
		q := &proposalRow{}
		var raw []byte
		if err := rows.Scan(&q.ID, &raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &q.Rec); err != nil {
			return nil, err
		}
		if keysOverlap(keys, lockKeys(q.body())) {
			out = append(out, q.ID)
		}
	}
	return out, rows.Err()
}

// parkConflicts parks the open proposals of the tenant that change a field the realizing
// proposal p now locks; their cards go dark (PRP-9).
func (c *Core) parkConflicts(ctx context.Context, tx pgx.Tx, now time.Time, p *proposalRow) error {
	keys := lockKeys(p.body())
	if len(keys) == 0 {
		return nil
	}
	rows, err := tx.Query(ctx, `SELECT id FROM proposals WHERE tenant = $1 AND id <> $2 AND state IN ('screened', 'in_review', 'recommended', 'presented')
		AND record->'body'->'change'->>'kind' = 'manifest_change' ORDER BY id`, p.Tenant, p.ID)
	if err != nil {
		return err
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	for _, id := range ids {
		q, err := loadProposal(ctx, tx, id)
		if err != nil {
			return err
		}
		if !keysOverlap(keys, lockKeys(q.body())) {
			continue
		}
		q.Rec["conflicts_with"] = []any{p.ID}
		was := q.State
		if err := c.moveProposal(ctx, tx, now, q, "parked", "conflict"); err != nil {
			return err
		}
		if was == "presented" {
			if _, err := tx.Exec(ctx, `UPDATE proposals SET nonce = NULL, presented_at = NULL WHERE id = $1`, q.ID); err != nil {
				return err
			}
			if err := c.planProposalCardUpdates(ctx, tx, now, q); err != nil {
				return err
			}
		}
	}
	return nil
}
