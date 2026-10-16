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
	"strconv"
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
	Limits           TenantLimits      `json:"limits"`
	Budgets          Budgets           `json:"budgets"`
	ModelsAllowed    []string          `json:"models_allowed"`
	ProvidersAllowed []string          `json:"providers_allowed"`
	Privacy          Privacy           `json:"privacy"`
	LinkAllowlist    []string          `json:"link_allowlist"`
	Commands         map[string]string `json:"commands"`
	DefaultTypes     []DefaultType     `json:"default_types"`
	DelegatedGroups  []string          `json:"delegated_groups"`
	Dispatcher       TenantDispatcher  `json:"dispatcher"`
	Dispatch         TenantDispatch    `json:"dispatch"`
	Attention        TenantAttention   `json:"attention"`
	// Locale is the language of code texts for the tenant's people without their own (DLV-18).
	Locale string `json:"locale"`
	// Run holds the pipelines of the command /run <name> (APR-17).
	Run map[string]RunEntry `json:"run"`
	// Retention holds the tenant's periods: the file period (ART-1) and the periods of the
	// classes content, memory and metadata (RET-9).
	Retention struct {
		Files   string            `json:"files"`
		Classes map[string]string `json:"classes"`
	} `json:"retention"`
	// Insights holds the improvement cycle settings of the tenant (INS-6, PRP-5, DIG-1).
	Insights TenantInsights `json:"insights"`
	// Campaigns holds the tree limit of a root and the campaign ceilings of the tenant (PRM-6,
	// ORC-5, ORC-8, ORC-14).
	Campaigns struct {
		MaxTreeTasks      int  `json:"max_tree_tasks"`
		MaxItems          int  `json:"max_items"`
		MaxItemsUntrusted *int `json:"max_items_untrusted"`
		ConfirmReadOver   struct {
			Items int      `json:"items"`
			EUR   *float64 `json:"eur"`
		} `json:"confirm_read_over"`
		QueueMax int `json:"queue_max"`
	} `json:"campaigns"`
	// Scripts holds the script ceilings of the tenant, which only lower the farm's (SCR-5).
	Scripts struct {
		MaxSteps       int    `json:"max_steps"`
		MaxActionSteps *int   `json:"max_action_steps"`
		RunDeadline    string `json:"run_deadline"`
		// UnnamedTargetsMax is the widest machine selector the run card does not mark (TRN-10).
		UnnamedTargetsMax int `json:"unnamed_targets_max"`
	} `json:"scripts"`
	// Messaging holds the wake-up ceilings of the tenant (MSG-11).
	Messaging struct {
		WakeDayEUR  *float64 `json:"wake_day_eur"`
		WakesPerDay *int     `json:"wakes_per_day"`
	} `json:"messaging"`
}

// TenantInsights is the insights block of a tenant: where the digest goes, who realizes
// accepted changes and the deadlines and limits of the proposal flow.
type TenantInsights struct {
	PresentTo any `json:"present_to"`
	Digest    struct {
		Schedule string `json:"schedule"`
		MaxItems int    `json:"max_items"`
	} `json:"digest"`
	Realizers     []string `json:"realizers"`
	RealizeBy     string   `json:"realize_by"`
	DecideTTL     string   `json:"decide_ttl"`
	RecommendTTL  string   `json:"recommend_ttl"`
	ParkTTL       string   `json:"park_ttl"`
	ReviewWaitMax string   `json:"review_wait_max"`
	RedecideAfter string   `json:"redecide_after"`
	MaxInReview   *int     `json:"max_in_review"`
	ExtraGuards   []struct {
		Metric string   `json:"metric"`
		Guards []string `json:"guards"`
	} `json:"extra_guards"`
}

// RunEntry is one pipeline of /run: a task type or an action, with its positional arguments.
type RunEntry struct {
	Type   string   `json:"type"`
	Action string   `json:"action"`
	Args   []string `json:"args"`
}

// TenantAttention holds the card limits of the tenant (APR-2).
type TenantAttention struct {
	// CardsPerTenantHour is how many action approvals the core creates per hour; 20 when unset.
	CardsPerTenantHour int `json:"cards_per_tenant_hour"`
	// NoncriticalPerDay is how many cards of non-urgent decisions a person gets a day; nil
	// means the default 10 (APR-15).
	NoncriticalPerDay *int `json:"noncritical_per_day"`
}

