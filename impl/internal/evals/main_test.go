// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"fmt"
	"os"
	"testing"

	"github.com/HunterEwok/botaru/impl/internal/pgtest"
	"github.com/HunterEwok/botaru/impl/internal/pluginmain"
)

func TestMain(m *testing.M) {
	// The test binary is also the plugin process of the bench toolhosts.
	if kind := os.Getenv("BOTARU_EVAL_PLUGIN"); kind != "" {
		if err := pluginmain.Main(kind, os.Args[1:]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	pgtest.Main(m)
}

// testPlugin starts the test binary as a plugin of the given kind.
func testPlugin(kind string, args []string) ([]string, []string) {
	exe, _ := os.Executable()
	return append([]string{exe}, args...), []string{"BOTARU_EVAL_PLUGIN=" + kind}
}
