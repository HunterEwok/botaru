// SPDX-License-Identifier: AGPL-3.0-only

package evals

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/conform"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/money"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Thresholds are the values of evals/thresholds.yaml the runner applies.
type Thresholds struct {
	SecurityMaxFailures int
	Repeats             int
	MaxRunMicros        int64
	MaxCaseMicros       int64
	Retries             int
	MaxUnratedShare     float64
	Parallel            int
	CaseTimeoutS        int
}

// LoadThresholds reads evals/thresholds.yaml.
func LoadThresholds(root string) (*Thresholds, error) {
	b, err := os.ReadFile(filepath.Join(root, "evals/thresholds.yaml"))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yamlx.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("thresholds.yaml: %w", err)
	}
	get := func(sec, key string) float64 {
		v, _ := num(asMap(m[sec])[key])
		return v
	}
	t := &Thresholds{
		SecurityMaxFailures: int(get("security", "max_failures")), Repeats: int(get("security", "repeats_before_change")),
		MaxRunMicros: eurMicros(get("budget", "max_eur_per_run")), MaxCaseMicros: eurMicros(get("budget", "max_eur_per_case")),
		Retries: int(get("infra", "retries")), MaxUnratedShare: get("infra", "max_unrated_share"),
		Parallel: int(get("infra", "parallel")), CaseTimeoutS: int(get("infra", "case_timeout_s")),
	}
	if t.SecurityMaxFailures != 0 {
		// The zero security threshold never changes (evals/README.md, "Thresholds").
		return nil, fmt.Errorf("thresholds.yaml: security.max_failures must be 0")
	}
	if t.Repeats < 1 {
		t.Repeats = 1
	}
	if t.CaseTimeoutS <= 0 {
		t.CaseTimeoutS = 300
	}
	return t, nil
}

// eurMicros converts a euro amount of a skeleton file to micro-euros (D-87 (1)).
func eurMicros(eur float64) int64 { return money.FromEUR(eur) }

// formatEUR prints micro-euros as euros with four decimals.
func formatEUR(m int64) string { return fmt.Sprintf("%.4f", float64(m)/1e6) }

// CatalogEntry is one entry of evals/catalog.yaml.
type CatalogEntry struct {
	Area, Case, Version, Block, Method string
	Rules                              []string
}

// LoadCatalog reads evals/catalog.yaml.
func LoadCatalog(root string) (map[string]*CatalogEntry, error) {
	b, err := os.ReadFile(filepath.Join(root, "evals/catalog.yaml"))
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yamlx.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("catalog.yaml: %w", err)
	}
	out := map[string]*CatalogEntry{}
	for _, a := range asList(m["areas"]) {
		am := asMap(a)
		for _, e := range asList(am["cases"]) {
			em := asMap(e)
			ce := &CatalogEntry{Area: str(am["area"]), Case: str(em["case"]), Version: str(em["version"]), Block: str(em["block"]),
				Method: str(em["method"]), Rules: strs(em["rules"])}
			out[ce.Case] = ce
		}
	}
	return out, nil
}

// Estimate of one case: the tokens of one ops-reader task of docs/quality.md ("Cost": about 25
// thousand input tokens and 3 thousand output tokens), priced by the base profile of the case's
// role. A case that expects a rejection before any work costs nothing, and so does a case whose
// setup runs the mock model.
const (
	estimateInputTokens  = 25_000
	estimateOutputTokens = 3_000
)

// roleProfiles are the model profiles of the case folders whose role has no example manifest
// with models.base, or whose cases run on a seat rather than a role.
var roleProfiles = map[string]string{
	"commission": "seat-std", "translator": "translate-std", "insights": "analysis-std", "manager": "manager-std",
}

// roleManifests are the example role manifests of case folders that are not named as the role.
var roleManifests = map[string]string{
	"hello": "examples/hello-bot/role.yaml", "manager": "examples/roles/farm-manager/role.yaml",
}

// Price is the price of a model profile in EUR per million tokens (docs/models-and-money.md).
type Price struct {
	Profile, Model string
	Input, Output  float64
}