// TenantDispatch holds the dispatch rules of the tenant policy (DSP-3, DSP-7).
type TenantDispatch struct {
	Rules []DispatchRule `json:"rules"`
	// Fairness holds the weight of each kind of fairness key within a class (DSP-6); without
	// it the order within a class is the enqueue time, as in v0.1.
	Fairness *struct {
		Weights map[string]int `json:"weights"`
	} `json:"fairness"`
}

// DispatchRule matches tasks by type or type prefix and source kind; this build reads its
// ceiling of running tasks (DSP-7).
type DispatchRule struct {
	Match struct {
		Type       string `json:"type"`
		TypePrefix string `json:"type_prefix"`
		Source     string `json:"source"`
	} `json:"match"`
	MaxRunning int `json:"max_running"`
}

// TenantDispatcher holds the dispatcher settings the core reads (DSP-3, DSP-8).
type TenantDispatcher struct {
	// UnclaimedAlarmAfter overrides the type's start_by for alarm.task.unclaimed.
	UnclaimedAlarmAfter string `json:"unclaimed_alarm_after"`
}

// TenantLimits are the CAP-1 ceilings.
type TenantLimits struct {
	MaxQueued         int   `json:"max_queued"`
	MaxEnqueuePerHour int   `json:"max_enqueue_per_hour"`
	MaxStorageBytes   int64 `json:"max_storage_bytes"`
	// MaxFileBytes and MaxTaskFilesBytes lower the farm ceilings of one file and of all files
	// of one task (ART-4); 0 keeps the farm ceiling.
	MaxFileBytes      int64 `json:"max_file_bytes"`
	MaxTaskFilesBytes int64 `json:"max_task_files_bytes"`
}

// Budgets are the tenant money limits in euros (BUD-1).
type Budgets struct {
	MonthEUR *float64 `json:"month_eur"`
	DayEUR   *float64 `json:"day_eur"`
	HourEUR  *float64 `json:"hour_eur"`
	// Lines are the sublimits of the budget lines by purpose (BUD-7).
	Lines map[string]LineBudget `json:"lines"`
}

// LineBudget is one budget line: limits inside the tenant total. A line other than work has
// a month limit of 0 when it sets none (BUD-7).
type LineBudget struct {
	MonthEUR        *float64 `json:"month_eur"`
	DayEUR          *float64 `json:"day_eur"`
	PerRoleMonthEUR *float64 `json:"per_role_month_eur"`
}

// Privacy is the tenant privacy block (SEC-8).
type Privacy struct {
	Purposes []struct {
		Purpose string `json:"purpose"`
	} `json:"purposes"`
	Controller struct {
		Name    string `json:"name"`
		Contact string `json:"contact"`
	} `json:"controller"`
	Recipients    []string       `json:"recipients"`
	Subprocessors []Subprocessor `json:"subprocessors"`
	// Channels are the channel drivers with their third-party storage and ceiling (SEC-9).
	Channels      []PrivacyChannel `json:"channels"`
	NoticeRef     string           `json:"notice_ref"`
	NoticeVersion json.RawMessage  `json:"notice_version"`
	// SubjectRequestDays is the deadline of a person's request and of an erasure (SEC-11).
	SubjectRequestDays int `json:"subject_request_days"`
	// ExportSink is the export_only sink of /mydata export (SEC-10, LCY-2).
	ExportSink string `json:"export_sink"`
}

// PrivacyChannel is one channel driver of the privacy block (SEC-9).
type PrivacyChannel struct {
	Driver            string `json:"driver"`
	ThirdPartyStorage bool   `json:"third_party_storage"`
	Ceiling           string `json:"ceiling"`
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
	// ActionsMayRequest are the actions the model may ask for through request_action (APR-1).
	ActionsMayRequest []string `json:"actions_may_request"`
	// MayEnqueue are the child task types of enqueue_child (PRM-6).
	MayEnqueue []string `json:"may_enqueue"`
	// ServiceControl names the catalog services the role may control, or ["*"] (APR-18).
	ServiceControl []string `json:"service_control"`
	// MonitorScripts names the catalog scripts of the monitoring system the role may run, or
	// ["*"] (MON-10).
	MonitorScripts []string `json:"monitor_scripts"`
	// Insight is the seat analysis of the role (INS-1).
	Insight *RoleInsight `json:"insight"`
	// Messages are the message types the role sends and receives (MSG-5).
	Messages RoleMessages `json:"messages"`
	// InstanceInbox gives every worker of the role its own inbox; unset, a role of a job,
	// toolhost or channel worker has one and an agent role has none (MSG-14).
	InstanceInbox *bool `json:"instance_inbox"`
	// Views are the status.* views of the role (REP-8).
	Views []string `json:"views"`
	// Campaigns says which campaigns the role proposes and the continuation types of their end
	// (ORC-1, CNT-1).
	Campaigns *RoleCampaigns `json:"campaigns"`
	// MayTargetGroups are the bot groups the role may address work to (DSP-4).
	MayTargetGroups []string `json:"may_target_groups"`
	// Scripts is the vocabulary of a script author (SCR-5, TRN-2).
	Scripts *RoleScripts `json:"scripts"`
	// ModelGrants is the ladder of stronger profiles of request_model_grant (MGR-17).
	ModelGrants *struct {
		Ladder []string `json:"ladder"`
	} `json:"model_grants"`
	// Aggregate is the sources block of an aggregator (AGG-2, AGG-3).
	Aggregate *RoleAggregate `json:"aggregate"`
	Runtime   struct {
		Driver string `json:"driver"`
	} `json:"runtime"`
}

