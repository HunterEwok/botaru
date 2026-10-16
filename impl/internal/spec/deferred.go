// SPDX-License-Identifier: AGPL-3.0-only

package spec

import (
	"strconv"
	"strings"
)

// Surfaces of a deferred rule: where a request could reach its mechanism.
const (
	// SurfaceManifest is a manifest field: Kind ("*" for any kind) and the pointer Where.
	SurfaceManifest = "manifest"
	// SurfaceOp is a P1 or P2 operation ("P2.task.replay"), or with Field a field of its body.
	SurfaceOp = "op"
	// SurfaceTool is a built-in tool of the core, or with Field a field of its arguments.
	SurfaceTool = "tool"
	// SurfaceHello is a field of the P1 hello body (Field).
	SurfaceHello = "hello"
	// SurfaceNone is a deferred rule that has no surface a request reaches; Note says why.
	SurfaceNone = "none"
)

// DeferredEntry is one surface of a rule with status deferred in conformance/requirements.yaml.
// A deferred rule is in no skeleton version, and an implementation does not offer its
// mechanism even where its fields are still in the schemas (conformance/deferred.md, D-114):
// the core refuses each surface with version_unsupported and the path, like a field of a later
// version (MAN-11).
type DeferredEntry struct {
	Rule    string
	Surface string
	// Kind is the manifest kind of a manifest surface.
	Kind string
	// Where is the manifest pointer, the operation ("P1.enqueue") or the built-in tool name.
	Where string
	// Field is a pointer into the body of an operation, a tool or hello; empty is the whole
	// operation or tool. "*" stands for any one segment in Where and Field.
	Field string
	// Value, when set, limits the surface to that value of the field; Except lets one value
	// pass (the value that turns the mechanism off).
	Value  any
	Except any
	// Note says why a rule has no surface, or what else holds its mechanism back.
	Note string
}

