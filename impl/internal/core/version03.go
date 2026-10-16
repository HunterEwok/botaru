// SPDX-License-Identifier: AGPL-3.0-only

package core

import "github.com/HunterEwok/botaru/impl/internal/spec"

// claimsV03 says whether the core claims skeleton v0.3, whose rules replace those of v0.1 and
// v0.2 marked until v0.2 (for the farm manager GOV-22, MGR-21, MGR-33 to MGR-36 give way to
// GOV-47, MGR-45 to MGR-55; for the commission the accountability of COM-15 and GOV-36). It is
// the one switch of every v0.3 path. It follows the effective claim of spec (spec.Effective),
// which a test overrides (setClaimsV03) to run both paths before the claimed version moves; the
// same override decides which v0.3 fields of spec.Ahead apply accepts, so a v0.3 field passes
// apply only when its behaviour runs (MAN-11).
func claimsV03() bool {
	return !spec.VersionLater("v0.3", spec.Effective())
}
