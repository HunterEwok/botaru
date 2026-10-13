// SPDX-License-Identifier: AGPL-3.0-only

// Package manifest reads farm manifests (the wrapper apiVersion, kind, metadata, spec of
// spec/manifests/manifest.schema.json) and gives typed views of the kinds the core uses. The
// wrapper and the spec of every kind are checked by the skeleton schemas before a manifest is
// decoded into these types, so the types hold only the fields the core reads.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/ijson"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

// Doc is one manifest as a JSON-shaped value.
type Doc struct {
	Kind   string
	Name   string
	Tenant string
	Value  map[string]any // the whole manifest
	Source string         // file and document number, for messages
}

// Key identifies a manifest object.
type Key struct{ Kind, Tenant, Name string }

func (d Doc) Key() Key { return Key{d.Kind, d.Tenant, d.Name} }

// FromValue builds a Doc from a decoded manifest without checking it.
func FromValue(v map[string]any, source string) Doc {
	d := Doc{Value: v, Source: source}
	d.Kind, _ = v["kind"].(string)
	if md, ok := v["metadata"].(map[string]any); ok {
		d.Name, _ = md["name"].(string)
		d.Tenant, _ = md["tenant"].(string)
	}
	if d.Kind == "Tenant" {
		d.Tenant = d.Name
	}
	return d
}

// ParseYAML reads all documents of a YAML stream. Documents that are not mappings with an
// apiVersion of the form botaru/... are skipped (D-87 (6)).
func ParseYAML(r io.Reader, source string) ([]Doc, error) {
	dec := yamlx.NewDecoder(r)
	var out []Doc
	for i := 0; ; i++ {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		if v == nil {
			continue
		}
		n, err := ijson.Normalize(v)
		if err != nil {
			return nil, fmt.Errorf("%s: document %d: %w", source, i+1, err)
		}
		m, ok := n.(map[string]any)
		if !ok {
			continue
		}
		if av, _ := m["apiVersion"].(string); !strings.HasPrefix(av, "botaru/") {
			continue
		}
		out = append(out, FromValue(m, fmt.Sprintf("%s#%d", source, i+1)))
	}
}

// ReadFiles reads manifests from files and directories (recursively, *.yaml and *.yml, hidden
// folders skipped).
func ReadFiles(paths []string) ([]Doc, error) {
	var files []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !st.IsDir() {
			files = append(files, p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && path != p && strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			if !d.IsDir() && (strings.HasSuffix(path, ".yaml") || strings.HasSuffix(path, ".yml")) {
				files = append(files, path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	sort.Strings(files)
	var out []Doc
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return nil, err
		}
		docs, err := ParseYAML(bytes.NewReader(b), f)
		if err != nil {
			return nil, err
		}
		out = append(out, docs...)
	}
	return out, nil
}

// Decode decodes the spec of a manifest into a typed struct.
func Decode[T any](d Doc) (T, error) {
	var out T
	spec, ok := d.Value["spec"]
	if !ok {
		return out, fmt.Errorf("%s %s: no spec", d.Kind, d.Name)
	}
	b, err := json.Marshal(spec)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return out, fmt.Errorf("%s %s: %w", d.Kind, d.Name, err)
	}
	return out, nil
}

// Tenant is the spec of a Tenant manifest (the fields the core reads).
type Tenant struct {
	Kind             string            `json:"kind"`
	Timezone         string            `json:"timezone"`
	SelfApprove      bool              `json:"self_approve"`
	Locale           string            `json:"locale"`
	Limits           TenantLimits      `json:"limits"`
	Budgets          Budgets           `json:"budgets"`
	ModelsAllowed    []string          `json:"models_allowed"`
	ProvidersAllowed []string          `json:"providers_allowed"`
	Privacy          Privacy           `json:"privacy"`
	LinkAllowlist    []string          `json:"link_allowlist"`
	Commands         map[string]string `json:"commands"`
	DefaultTypes     []DefaultType     `json:"default_types"`
	DelegatedGroups  []string          `json:"delegated_groups"`
}

// TenantLimits are the CAP-1 ceilings.
type TenantLimits struct {
	MaxQueued         int   `json:"max_queued"`
	MaxEnqueuePerHour int   `json:"max_enqueue_per_hour"`
	MaxStorageBytes   int64 `json:"max_storage_bytes"`
}

// Budgets are the tenant money limits in euros (BUD-1).
type Budgets struct {
	MonthEUR *float64 `json:"month_eur"`
	DayEUR   *float64 `json:"day_eur"`
	HourEUR  *float64 `json:"hour_eur"`
}

// Privacy is the tenant privacy block (SEC-8).
type Privacy struct {
	Subprocessors []Subprocessor  `json:"subprocessors"`
	NoticeRef     string          `json:"notice_ref"`
	NoticeVersion json.RawMessage `json:"notice_version"`
}

// Subprocessor is one model provider entry of the privacy block.
type Subprocessor struct {
	Provider  string `json:"provider"`
	Region    string `json:"region"`
	Retention struct {
		Mode    string `json:"mode"`
		MaxDays *int   `json:"max_days"`
	} `json:"retention"`
}

// NoticeVersionString returns the privacy notice version as text.
func (p Privacy) NoticeVersionString() string {
	s := strings.TrimSpace(string(p.NoticeVersion))
	return strings.Trim(s, `"`)
}

// DefaultType is an element of default_types: a bare type name or {type, chats}.
type DefaultType struct {
	Type  string   `json:"type"`
	Chats []string `json:"chats"`
}

// UnmarshalJSON accepts both forms of a default_types element.
func (d *DefaultType) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		d.Type = s
		return nil
	}
	type plain DefaultType
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*d = DefaultType(p)
	return nil
}

