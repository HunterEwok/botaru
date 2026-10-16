// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"sync/atomic"

	"github.com/HunterEwok/botaru/impl/internal/spec"
)

// claimsV03 says whether the core claims skeleton v0.3, whose rules replace those of v0.1 and
// v0.2 marked until v0.2 (for the farm manager GOV-22, MGR-21, MGR-33 to MGR-36 give way to
// GOV-47, MGR-45 to MGR-55; for the commission the accountability of COM-15 and GOV-36). It is
// the one switch of every v0.3 path; a test sets v03Forced (setClaimsV03) to run both paths
// before the claimed version moves.
func claimsV03() bool {
	switch v03Forced.Load() {
	case v03On:
		return true
	case v03Off:
		return false
	}
	return !spec.VersionLater("v0.3", spec.Version)
}

// v03Forced overrides the claimed version for claimsV03: 0 follows spec.Version.
var v03Forced atomic.Int32

const (
	v03On  = 1
	v03Off = 2
)
