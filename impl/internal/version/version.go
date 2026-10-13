// SPDX-License-Identifier: AGPL-3.0-only

// Package version reports the build version of the botaru binaries.
package version

// Version is set at build time with -ldflags "-X github.com/HunterEwok/botaru/impl/internal/version.Version=<v>".
var Version = "dev"

// String returns the version line printed by --version.
func String(name string) string {
	return name + " " + Version
}
