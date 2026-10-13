// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-core is the farm core. It is a placeholder until the work on the core starts:
// it only prints its version.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-core"))
		return
	}
	fmt.Fprintln(os.Stderr, "botaru-core: the core is not implemented yet; only --version works")
	os.Exit(2)
}
