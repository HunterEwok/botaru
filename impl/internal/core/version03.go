// SPDX-License-Identifier: AGPL-3.0-only

package core

import "github.com/HunterEwok/botaru/impl/internal/spec"

// claimsV03 says whether the core claims skeleton v0.3, whose rules replace those of v0.1 and
// v0.2 marked until v0.2 (for the farm manager GOV-22, MGR-21, MGR-33 to MGR-36 give way to
// GOV-47, MGR-45 to MGR-55). It is a variable so that tests run both paths.
var claimsV03 = func() bool { return !spec.VersionLater("v0.3", spec.Version) }