// GoalTemplate is one goal template of a task type for script steps.
type GoalTemplate struct {
	Name string `json:"name"`
	Goal string `json:"goal"`
}

// RoleScripts is the scripts block of a role: propose is none, draft or in_proposal.
type RoleScripts struct {
	Propose   string   `json:"propose"`
	StepTypes []string `json:"step_types"`
	Actions   []string `json:"actions"`
	MaxSteps  int      `json:"max_steps"`
}

// RoleAggregate is what an aggregator reads: only its listed sources (AGG-3).
type RoleAggregate struct {
	Sources struct {
		Roles        []string `json:"roles"`
		BotGroups    []string `json:"bot_groups"`
		Series       []string `json:"series"`
		MessageKinds []string `json:"message_kinds"`
	} `json:"sources"`
	MaxSources int `json:"max_sources"`
}

// RoleCampaigns is the campaigns block of a role: propose is none, read or action.
type RoleCampaigns struct {
	Propose string   `json:"propose"`
	Then    []string `json:"then"`
}

// RoleMessages is the messages block of a role (MSG-5).
type RoleMessages struct {
	Send []struct {
		Type string   `json:"type"`
		To   []string `json:"to"`
	} `json:"send"`
	Receive []struct {
		Type string `json:"type"`
	} `json:"receive"`
}

// Receives says whether the role receives the message type.
func (r Role) Receives(typ string) bool {
	for _, x := range r.Messages.Receive {
		if x.Type == typ {
			return true
		}
	}
	return false
}

// HasInstanceInbox says whether every worker of the role has its own inbox (MSG-14).
func (r Role) HasInstanceInbox() bool {
	if r.InstanceInbox != nil {
		return *r.InstanceInbox
	}
	return r.WorkerKind != "" && r.WorkerKind != "agent"
}

// MessageType is the spec of a MessageType manifest (MSG-2).
type MessageType struct {
	Kind         string         `json:"kind"`
	FieldsSchema map[string]any `json:"fields_schema"`
	SubjectFrom  string         `json:"subject_from"`
	DataClass    string         `json:"data_class"`
	Note         struct {
		MaxChars int `json:"max_chars"`
	} `json:"note"`
	Delivery   string   `json:"delivery"`
	TTL        string   `json:"ttl"`
	StaleAfter string   `json:"stale_after"`
	MaxBytes   int      `json:"max_bytes"`
	Producers  []string `json:"producers"`
	Rate       struct {
		PerHourPerSender int `json:"per_hour_per_sender"`
	} `json:"rate"`
}

// BotGroup is the spec of a BotGroup manifest: a selector by roles and labels (BGR-1).
type BotGroup struct {
	Select struct {
		Roles  []string          `json:"roles"`
		Labels map[string]string `json:"labels"`
	} `json:"select"`
}

// Route is the spec of a Route manifest: what the core does on an event, an inbox, a message or
// the end of a task (MSG-16, ING-4).
type Route struct {
	On struct {
		Event string `json:"event"`
		// Subject and Match filter an event: the subject exactly, and closed fields of the
		// event data by an exact value or one of a list (OUT-1, MSG-16).
		Subject       string         `json:"subject"`
		Match         map[string]any `json:"match"`
		Inbox         string   `json:"inbox"`
		Kinds         []string `json:"kinds"`
		Min           int      `json:"min"`
		Debounce      string   `json:"debounce"`
		Message       string   `json:"message"`
		SubjectPrefix string   `json:"subject_prefix"`
		TaskDone      string   `json:"task_done"`
	} `json:"on"`
	Mode string `json:"mode"`
	Task *struct {
		Type   string         `json:"type"`
		Goal   string         `json:"goal"`
		Params map[string]any `json:"params"`
	} `json:"task"`
	Template string `json:"template"`
	Report   *struct {
		Type string `json:"type"`
		To   string `json:"to"`
	} `json:"report"`
	DeliverTo []any `json:"deliver_to"`
}

