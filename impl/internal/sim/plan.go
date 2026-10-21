// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A money plan (botaru-sim -dry-plan, the load test brief D-10): a mock run of a profile counts
// the model calls and the tokens by rank, prices them at the stand's and the provider's prices
// of each rank's model, and recommends the fuse of the real run: the estimate times 1.5 and a
// reserve for the calls in flight (the worst call times the calls the limiter lets run at once).

// Plan is the money estimate of a run.
type Plan struct {
	// Key names what the run did: the farm, the scenarios, the load and its length.
	Key string    `json:"key"`
	At  time.Time `json:"at"`
	// Factor is what a real run spends against the tokens of the mock (the mock answers in few
	// tokens and few turns); the estimate is the mock's tokens times it.
	Factor float64    `json:"factor"`
	Ranks  []PlanRank `json:"ranks"`
	// StandEUR and ProviderEUR are the estimate at the two prices.
	StandEUR    float64 `json:"stand_eur"`
	ProviderEUR float64 `json:"provider_eur"`
	// WorstCallEUR is what the gateway reserves for the dearest call of any profile of the set
	// (a real model may hand work to a rank the mock never called), at the stand's price;
	// Parallel the calls the limiter lets run at once, which are the calls that hold a reserve
	// (the limiter stands before P4, so a waiting call holds none).
	WorstCallEUR float64 `json:"worst_call_eur"`
	Parallel     int     `json:"parallel"`
	// MaxEUR is the recommended -max-eur.
	MaxEUR float64 `json:"max_eur"`
	// Partial says the mock run of the plan did not run to its end (the fuse, a halt, an
	// interrupt or a failed check): the estimate is then too low.
	Partial string `json:"partial,omitempty"`
}

// PlanRank is the estimate of the calls of one rank.
type PlanRank struct {
	Rank         int     `json:"rank"`
	Model        string  `json:"model"`
	Calls        int     `json:"calls"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	StandEUR     float64 `json:"stand_eur"`
	ProviderEUR  float64 `json:"provider_eur"`
}

// PlanFactor is what a real run spends against the mock's tokens of the same work, at the
// stand's prices: the seven scenarios of the first rank runs (nginx-down to scaffold-evolution)
// spent 1.39 and 1.22 EUR on the models of the ranks, and the mock's tokens of the same scenarios
// price at 0.35 EUR (2026-10-21): the real models take more turns and write more. Correct it
// after each real run.
const PlanFactor = 3.7

// PlanKey names a run for its plan: the farm, the scenarios, the load and its length, and the
// one model of a run that names one.
func PlanKey(farm, scenarios, load string, duration time.Duration, model string) string {
	k := "farm=" + farm
	if model != "" {
		k += " model=" + model
	}
	if scenarios != "" {
		var names []string
		for n := range strings.SplitSeq(scenarios, ",") {
			if n = strings.TrimSpace(n); n != "" {
				names = append(names, n)
			}
		}
		k += " scenarios=" + strings.Join(names, ",")
	}
	if load != "" {
		k += " load=" + load
	}
	if duration > 0 {
		k += " duration=" + duration.String()
	}
	return k
}

var reUnsafe = regexp.MustCompile(`[^a-z0-9.-]+`)

// PlanFile is the file of the plan of a key in a folder; a long key is named by its hash (the
// file holds the key).
func PlanFile(dir, key string) string {
	name := strings.Trim(reUnsafe.ReplaceAllString(strings.ToLower(key), "-"), "-")
	if len(name) > 100 {
		sum := sha256.Sum256([]byte(key))
		name = name[:60] + "-" + hex.EncodeToString(sum[:8])
	}
	return filepath.Join(dir, "plan-"+name+".json")
}

// MakePlan prices the tokens of the run so far at the stand's and the provider's prices of the
// model of each rank.
func (s *Stand) MakePlan(m *Metrics, key string, rankModels map[int]string, stand, provider map[string][2]float64, parallel int) Plan {
	p := Plan{Key: key, At: time.Now().UTC(), Factor: PlanFactor, Parallel: parallel}
	byRank := map[int]*PlanRank{}
	for _, r := range m.Spend.Rows {
		if r.Rank == 0 {
			continue
		}
		pr := byRank[r.Rank]
		if pr == nil {
			pr = &PlanRank{Rank: r.Rank, Model: rankModel(rankModels, r.Rank)}
			byRank[r.Rank] = pr
		}
		pr.Calls += r.Calls
		pr.InputTokens += r.InputTokens
		pr.OutputTokens += r.OutputTokens
	}
	eur := func(p [2]float64, in, out int64) float64 {
		return PlanFactor * (float64(in)*p[0] + float64(out)*p[1]) / 1e6
	}
	for _, rank := range slices.Sorted(maps.Keys(byRank)) {
		pr := byRank[rank]
		pr.StandEUR = eur(stand[pr.Model], pr.InputTokens, pr.OutputTokens)
		pr.ProviderEUR = eur(provider[pr.Model], pr.InputTokens, pr.OutputTokens)
		p.StandEUR += pr.StandEUR
		p.ProviderEUR += pr.ProviderEUR
		p.Ranks = append(p.Ranks, *pr)
	}
	// The worst call: the reserve of the gateway (the output limit, and the request's bytes and
	// 1024 as tokens) at the stand's price of the profile's rank model; the request is the
	// largest of the run with half again, at least 60000 bytes (a real conversation grows
	// longer than the mock's).
	in := float64(max(60000, s.maxReq.Load()*3/2) + 1024)
	s.mu.Lock()
	for name, rank := range s.profiles {
		pr := stand[rankModel(rankModels, rank)]
		p.WorstCallEUR = max(p.WorstCallEUR, (s.maxOut[name]*pr[1]+in*1.25*pr[0])/1e6)
	}
	s.mu.Unlock()
	p.MaxEUR = math.Ceil((1.5*p.StandEUR+p.WorstCallEUR*float64(max(parallel, 1)))*100) / 100
	return p
}

// String is the plan as the console prints it.
func (p Plan) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "money plan of %s (the mock's tokens times %.2f):\n", p.Key, p.Factor)
	for _, r := range p.Ranks {
		fmt.Fprintf(&b, "  rank %d %s: %d calls, %d tokens in, %d out: %.2f EUR at the stand's price, %.2f at the provider's\n",
			r.Rank, r.Model, r.Calls, r.InputTokens, r.OutputTokens, r.StandEUR, r.ProviderEUR)
	}
	fmt.Fprintf(&b, "  estimate %.2f EUR at the stand's price (the fuse counts it), %.2f EUR at the provider's\n", p.StandEUR, p.ProviderEUR)
	fmt.Fprintf(&b, "  recommended -max-eur %.2f = %.2f x 1.5 + the worst call %.2f x %d calls at once", p.MaxEUR, p.StandEUR, p.WorstCallEUR, max(p.Parallel, 1))
	if p.Partial != "" {
		fmt.Fprintf(&b, "\n  the mock run did not run to its end (%s): the estimate is too low", p.Partial)
	}
	return b.String()
}

// Save writes the plan through a temporary file and a rename.
func (p Plan) Save(path string) error {
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(path, append(raw, '\n')); err != nil {
		return err
	}
	return os.Chmod(path, 0o644)
}

// LoadPlan reads a plan written by Save.
func LoadPlan(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &p, nil
}