// Deferred lists, by rule, the surfaces of the deferred rules (D3 of the v0.3 plan). Every
// deferred rule of the registry has at least one entry; a rule without a surface has one entry
// of SurfaceNone with its note (TestDeferredCoversRegistry).
var Deferred = []DeferredEntry{
	{Rule: "APR-8", Surface: SurfaceOp, Where: "P2.task.replay"},
	{Rule: "APR-13", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/attention/fast_decision_s"},
	{Rule: "AUD-4", Surface: SurfaceOp, Where: "P2.task.journal",
		Note: "retention.journals and operator_access.debug_journal are fields of the debug profile, which this core does not claim (MAN-11)"},
	{Rule: "BUD-6", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/budgets/tasks_in_flight"},
	{Rule: "BUD-8", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/dispatch/background_share"},
	{Rule: "CAP-3", Surface: SurfaceManifest, Kind: "Channel", Where: "/spec/rate"},
	{Rule: "CNT-6", Surface: SurfaceManifest, Kind: "TaskType", Where: "/spec/await"},
	{Rule: "COM-9", Surface: SurfaceNone,
		Note: "the shadow field of a review result: with no shadow tasks (COM-10) the verdict code counts a review with shadow: true as schema_invalid"},
	{Rule: "COM-10", Surface: SurfaceManifest, Kind: "Commission", Where: "/spec/shadow", Except: "off"},
	{Rule: "COM-18", Surface: SurfaceManifest, Kind: "Governance", Where: "/spec/commission/seats/*/reward_profile"},
	{Rule: "COM-18", Surface: SurfaceManifest, Kind: "Governance", Where: "/spec/commission/reward_cap_eur_month"},
	{Rule: "DIG-9", Surface: SurfaceNone, Note: "the digest item replay_item follows a replay (PRP-25), which this core never runs"},
	{Rule: "DLV-17", Surface: SurfaceManifest, Kind: "Sink", Where: "/spec/driver", Value: "webhook"},
	{Rule: "DLV-17", Surface: SurfaceManifest, Kind: "Sink", Where: "/spec/signing_key_ref"},
	{Rule: "DSP-9", Surface: SurfaceOp, Where: "P2.dispatch.explain"},
	{Rule: "DSP-9", Surface: SurfaceOp, Where: "P2.dispatch.queue"},
	{Rule: "DSP-10", Surface: SurfaceOp, Where: "P2.chat.bind", Field: "/assistant", Value: true,
		Note: "a role with memory: person is refused by the lint person_memory_deferred (MEM-2)"},
	{Rule: "DSP-11", Surface: SurfaceManifest, Kind: "BotGroup", Where: "/spec/max_running"},
	{Rule: "DSP-11", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/dispatch/rules/*/max_share"},
	{Rule: "GOV-35", Surface: SurfaceNone,
		Note: "a locked commission is a state code would compute from three months of flags; this core computes none, sends no commission_review card and shows no commission_locked"},
	{Rule: "GOV-39", Surface: SurfaceTool, Where: "link_hint"},
	{Rule: "INS-5", Surface: SurfaceNone,
		Note: "the turn order of analysis tasks (dispatcher condition D16) has no field or operation; analysis tasks go by the ordinary dispatch order"},
	{Rule: "KNW-6", Surface: SurfaceNone,
		Note: "kb_search and kb_read are tools of the active rules KNW-1 to KNW-5; KNW-6 only fixed their inner word search, which this core chooses itself (impl/README.md)"},
	{Rule: "LCY-5", Surface: SurfaceManifest, Kind: "ToolPlugin", Where: "/spec/source/accepted"},
	{Rule: "MAN-7", Surface: SurfaceManifest, Kind: "*", Where: "/metadata/deprecated"},
	{Rule: "MAN-7", Surface: SurfaceManifest, Kind: "*", Where: "/metadata/sunset"},
	{Rule: "MAN-12", Surface: SurfaceNone,
		Note: "an OpenAPI document of the P1 binding; the core serves the P1 paths of spec/ports/operations.yaml"},
	{Rule: "MEM-5", Surface: SurfaceManifest, Kind: "TaskType", Where: "/spec/conversation_concurrency"},
	{Rule: "MGR-12", Surface: SurfaceTool, Where: "control_ops", Field: "/op", Value: "pin_task"},
	{Rule: "MGR-12", Surface: SurfaceOp, Where: "P2.control.apply", Field: "/op", Value: "pin_task"},
	{Rule: "MON-7", Surface: SurfaceManifest, Kind: "Role", Where: "/spec/outputs"},
	{Rule: "MON-7", Surface: SurfaceOp, Where: "P1.complete", Field: "/monitor_note"},
	{Rule: "OBS-13", Surface: SurfaceNone,
		Note: "the metrics catalog of the telemetry binding: the core exports none, and no core decision reads a metric"},
	{Rule: "OBV-8", Surface: SurfaceOp, Where: "P1.obs.append"},
	{Rule: "ORC-15", Surface: SurfaceOp, Where: "P1.campaign.propose", Field: "/rollout/waves"},
	{Rule: "ORC-15", Surface: SurfaceOp, Where: "P1.campaign.propose", Field: "/rollout/rate_per_min"},
	{Rule: "ORC-15", Surface: SurfaceTool, Where: "propose_campaign", Field: "/rollout/waves"},
	{Rule: "ORC-15", Surface: SurfaceTool, Where: "propose_campaign", Field: "/rollout/rate_per_min"},
	{Rule: "PRM-11", Surface: SurfaceOp, Where: "P1.enqueue", Field: "/mode"},
	{Rule: "PRP-17", Surface: SurfaceNone,
		Note: "the related_to mark is computed by code from decided proposals and has no input; this core computes none"},
	{Rule: "PRP-25", Surface: SurfaceNone,
		Note: "the replay of a dismissed proposal is run by code and has no input; this core never replays"},
	{Rule: "RET-9", Surface: SurfaceNone,
		Note: "Tenant.spec.retention stays served because the active RET-10 deletes by its periods; retention.journals is of the debug profile and refused (MAN-11)"},
	{Rule: "SBX-1", Surface: SurfaceNone,
		Note: "tier 2 and placement: sandbox belong to the sandbox profile, which this core does not claim, so apply refuses them (MAN-11)"},
	{Rule: "SBX-2", Surface: SurfaceNone, Note: "the sandbox toolhost belongs to the sandbox profile, which this core does not claim (MAN-11)"},
	{Rule: "SCR-12", Surface: SurfaceManifest, Kind: "Script", Where: "/kind"},
	{Rule: "SCR-12", Surface: SurfaceManifest, Kind: "Schedule", Where: "/spec/script"},
	{Rule: "SCR-13", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/scripts/wait_run_deadline",
		Note: "a script draft with a wait or await step, or with their fields, is refused at acceptance with script_invalid (SCR-5)"},
	{Rule: "SCR-14", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/script_commands",
		Note: "kind: Script is refused with SCR-12; a draft with params or goals is refused with script_free_goal"},
	{Rule: "SCR-15", Surface: SurfaceNone, Note: "mechanism removed: the condition-ops registry has only eq, ne and in"},
	{Rule: "SEC-7", Surface: SurfaceManifest, Kind: "Catalog", Where: "/spec/connection_providers",
		Note: "a tool credential per_principal belongs to the connections profile, which this core does not claim (MAN-11)"},
	{Rule: "SEC-7", Surface: SurfaceOp, Where: "P2.connection.register"},
	{Rule: "SEC-7", Surface: SurfaceOp, Where: "P2.connection.revoke"},
	{Rule: "SEC-12", Surface: SurfaceOp, Where: "P2.tenant.privacy_report"},
	{Rule: "SKL-4", Surface: SurfaceHello, Field: "/runtime/skills", Value: "farm"},
	{Rule: "SKL-4", Surface: SurfaceTool, Where: "skill_read"},
	{Rule: "STA-3", Surface: SurfaceNone,
		Note: "the self-report column and the mismatch code of status.workers are outputs: the core shows the core observation only (Offered)"},
	{Rule: "TRN-8", Surface: SurfaceManifest, Kind: "Tenant", Where: "/spec/evals/translator_clarify_share_max"},
}

// Offered says whether the mechanism of a rule is offered: false for a rule of Deferred. Code
// that a deferred rule would need, kept for when it returns, runs only behind it.
func Offered(rule string) bool {
	for _, e := range Deferred {
		if e.Rule == rule {
			return false
		}
	}
	return true
}

// DeferredHit is a surface of a deferred rule that a request reaches: the pointer of the value
// ("" for a whole operation or tool) and the rule.
type DeferredHit struct {
	Ptr, Rule string
}

// DeferredManifest returns the pointers of the fields of a manifest that belong to deferred
// rules, with the rule of each; "/kind" stands for a kind of a deferred rule.
func DeferredManifest(kind string, v map[string]any) []DeferredHit {
	var out []DeferredHit
	for _, e := range Deferred {
		if e.Surface != SurfaceManifest || e.Kind != "*" && e.Kind != kind {
			continue
		}
		if e.Where == "/kind" {
			out = append(out, DeferredHit{Ptr: "/kind", Rule: e.Rule})
			continue
		}
		for _, p := range e.find(v, e.Where) {
			out = append(out, DeferredHit{Ptr: p, Rule: e.Rule})
		}
	}
	return out
}

// DeferredCall returns the surface of a deferred rule that a call of an operation ("P1.enqueue",
// "P2.task.replay") or a built-in tool reaches with its body, if any. surface is SurfaceOp,
// SurfaceTool or SurfaceHello (where is then ignored).
func DeferredCall(surface, where string, body map[string]any) (DeferredHit, bool) {
	for _, e := range Deferred {
		if e.Surface != surface || surface != SurfaceHello && e.Where != where {
			continue
		}
		if e.Field == "" {
			return DeferredHit{Rule: e.Rule}, true
		}
		if ps := e.find(body, e.Field); len(ps) > 0 {
			return DeferredHit{Ptr: ps[0], Rule: e.Rule}, true
		}
	}
	return DeferredHit{}, false
}

// find returns the pointers in v that the pattern names and that hold a value of the entry.
func (e DeferredEntry) find(v any, pattern string) []string {
	var out []string
	var walk func(x any, segs []string, at string)
	walk = func(x any, segs []string, at string) {
		if len(segs) == 0 {
			if e.Value != nil && !sameScalar(e.Value, x) || e.Except != nil && sameScalar(e.Except, x) {
				return
			}
			out = append(out, at)
			return
		}
		switch c := x.(type) {
		case map[string]any:
			if segs[0] == "*" {
				for k, y := range c {
					walk(y, segs[1:], at+"/"+escape(k))
				}
			} else if y, ok := c[segs[0]]; ok {
				walk(y, segs[1:], at+"/"+escape(segs[0]))
			}
		case []any:
			for i, y := range c {
				if segs[0] == "*" || segs[0] == strconv.Itoa(i) {
					walk(y, segs[1:], at+"/"+strconv.Itoa(i))
				}
			}
		}
	}
	walk(v, strings.Split(strings.TrimPrefix(pattern, "/"), "/"), "")
	return out
}