// RouteMode is the mode of a route: task unless it says otherwise (MSG-16).
func (r Route) RouteMode() string {
	if r.Mode == "" {
		return "task"
	}
	return r.Mode
}

// Action is the spec of an Action manifest: a button with a closed form (MAN-9).
type Action struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Risk        string         `json:"risk"`
	Args        map[string]any `json:"args"`
	TTL         string         `json:"ttl"`
	Idempotent  bool           `json:"idempotent"`
	Approvals   int            `json:"approvals"`
	Executor    struct {
		Plugin string `json:"plugin"`
		Tool   string `json:"tool"`
	} `json:"executor"`
	WritesOnly  []string `json:"writes_only"`
	RunsPerHour int      `json:"runs_per_hour"`
	RunsPerDay  int      `json:"runs_per_day"`
	// Exclusive resource allows one released write call per resource (ORC-12).
	Exclusive string `json:"exclusive"`
	// Campaign holds the ceilings of a campaign with the action (ORC-5).
	Campaign *ActionCampaign `json:"campaign"`
	// Script says whether the action may be a script step (SCR-2).
	Script *struct {
		Allowed bool `json:"allowed"`
	} `json:"script"`
}

// ActionCampaign is the campaign section of an action (ORC-5).
type ActionCampaign struct {
	Allowed        bool   `json:"allowed"`
	MaxItems       int    `json:"max_items"`
	MaxInFlight    int    `json:"max_in_flight"`
	CanaryMin      int    `json:"canary_min"`
	AfterCanary    string `json:"after_canary"`
	MaxFailuresMax *int   `json:"max_failures_max"`
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
	Interactive   bool              `json:"interactive"`
	Requires      map[string]string `json:"requires"`
	Timeouts      Timeouts          `json:"timeouts"`
	Retry         Retry             `json:"retry"`
	TreeBudgetEUR float64           `json:"tree_budget_eur"`
	GoalTrust     string            `json:"goal_trust"`
	// ResultTTL is the period of the files that became the type's result (ART-1).
	ResultTTL string `json:"result_ttl"`
	// ResultMediaTypes are the sniffed types of the result files people and sinks get (ART-5).
	ResultMediaTypes []string `json:"result_media_types"`
	// SummaryForManagers is the description of the type that a manager sees (ORC-13).
	SummaryForManagers string `json:"summary_for_managers"`
	// GoalTemplates are the goals a script step of the type may name (SCR-4).
	GoalTemplates []GoalTemplate `json:"goal_templates"`
	// MaxChildren limits the direct children of a task; 10 when absent (PRM-6).
	MaxChildren *int `json:"max_children"`
	// Continuation is the reply continuation of the type's children (CNT-1).
	Continuation *struct {
		Type string `json:"type"`
		Goal string `json:"goal"`
	} `json:"continuation"`
	// FollowupWindowS is the follow-up window after need_input: true; 1800 when absent, 0
	// turns it off (CNT-5).
	FollowupWindowS *int `json:"followup_window_s"`
	// Run with approval none marks a pipeline that /run starts without a button (APR-17).
	Run *struct {
		Approval string `json:"approval"`
	} `json:"run"`
	// Memory says whether the model may propose facts and whether the attempt reads them (MEM-6).
	Memory *struct {
		Facts string `json:"facts"`
		Read  string `json:"read"`
	} `json:"memory"`
	// Events are the event types a job of the type may publish through P1 emit (IDM-4).
	Events []string `json:"events"`
	// Observes are the series a job of the type reads through P1 obs.query (OBV-3).
	Observes []string `json:"observes"`
	// Proposable false keeps the type and its schedules out of proposals (PRP-10).
	Proposable *bool `json:"proposable"`
	// MakesBackup marks a backup type: its run.finished.v1 events give the backup age (OBV-7).
	MakesBackup bool `json:"makes_backup"`
	// Inbox names the message types an attempt of the type reads (MSG-8).
	Inbox struct {
		Read []string `json:"read"`
	} `json:"inbox"`
	// GrantEligible, GrantEvalSet and Grant are the signs of the right to ask for a stronger
	// model (MGR-17).
	GrantEligible bool   `json:"grant_eligible"`
	GrantEvalSet  string `json:"grant_eval_set"`
	Grant         *struct {
		InputTokensMin int `json:"input_tokens_min"`
	} `json:"grant"`
}

