// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// A person's request for the model paragraph of an incident's analysis from v0.3 (MGR-53):
// the command /incident analyse, the request kept in the incident record and the paragraph
// turned on in an analysis that waits for its worker.

// paragraphRecent says whether an analysis of the same class and object got a model paragraph in
// the last 7 days, other than the task except (MGR-53).
func paragraphRecent(ctx context.Context, tx pgx.Tx, now time.Time, r *incidentRow, except string) (bool, error) {
	var recent bool
	err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks t JOIN incidents i ON i.id = t.record->>'postmortem'
		WHERE t.tenant = $1 AND t.type = $2 AND (t.record->>'paragraph')::boolean AND t.created_at > $3 AND i.class = $4 AND i.subject_kind = $5 AND i.subject_id = $6
		AND t.id <> $7)`,
		r.Tenant, postmortemType, now.Add(-paragraphEvery), r.Class, r.SubjectKind, r.SubjectID, except).Scan(&recent)
	return recent, err
}

// paragraphAsked is a person's request for the model paragraph of an incident's analysis, from
// the channel command /incident analyse <number> by a person with incident.manage (MGR-53). Before
// the incident resolves the request is kept in its record and the analysis gets the paragraph;
// an analysis that waits for its worker gets it at once; once it started or ended it is too late.
// With incident_review off, or a paragraph of the same class and object in 7 days, there is none.
// It returns the answer code: requested, reading_off, recent or late.
func (c *Core) paragraphAsked(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, tv *TenantView, person, ref string) (string, string, error) {
	r, err := loadIncident(ctx, tx, tv.Name, ref, true)
	if err != nil {
		return "", "", err
	}
	if r == nil {
		return "", "", refuse("not_found", "no such incident in the tenant")
	}
	// The tenant gets no analysis at all: nothing to ask for (the conditions of enqueuePostmortem).
	if !postmortemOn(cat, tv.Name) {
		return r.ID, "no_analysis", nil
	}
	if !incidentReviewOn(tv) {
		return r.ID, "reading_off", nil
	}
	var task, state string
	var paragraph bool
	err = tx.QueryRow(ctx, `SELECT id, state, coalesce((record->>'paragraph')::boolean, false) FROM tasks WHERE tenant = $1 AND idempotency_key = $2 FOR UPDATE`,
		tv.Name, "system:postmortem:"+r.ID).Scan(&task, &state, &paragraph)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		if r.State == "resolved" || r.State == "closed" || r.State == "dismissed" {
			return r.ID, "late", nil
		}
	case err != nil:
		return "", "", err
	case paragraph:
		return r.ID, "requested", nil
	case state != "submitted":
		return r.ID, "late", nil
	}
	if recent, err := paragraphRecent(ctx, tx, now, r, task); err != nil {
		return "", "", err
	} else if recent {
		return r.ID, "recent", nil
	}
	if task == "" {
		// Beside the record, which has no such field, so no incident view shows it.
		if _, err := tx.Exec(ctx, `UPDATE incidents SET paragraph_requested_at = $2, paragraph_requested_by = $3 WHERE id = $1`, r.ID, now, person); err != nil {
			return "", "", err
		}
	} else if err := setPostmortemParagraph(ctx, tx, task); err != nil {
		return "", "", err
	}
	_, err = c.appendAudit(ctx, tx, now, Audit{
		Tenant: tv.Name, Kind: "command", Actor: Actor{Principal: person, Via: "channel"}, Command: "incident.analyse",
		Target: &Target{Kind: "incident", ID: r.ID}, OK: true, After: map[string]any{"paragraph": true},
	})
	return r.ID, "requested", err
}

// paragraphRequested says whether a person asked for the analysis paragraph of an incident
// before it resolved (MGR-53).
func paragraphRequested(ctx context.Context, tx pgx.Tx, incident string) (bool, error) {
	var ok bool
	err := tx.QueryRow(ctx, `SELECT paragraph_requested_at IS NOT NULL FROM incidents WHERE id = $1`, incident).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return ok, err
}

// setPostmortemParagraph turns the paragraph on in a waiting analysis: in its record and in the
// input of its body, with the body hash computed again over the new body (MGR-53).
func setPostmortemParagraph(ctx context.Context, tx pgx.Tx, task string) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT body FROM tasks WHERE id = $1`, task).Scan(&raw); err != nil {
		return err
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return err
	}
	input := mmap(mmap(body, "params"), "input")
	if input == nil {
		return nil
	}
	input["paragraph"] = true
	hash, err := jcs.Hash(body)
	if err != nil {
		return err
	}
	nb, err := json.Marshal(body)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE tasks SET record = record || '{"paragraph": true}', body = $2, body_hash = $3 WHERE id = $1`, task, nb, hash)
	return err
}

// postmortemOn says whether a resolved incident of the tenant gets an analysis: the conditions of
// enqueuePostmortem (MGR-53).
func postmortemOn(cat *Catalog, tenant string) bool {
	role := managerRole(cat)
	if cat.Tenants[tenant] == nil || !managerEnabled(cat, tenant) || managerCharter(cat)["postmortem"] == false || role == "" ||
		!slices.Contains(cat.Roles[role].Claims, postmortemType) {
		return false
	}
	_, ok := cat.TaskTypes[postmortemType]
	return ok
}

// analyseCommand is /incident analyse <number> (MGR-53).
func (c *Core) analyseCommand(ctx context.Context, tx pgx.Tx, now time.Time, pc personCommand, ref string) (*ActivityResult, error) {
	id, code, err := c.paragraphAsked(ctx, tx, now, pc.cat, pc.tv, pc.person, ref)
	if p, ok := AsProblem(err); ok && p.Reason == "not_found" {
		return pc.reply(c, "cmd.incident.not_found", nil), nil
	}
	if err != nil {
		return nil, err
	}
	return pc.reply(c, "cmd.incident.analyse_"+code, map[string]any{"incident": id}), nil
}
