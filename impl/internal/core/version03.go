// SPDX-License-Identifier: AGPL-3.0-only

package core

import "github.com/HunterEwok/botaru/impl/internal/spec"

// claimsV03 says whether the v0.3 paths run: the core claims v0.3, or a test bench switched them
// on before the claimed version moves (v03).
func (c *Core) claimsV03() bool {
	return !spec.VersionLater("v0.3", spec.Version) || c.v03.Load()
}