// Timeouts are the type deadlines as durations (15m, 60s).
type Timeouts struct {
	StartBy    string `json:"start_by"`
	Lease      string `json:"lease"`
	Heartbeat  string `json:"heartbeat"`
	Attempt    string `json:"attempt"`
	StallAfter string `json:"stall_after"`
	Deadline   string `json:"deadline"`
	// ToolStartBy is the handout deadline of a tool call, at most 30 s (DLV-16).
	ToolStartBy string `json:"tool_start_by"`
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
	// Ingress is the block of a source channel, direction: in (ING-1 to ING-4).
	Ingress *Ingress `json:"ingress"`
}

// Ingress is the source block of a Channel: the event it gives, the authenticity check, the
// limits and the mapping of body fields to closed kinds (ING-1 to ING-4).
type Ingress struct {
	Event     string `json:"event"`
	Auth      string `json:"auth"`
	Tolerance string `json:"tolerance"`
	Limits    struct {
		PerMinute    int   `json:"per_minute"`
		MaxBodyBytes int64 `json:"max_body_bytes"`
	} `json:"limits"`
	Mapping map[string]IngressField `json:"mapping"`
}

// IngressField is one entry of ingress.mapping: a JSON pointer into the delivery body and the
// closed schema its value passes (ING-2).
type IngressField struct {
	From   string         `json:"from"`
	Schema map[string]any `json:"schema"`
}

// Source is the source of a source channel: the second segment of ingress.event, which names
// the service principal hook:<source>, the path and the delivery key (ING-3).
func (i *Ingress) Source() string {
	if i == nil {
		return ""
	}
	parts := strings.Split(i.Event, ".")
	if len(parts) < 3 || parts[0] != "ext" {
		return ""
	}
	return parts[1]
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
	// Checked is the date a person checked the prices (GW-17).
	Checked string `json:"checked"`
}

// DropPointer removes the field or the array element that a JSON pointer names from a
// decoded manifest. It reports whether something was removed.
func DropPointer(v map[string]any, ptr string) bool {
	toks := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	if ptr == "" || len(toks) == 0 {
		return false
	}
	for i := range toks {
		toks[i] = strings.ReplaceAll(strings.ReplaceAll(toks[i], "~1", "/"), "~0", "~")
	}
	var parent any = v
	for _, t := range toks[:len(toks)-1] {
		switch p := parent.(type) {
		case map[string]any:
			parent = p[t]
		case []any:
			n, err := strconv.Atoi(t)
			if err != nil || n < 0 || n >= len(p) {
				return false
			}
			parent = p[n]
		default:
			return false
		}
	}
	last := toks[len(toks)-1]
	if m, ok := parent.(map[string]any); ok {
		if _, has := m[last]; has {
			delete(m, last)
			return true
		}
	}
	return false
}

// Schedule is the spec of a Schedule manifest (TIM-3).
type Schedule struct {
	Cron     string `json:"cron"`
	Timezone string `json:"timezone"`
	Task     *struct {
		Type   string         `json:"type"`
		Goal   string         `json:"goal"`
		Params map[string]any `json:"params"`
	} `json:"task"`
	DeliverTo     []any `json:"deliver_to"`
	SkipIfWaiting *bool `json:"skip_if_waiting"`
	// Proposable false keeps the schedule out of proposals (PRP-10).
	Proposable *bool `json:"proposable"`
	// Locale is the language of code texts for the schedule's addresses (DLV-18).
	Locale string `json:"locale"`
}

// RoleInsight is the insight section of a role: the analysis types, the series and tools it
// reads and its limits (INS-1, INS-2, INS-4).
type RoleInsight struct {
	Types            []string `json:"types"`
	Observes         []string `json:"observes"`
	Tools            []string `json:"tools"`
	MaxData          string   `json:"max_data"`
	MaxRuns          *RunsPer `json:"max_runs"`
	MaxEventRuns     *RunsPer `json:"max_event_runs"`
	MergeWindow      string   `json:"merge_window"`
	MaxOpenProposals *int     `json:"max_open_proposals"`
	MaxPerWeek       *int     `json:"max_per_week"`
}

// RunsPer is a run limit per window.
type RunsPer struct {
	Per   string `json:"per"`
	Count int    `json:"count"`
}