// Prices reads the model profiles: the example profiles of examples/model-profiles/, each
// replaced by a profile of the same name in dir when dir is given (a real run's profiles).
func Prices(root, dir string) (map[string]Price, error) {
	files, err := filepath.Glob(filepath.Join(root, "examples/model-profiles/*.yaml"))
	if err != nil {
		return nil, err
	}
	if dir != "" {
		more, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
		if err != nil {
			return nil, err
		}
		files = append(files, more...)
	}
	out := map[string]Price{}
	docs, err := manifest.ReadFiles(files)
	if err != nil {
		return nil, err
	}
	for _, d := range docs {
		if d.Kind != "ModelProfile" {
			continue
		}
		sp := asMap(d.Value["spec"])
		pr := asMap(sp["price"])
		in, _ := num(pr["input"])
		o, _ := num(pr["output"])
		out[d.Name] = Price{Profile: d.Name, Model: str(sp["model"]), Input: in, Output: o}
	}
	return out, nil
}

// caseProfile is the model profile a case runs on by default.
func caseProfile(root string, c *Case) string {
	if p := roleProfiles[c.Role]; p != "" {
		return p
	}
	path := roleManifests[c.Role]
	if path == "" {
		path = filepath.Join("examples/roles", c.Role, "role.yaml")
	}
	if docs, err := manifest.ReadFiles([]string{filepath.Join(root, path)}); err == nil && len(docs) == 1 {
		if p := str(asMap(asMap(docs[0].Value["spec"])["models"])["base"]); p != "" {
			return p
		}
	}
	return "base-std"
}

// usesModel says whether a case is expected to call a model at all: not a case whose
// expectation is a rejection before any work, and not a case whose setup scripts the mock.
func usesModel(c *Case) bool {
	return c.State != "rejected" && str(c.Setup["model"]) != "mock"
}

// CaseEstimate is the cost estimate of one run of a case in micro-euros.
func CaseEstimate(c *Case, profile string, prices map[string]Price) int64 {
	if !usesModel(c) {
		return 0
	}
	p := prices[profile]
	return money.TokensCost(estimateInputTokens, p.Input) + money.TokensCost(estimateOutputTokens, p.Output)
}

// RulePlan is one claimed eval rule with what checks it.
type RulePlan struct {
	Rule *conform.Rule
	// Cases are the eval case files of the run that name the rule; Later are those of a later
	// version or pending.
	Cases, Later []string
	// NoModel are the catalog rows without a model and the conformance checks the rule names
	// (other methods); Missing are case ids of the catalog without a case file.
	NoModel, Missing []string
}

// CasePlan is one case of the run with its profile and estimate.
type CasePlan struct {
	Case     *Case
	Profile  string
	Estimate int64
}

// Plan is a run: the claimed eval rules, the cases and the estimate.
type Plan struct {
	Claim       conform.Claim
	Rules       []*RulePlan
	Cases       []*CasePlan
	Later       []*Case
	EstimateRun int64
}

// EvalRules are the active rules of a claim with the eval method.
func EvalRules(rules map[string]*conform.Rule, claim conform.Claim) []*conform.Rule {
	var out []*conform.Rule
	for _, r := range rules {
		if slices.Contains(r.Check, "eval") && conform.RuleInScope(r, claim) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return ruleLess(out[i].ID, out[j].ID) })
	return out
}

func ruleLess(a, b string) bool {
	fa, na := splitRule(a)
	fb, nb := splitRule(b)
	if fa != fb {
		return fa < fb
	}
	return na < nb
}

func splitRule(id string) (string, int) {
	fam, n, _ := strings.Cut(id, "-")
	v := 0
	for _, ch := range n {
		if ch < '0' || ch > '9' {
			break
		}
		v = v*10 + int(ch-'0')
	}
	return fam, v
}