// Group is the spec of a people Group manifest.
type Group struct {
	Title    string   `json:"title"`
	Caps     []string `json:"caps"`
	Data     []string `json:"data"`
	Inherits []string `json:"inherits"`
	DayEUR   *float64 `json:"day_eur"`
}

// Role is the spec of a Role manifest.
type Role struct {
	Profile    string            `json:"profile"`
	WorkerKind string            `json:"worker_kind"`
	Claims     []string          `json:"claims"`
	Models     map[string]string `json:"models"`
	Memory     any               `json:"memory"`
	Tools      []any             `json:"tools"`
}

// TaskType is the spec of a TaskType manifest.
type TaskType struct {
	Title         string         `json:"title"`
	WorkerKind    string         `json:"worker_kind"`
	Purpose       string         `json:"purpose"`
	Tier          int            `json:"tier"`
	DataClass     string         `json:"data_class"`
	Instruction   string         `json:"instruction"`
	ParamsSchema  map[string]any `json:"params_schema"`
	ResultSchema  map[string]any `json:"result_schema"`
	SummaryFields []string       `json:"summary_fields"`
	Stages        []string       `json:"stages"`
	Dispatch      struct {
		PriorityClass int `json:"priority_class"`
	} `json:"dispatch"`
	Interactive   bool     `json:"interactive"`
	Timeouts      Timeouts `json:"timeouts"`
	Retry         Retry    `json:"retry"`
	TreeBudgetEUR float64  `json:"tree_budget_eur"`
	GoalTrust     string   `json:"goal_trust"`
}

// Timeouts are the type deadlines as durations (15m, 60s).
type Timeouts struct {
	StartBy    string `json:"start_by"`
	Lease      string `json:"lease"`
	Heartbeat  string `json:"heartbeat"`
	Attempt    string `json:"attempt"`
	StallAfter string `json:"stall_after"`
	Deadline   string `json:"deadline"`
}

// Retry is the type retry policy.
type Retry struct {
	MaxAttempts  int      `json:"max_attempts"`
	MaxRuns      int      `json:"max_runs"`
	Backoff      Backoff  `json:"backoff"`
	NonRetryable []string `json:"non_retryable"`
}

// Backoff is the retry pause.
type Backoff struct {
	Initial string  `json:"initial"`
	Factor  float64 `json:"factor"`
	Max     string  `json:"max"`
	Jitter  bool    `json:"jitter"`
}

// Channel is the spec of a Channel manifest.
type Channel struct {
	Driver              string `json:"driver"`
	Direction           string `json:"direction"`
	IdentityKind        string `json:"identity_kind"`
	IdentityAssurance   string `json:"identity_assurance"`
	MembershipAssurance string `json:"membership_assurance"`
	AIDisclosure        string `json:"ai_disclosure"`
	Provenance          string `json:"provenance"`
	Capabilities        struct {
		MaxText int  `json:"max_text"`
		Buttons bool `json:"buttons"`
		Files   bool `json:"files"`
		Edit    bool `json:"edit"`
	} `json:"capabilities"`
}

// ModelProfile is the spec of a ModelProfile manifest.
type ModelProfile struct {
	Provider              string   `json:"provider"`
	Model                 string   `json:"model"`
	Family                string   `json:"family"`
	Rank                  int      `json:"rank"`
	Pool                  string   `json:"pool"`
	Fallback              []string `json:"fallback"`
	MaxOutputTokens       int      `json:"max_output_tokens"`
	TokensPerS            int      `json:"tokens_per_s"`
	Price                 Price    `json:"price"`
	ProviderRetentionDays *struct {
		Value int `json:"value"`
	} `json:"provider_retention_days"`
	ProviderTraining *struct {
		Value bool `json:"value"`
	} `json:"provider_training"`
}

// Price is in euros per million tokens, that is micro-euros per token.
type Price struct {
	Input      float64  `json:"input"`
	Output     float64  `json:"output"`
	CacheWrite *float64 `json:"cache_write"`
	CacheRead  *float64 `json:"cache_read"`
}
