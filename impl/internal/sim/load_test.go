// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// The embedded load profiles read and name only known work; an unknown code is refused.
func TestLoadProfiles(t *testing.T) {
	for _, n := range LoadNames() {
		p, err := LoadLoad(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if len(p.Works) == 0 || p.Caps.AnalysesPerHour != 2 || p.Caps.ProposalsPerRun != 4 {
			t.Errorf("%s: %+v", n, p)
		}
	}
	bad := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(bad, []byte("name: bad\nworks: [{code: R-99, per_hour: 1}]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLoad(bad); err == nil {
		t.Fatal("an unknown work loaded")
	}
}

// The rate factor ramps up over the warm-up and multiplies in a peak; the Poisson draws of a seed
// repeat and their mean is the rate.
func TestLoadFactorAndDraws(t *testing.T) {
	p := &LoadProfile{Warmup: 10 * time.Minute, Peaks: []LoadPeak{{At: 20 * time.Minute, For: 15 * time.Minute, Factor: 3}}}
	for _, c := range []struct {
		at   time.Duration
		want float64
	}{{0, 0}, {5 * time.Minute, 0.5}, {15 * time.Minute, 1}, {25 * time.Minute, 3}, {40 * time.Minute, 1}} {
		if got := p.factor(c.at); got != c.want {
			t.Errorf("factor at %v = %v, want %v", c.at, got, c.want)
		}
	}
	draw := func() (int, []int) {
		r := rand.New(rand.NewPCG(42, 17))
		sum, first := 0, []int{}
		for i := range 100000 {
			n := poisson(r, 0.05)
			sum += n
			if i < 20 {
				first = append(first, n)
			}
		}
		return sum, first
	}
	a, fa := draw()
	b, fb := draw()
	if a != b || len(fa) != len(fb) {
		t.Fatal("a seed does not repeat")
	}
	for i := range fa {
		if fa[i] != fb[i] {
			t.Fatal("a seed does not repeat")
		}
	}
	if a < 4700 || a > 5300 {
		t.Errorf("mean %v, want about 0.05", float64(a)/100000)
	}
}
