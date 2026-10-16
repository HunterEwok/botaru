// SPDX-License-Identifier: AGPL-3.0-only

package core

import "slices"

// The eval gate of the commission persona from v0.3 (GOV-46, D-111): the commission persona says
// that forecasts are checked against outcomes, and that line must not move the reviews. With and
// without it the mean score of a lens may shift by no more than 0.2 on the fixed set of 40
// proposals, the evaluation of GOV-29 (evals/cases/commission/gov-res-aware-*). So on a v0.3 core
// a role of the commission position whose static fingerprint is new or changed (which covers a
// changed persona, PRV-3) needs, besides the entry of PRV-3, an entry of the set gov-res-aware for
// the same fingerprint with at least one case and no failure: a drift case above its threshold
// counts as a failure of the run. Without it a production tenant does not enable the role
// (eval_report_missing), as for PRV-3. The run itself costs money and is an operator step.

// suiteUnawareness is the eval set of GOV-29 and GOV-46.
const suiteUnawareness = "gov-res-aware"

// unawareReported says whether a role passes the GOV-46 gate at fingerprint fp: always for a role
// outside the commission position or on a core before v0.3.
func (x evalIndex) unawareReported(role, fp string) bool {
	if !claimsV03() || profilePosition(mstr(x.spec("Role", "", role), "profile")) != "commission" {
		return true
	}
	return slices.ContainsFunc(x.reports(), func(r evalReport) bool {
		return r.SubjectKind == "role" && r.SubjectName == role && r.Fingerprint == fp && r.Suite == suiteUnawareness && r.Cases > 0 && r.Failures == 0
	})
}
