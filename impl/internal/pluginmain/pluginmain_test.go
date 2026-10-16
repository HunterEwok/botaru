// SPDX-License-Identifier: AGPL-3.0-only

package pluginmain

import (
	"errors"
	"flag"
	"os"
	"testing"
)

// TestFlagErrors: -h and a bad flag keep the exit codes of flag.ExitOnError (0 and 2) that the
// plugin had before its code moved here (DEP-7: the toolhost reads a plugin's exit status).
func TestFlagErrors(t *testing.T) {
	stderr := os.Stderr
	os.Stderr, _ = os.Open(os.DevNull)
	defer func() { os.Stderr = stderr }()
	if err := Main("logs", []string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("-h: %v", err)
	}
	var u *UsageError
	if err := Main("logs", []string{"-no-such-flag"}); !errors.As(err, &u) {
		t.Fatalf("bad flag: %v", err)
	}
}
