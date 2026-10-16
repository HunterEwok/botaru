// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/HunterEwok/botaru/impl/internal/jcs"
)

// Proposals with a script change (PRP-3). The author role declares scripts with propose:
// in_proposal; the draft is a botaru.script/v1 within that vocabulary. At screening the core
// checks the draft's form and vocabulary; "Accept" puts it through the SCR-5 acceptance on behalf
// of the person who accepted, as if they had asked for it, and writes a proposed run whose
// script_confirm card goes to that person (SCR-6). The run is the realization object: a
// completed run realizes the proposal, a run that ends otherwise fails it as object_failed.

// checkScriptChange is the screening of a script change: the author role's propose:
// in_proposal, the draft by the script schema, every step within the role's step types and
// actions, and no goal taken from an assignment (a proposal has none). The target of the
// fingerprint is the hash of the draft.
func (c *Core) checkScriptChange(cat *Catalog, a proposalAuthor, ch proposalChange) (changeCheck, error) {
	role, ok := cat.Roles[a.Role]
	if !ok || role.Scripts == nil || role.Scripts.Propose != "in_proposal" {
		return changeCheck{}, rejection("forbidden_change")
	}
	if ch.Script == nil || c.spec.Validate("script-propose-request", map[string]any{"script": ch.Script}) != nil {
		return changeCheck{}, rejection("schema_invalid")
	}
	if _, ok := ch.Script["params"]; ok {
		return changeCheck{}, rejection("forbidden_change")
	}
	if _, ok := ch.Script["goals"]; ok {
		return changeCheck{}, rejection("forbidden_change")
	}
	steps, err := parseSteps(ch.Script)
	if err != nil || len(steps) == 0 {
		return changeCheck{}, rejection("schema_invalid")
	}
	for _, s := range steps {
		if s.Type != "" && !slices.Contains(role.Scripts.StepTypes, s.Type) || s.Action != "" && !slices.Contains(role.Scripts.Actions, s.Action) {
			return changeCheck{}, rejection("forbidden_change")
		}
		if s.Goal != nil && s.Goal["assignment"] != nil {
			return changeCheck{}, rejection("forbidden_change")
		}
	}
	h, err := jcs.Hash(ch.Script)
	if err != nil {
		return changeCheck{}, err
	}
	return changeCheck{Targets: []any{map[string]any{"script": h}}}, nil
}

// realizeScript runs the SCR-5 acceptance of an accepted script change on behalf of the person
// who accepted: their rights for every step, the author role's vocabulary and the limits of
// the catalog as it is now. It returns the proposed run, or a refusal code for the press.
func (c *Core) realizeScript(ctx context.Context, tx pgx.Tx, now time.Time, cat *Catalog, press *cardPress, p *proposalRow) (string, string, error) {
	b := p.body()
	origin := mmapAny(p.Rec["origin"])
	g := &toolsGrant{
		Tenant: press.tv.Name, Task: asString(origin["task"]), Role: asString(origin["role"]), Worker: asString(origin["worker"]),
		Initiator: press.principal, InitiatorGroups: press.groups, Mark: asString(p.Rec["data_mark"]),
	}
	role, ok := cat.Roles[g.Role]
	if !ok || role.Scripts == nil || role.Scripts.Propose != "in_proposal" || b.Change.Script == nil || g.Task == "" {
		return "", "forbidden", nil
	}
	// The run is anchored to the analysis task; one that is gone refuses the press.
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tasks WHERE id = $1 AND tenant = $2)`, g.Task, g.Tenant).Scan(&exists); err != nil {
		return "", "", err
	}
	if !exists {
		return "", "forbidden", nil
	}
	// The analysis task's goal is no person's assignment: the run has none, and the step
	// marks are computed without one.
	acc, prob, err := c.acceptScriptAs(ctx, tx, now, cat, g, map[string]any{"script": b.Change.Script}, true)
	if err != nil {
		return "", "", err
	}
	if prob != nil {
		if prob.Reason == "held" {
			return "", "held", nil
		}
		return "", "forbidden", nil
	}
	acc.key = "system:script:proposal:" + p.ID + ":" + acc.planHash
	id, _, err := c.insertRun(ctx, tx, now, cat, g, acc)
	if err != nil {
		return "", "", err
	}
	if id == "" {
		return "", "forbidden", nil
	}
	return id, "", nil
}

// scriptRealized follows a run that realizes a proposal: "realized", "failed" or "" while the
// run is still proposed, running or paused.
func scriptRealized(ctx context.Context, tx pgx.Tx, run string) (string, error) {
	if !strings.HasPrefix(run, "scr_") {
		return "", nil
	}
	var state string
	err := tx.QueryRow(ctx, `SELECT state FROM script_runs WHERE id = $1`, run).Scan(&state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "failed", nil
	}
	if err != nil {
		return "", err
	}
	switch state {
	case "completed":
		return "realized", nil
	case "failed", "canceled", "rejected":
		return "failed", nil
	}
	return "", nil
}