// NewPlan plans a run of the selected cases against the eval rules of a claim.
func NewPlan(root string, rules map[string]*conform.Rule, catalog map[string]*CatalogEntry, all, in, later []*Case, claim conform.Claim, prices map[string]Price) *Plan {
	p := &Plan{Claim: claim, Later: later}
	byID := map[string]*Case{}
	for _, c := range all {
		byID[c.ID] = c
	}
	inRun := map[string]bool{}
	for _, c := range in {
		inRun[c.ID] = true
		prof := caseProfile(root, c)
		cp := &CasePlan{Case: c, Profile: prof, Estimate: CaseEstimate(c, prof, prices)}
		p.Cases = append(p.Cases, cp)
		p.EstimateRun += cp.Estimate
	}
	for _, r := range EvalRules(rules, claim) {
		rp := &RulePlan{Rule: r}
		names := slices.Clone(r.Fixtures)
		for _, c := range all {
			if slices.Contains(c.Rules, r.ID) && !slices.Contains(names, c.ID) {
				names = append(names, c.ID)
			}
		}
		for _, n := range names {
			c, file := byID[n]
			ce := catalog[n]
			switch {
			case file && inRun[n]:
				rp.Cases = append(rp.Cases, n)
			case file:
				rp.Later = append(rp.Later, n+" ("+c.Version+", "+c.Status+")")
			case strings.Contains(n, ":") || strings.Contains(n, "/") || ce != nil && (ce.Method != "" || ce.Block == "without a model"):
				rp.NoModel = append(rp.NoModel, n)
			case ce != nil:
				rp.Missing = append(rp.Missing, n)
			default:
				// A fixture name of conformance/fixtures/ (fx-*, gov-fx-*), not an eval case.
				rp.NoModel = append(rp.NoModel, n)
			}
		}
		p.Rules = append(p.Rules, rp)
	}
	return p
}

// WritePlan prints the dry plan: every claimed eval rule with its cases and the estimate.
func WritePlan(w io.Writer, p *Plan, prices map[string]Price, repeats int) error {
	var b strings.Builder
	fmt.Fprintf(&b, "Eval plan for claim %s, profiles %s\n\n", p.Claim.Version, strings.Join(p.Claim.Profiles, ", "))
	nonWaivable := 0
	for _, rp := range p.Rules {
		if !rp.Rule.Waivable {
			nonWaivable++
		}
	}
	fmt.Fprintf(&b, "Claimed rules with check eval: %d (%d non-waivable)\n\n", len(p.Rules), nonWaivable)
	for _, rp := range p.Rules {
		flag := "waivable"
		if !rp.Rule.Waivable {
			flag = "non-waivable"
		}
		fmt.Fprintf(&b, "%s (%s, %s, %s)\n", rp.Rule.ID, rp.Rule.Version, rp.Rule.Profile, flag)
		line := func(label string, l []string) {
			if len(l) > 0 {
				fmt.Fprintf(&b, "  %s: %s\n", label, strings.Join(l, ", "))
			}
		}
		line("cases", rp.Cases)
		line("cases of a later version", rp.Later)
		line("in the catalog without a case file", rp.Missing)
		line("checked without a model", rp.NoModel)
		if len(rp.Cases) == 0 {
			b.WriteString("  no eval case of the run: the rule is reported as eval not run\n")
		}
	}
	byProfile := map[string][2]int64{}
	blocking := 0
	for _, cp := range p.Cases {
		v := byProfile[cp.Profile]
		v[0]++
		v[1] += cp.Estimate
		byProfile[cp.Profile] = v
		if cp.Case.Blocking {
			blocking++
		}
	}
	fmt.Fprintf(&b, "\nCases in the run: %d (%d blocking), %d of a later version or pending\n", len(p.Cases), blocking, len(p.Later))
	fmt.Fprintf(&b, "Estimate per case: %d input and %d output tokens at the base profile of the case's role (docs/quality.md, \"Cost\")\n",
		estimateInputTokens, estimateOutputTokens)
	for _, name := range slices.Sorted(mapKeys(byProfile)) {
		v := byProfile[name]
		pr := prices[name]
		fmt.Fprintf(&b, "  %-14s %3d cases, price %.2f/%.2f EUR per M tokens: %s EUR\n", name, v[0], pr.Input, pr.Output, formatEUR(v[1]))
	}
	fmt.Fprintf(&b, "Estimate of one run: %s EUR; with %d repeats of the blocking cases: %s EUR\n", formatEUR(p.EstimateRun), repeats,
		formatEUR(p.EstimateWith(repeats)))
	_, err := io.WriteString(w, b.String())
	return err
}

// EstimateWith is the estimate of the run with repeats runs of each blocking case.
func (p *Plan) EstimateWith(repeats int) int64 {
	return p.EstimateRun + int64(max(repeats, 1)-1)*blockingEstimate(p)
}

func blockingEstimate(p *Plan) int64 {
	var n int64
	for _, cp := range p.Cases {
		if cp.Case.Blocking {
			n += cp.Estimate
		}
	}
	return n
}

func mapKeys[K comparable, V any](m map[K]V) func(func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
