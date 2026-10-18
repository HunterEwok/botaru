// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// The merge gate of a manifest change (EVO-9). The core never runs git or evals: a person with
// proposal.gate:<category> runs `farmctl proposal gate`, which checks the change in a temporary
// worktree and posts the closed result here. A tenant with insights.gate.required shows a
// manifest change to people only after a passed gate at the applied commit, and accepts it only
// while that result is current.

// gateCodes are the codes of a failed gate (spec/model/reasons.yaml).
var gateCodes = []string{"diff_size_exceeded", "forbidden_path_modified", "diff_unrenderable", "lint_failed", "coupling_broken",
	"regression_failed", "holdout_regression", "reward_hacking_detected", "shortcut_gaming_detected", "audit_tampering_detected"}

var reCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

var gateSteps = []string{"diff", "lint", "coupling", "regression", "holdout"}

// GateRequest is P2 proposal.gate: without a result it returns what the gate needs; with one it
// records the verdict.
type GateRequest struct {
	Proposal string      `json:"proposal"`
	Result   *GateResult `json:"result"`
}

// GateResult is the closed result of a gate run: no file, no diff text, no hidden case.
type GateResult struct {
	Verdict            string     `json:"verdict"`
	Code               string     `json:"code"`
	BaseSHA            string     `json:"base_sha"`
	Steps              []GateStep `json:"steps"`
	HoldoutPassed      *bool      `json:"holdout_passed"`
	RegressionPassRate *float64   `json:"regression_pass_rate"`
	Fields             int        `json:"fields"`
	DiffLines          int        `json:"diff_lines"`
}

// GateStep is the status of one step.
type GateStep struct {
	Step   string `json:"step"`
	Status string `json:"status"`
	Code   string `json:"code"`
}

// gateRequired says whether the tenant shows manifest changes only after a passed gate.
func gateRequired(tv *TenantView) bool {
	return tv != nil && tv.Spec.Insights.Gate != nil && tv.Spec.Insights.Gate.Required
}

// gateCurrent says whether the proposal passed the gate at the applied commit with every step
// passed: a step that did not run never counts as passed (EVO-9).
func gateCurrent(ctx context.Context, tx pgx.Tx, p *proposalRow) (bool, error) {
	g := mmapAny(p.Rec["gate"])
	if g == nil || g["state"] != "passed" {
		return false, nil
	}
	for _, s := range asList(g["steps"]) {
		if mmapAny(s)["status"] != "passed" {
			return false, nil
		}
	}
	commit, err := appliedCommit(ctx, tx)
	return err == nil && commit != "" && g["base_sha"] == commit, err
}

func appliedCommit(ctx context.Context, tx pgx.Tx) (string, error) {
	var commit string
	err := tx.QueryRow(ctx, `SELECT commit FROM apply_status WHERE id = 1`).Scan(&commit)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return commit, err
}

// gatedOut says whether a manifest change of a gate-required tenant waits for its gate.
func gatedOut(ctx context.Context, tx pgx.Tx, tv *TenantView, p *proposalRow) (bool, error) {
	if !gateRequired(tv) || p.kind() != "manifest_change" {
		return false, nil
	}
	ok, err := gateCurrent(ctx, tx, p)
	return !ok, err
}

func sameSteps(recorded []any, steps []GateStep) bool {
	if len(recorded) != len(steps) {
		return false
	}
	for i, s := range steps {
		if m := mmapAny(recorded[i]); m["step"] != s.Step || m["status"] != s.Status {
			return false
		}
	}
	return true
}

func checkGateResult(r *GateResult) error {
	bad := func(detail string) error { return refuse("input_schema_invalid", "%s", detail) }
	if !reCommitSHA.MatchString(r.BaseSHA) {
		return bad("base_sha is a full commit id")
	}
	if len(r.Steps) != len(gateSteps) {
		return bad("the result has every step")
	}
	failed := ""
	for i, s := range r.Steps {
		if s.Step != gateSteps[i] || !slices.Contains([]string{"passed", "failed", "not_run"}, s.Status) {
			return bad("the steps are diff, lint, coupling, regression, holdout with passed, failed or not_run")
		}
		if s.Status == "failed" {
			if failed != "" || !slices.Contains(gateCodes, s.Code) {
				return bad("one failed step with a gate code")
			}
			failed = s.Code
		} else if s.Code != "" {
			return bad("only a failed step has a code")
		}
	}
	switch r.Verdict {
	case "passed":
		if failed != "" || r.Code != "" || r.Steps[0].Status != "passed" {
			return bad("a passed gate has a passed diff step and no failed step")
		}
	case "failed":
		if failed == "" || r.Code != failed {
			return bad("a failed gate names the code of its failed step")
		}
	default:
		return bad("the verdict is passed or failed")
	}
	if (r.HoldoutPassed != nil) != (r.Steps[4].Status != "not_run") || r.HoldoutPassed != nil && *r.HoldoutPassed != (r.Steps[4].Status == "passed") {
		return bad("holdout_passed follows the holdout step")
	}
	if r.RegressionPassRate != nil && (*r.RegressionPassRate < 0 || *r.RegressionPassRate > 1) || r.Fields < 0 || r.DiffLines < 0 {
		return bad("the numbers are out of range")
	}
	return nil
}

// ProposalGate is P2 proposal.gate (EVO-9). The person needs proposal.gate:<category>. A read
// returns the change, the applied commit and the tenant's limits; a result is recorded for a
// recommended, presented or judged sample manifest change at the applied commit, once per
// commit: a passed gate lets the digest show it and "Accept" take it, a failed one dismisses a
// recommended or presented change with the reason gate_failed, so it never reaches a person, and
// keeps a judged sample item from "Accept" and "Lift veto". Both go into the audit.
func (c *Core) ProposalGate(ctx context.Context, op Operator, req GateRequest) (map[string]any, error) {
	tv, cat, err := c.requireTenant(ctx, op.Tenant)
	if err != nil {
		return nil, err
	}
	if req.Result != nil {
		if err := checkGateResult(req.Result); err != nil {
			return nil, err
		}
	}
	var out map[string]any
	err = c.tx(ctx, func(tx pgx.Tx, now time.Time) error {
		v, err := c.p2viewer(ctx, tx, now, cat, tv, op.Principal)
		if err != nil {
			return err
		}
		p, err := lockedProposal(ctx, tx, req.Proposal)
		if err != nil {
			return err
		}
		if p == nil || p.Tenant != tv.Name || p.kind() != "manifest_change" || !slices.Contains(proposalShown, p.State) {
			return refuse("not_found", "no such manifest change")
		}
		category := p.body().Category
		if !v.can("proposal.gate:" + category) {
			pr := refuse("forbidden", "proposal.gate of the proposal's category is needed")
			pr.Needs = []string{"proposal.gate:" + category}
			return pr
		}
		commit, err := appliedCommit(ctx, tx)
		if err != nil {
			return err
		}
		limits := map[string]any{"required": gateRequired(tv), "max_fields": 1, "max_diff_lines": 40}
		if g := tv.Spec.Insights.Gate; g != nil {
			if g.MaxFields > 0 {
				limits["max_fields"] = g.MaxFields
			}
			if g.MaxDiffLines > 0 {
				limits["max_diff_lines"] = g.MaxDiffLines
			}
		}
		if req.Result == nil {
			out = map[string]any{"proposal": p.ID, "tenant": p.Tenant, "category": category, "state": p.State,
				"ops":            mmapAny(mmapAny(mmapAny(p.Rec["body"])["change"])["manifest_change"])["ops"],
				"applied_commit": nullable(commit), "limits": limits, "gate": p.Rec["gate"]}
			_, err = c.appendAudit(ctx, tx, now, Audit{Tenant: tv.Name, Kind: "read", Actor: v.actor(), Command: "proposal.gate",
				Target: &Target{Kind: "proposal", ID: p.ID}, OK: true})
			return err
		}
		r := req.Result
		if r.BaseSHA != commit {
			return refuse("conflict", "the gate ran on %s, the applied commit is %s; run it again", r.BaseSHA, commit)
		}
		if g := mmapAny(p.Rec["gate"]); g != nil && g["base_sha"] == r.BaseSHA {
			// One verdict per commit: a repeat of it is the same answer. A passed gate run again
			// with other steps, for example with the evals that did not run before, replaces it.
			if g["state"] != r.Verdict {
				return refuse("conflict", "the gate at this commit is already recorded as %v", g["state"])
			}
			if r.Verdict == "failed" || sameSteps(asList(g["steps"]), r.Steps) {
				out = map[string]any{"proposal": p.ID, "state": p.State, "gate": g}
				return nil
			}
		}
		if p.State != "recommended" && p.State != "presented" && p.State != "sample_judged" {
			return refuse("already_decided", "the proposal is %s", p.State)
		}
		steps := make([]any, len(r.Steps))
		for i, s := range r.Steps {
			st := map[string]any{"step": s.Step, "status": s.Status}
			if s.Code != "" {
				st["code"] = s.Code
			}
			steps[i] = st
		}
		g := map[string]any{"state": r.Verdict, "code": nullable(r.Code), "base_sha": r.BaseSHA, "steps": steps,
			"holdout_passed": r.HoldoutPassed, "regression_pass_rate": r.RegressionPassRate, "fields": r.Fields,
			"diff_lines": r.DiffLines, "by": op.Principal, "at": fmtTime(now)}
		p.Rec["gate"] = g
		if r.Verdict == "failed" && p.State != "sample_judged" {
			// A judged sample item keeps its state: the failed gate only refuses "Accept" and
			// "Lift veto", and the person closes it with "Reject" or decide_ttl.
			if p.State == "presented" {
				if _, err := tx.Exec(ctx, `UPDATE proposals SET presented_at = NULL WHERE id = $1`, p.ID); err != nil {
					return err
				}
				p.PresentedAt = nil
			}
			if err := c.moveProposal(ctx, tx, now, p, "dismissed", "gate_failed"); err != nil {
				return err
			}
			if err := c.planProposalCardUpdates(ctx, tx, now, p); err != nil {
				return err
			}
		} else if err := c.saveProposal(ctx, tx, now, p); err != nil {
			return err
		}
		out = map[string]any{"proposal": p.ID, "state": p.State, "gate": g}
		_, err = c.appendAudit(ctx, tx, now, Audit{Tenant: tv.Name, Kind: "command", Actor: v.actor(), Command: "proposal.gate",
			Target: &Target{Kind: "proposal", ID: p.ID}, OK: true,
			After: map[string]any{"verdict": r.Verdict, "code": nullable(r.Code), "base_sha": r.BaseSHA, "state": p.State}})
		return err
	})
	return out, err
}
