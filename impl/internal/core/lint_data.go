// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// lintData runs the set checks of what leaves the farm and what reaches a model: the result
// file types of a task type (ART-5), the model exposure of datasets (PRV-4) and the export sinks
// of a person's copy (SEC-10, LCY-2).
func (c *Core) lintData(S *lintSet, f *lintFindings) {
	// Active formats are never declared, and application/pdf only by a job type, whose code
	// renders it from closed fields (ART-5). The skeleton lint has no code for this, so the
	// reason code of the refused handout names it.
	for _, o := range S.each("TaskType") {
		for j, mt := range mstrs(o.spec, "result_media_types") {
			if activeMediaType(mt) || mt == "application/pdf" && mstr(o.spec, "worker_kind") != "job" {
				f.add(o, "media_type_not_allowed", fmt.Sprintf("/result_media_types/%d", j))
			}
		}
	}

	// Model exposure of datasets (PRV-4): the tools of agent roles in read_by.
	tools := map[string]toolspec.Tool{}
	for _, o := range S.each("ToolPlugin") {
		for _, t := range mlist(o.spec, "tools") {
			tm, _ := t.(map[string]any)
			if tm == nil {
				continue
			}
			var tl toolspec.Tool
			if b, err := json.Marshal(tm); err == nil && json.Unmarshal(b, &tl) == nil {
				tools[o.name+"/"+tl.Name] = tl
			}
		}
	}
	agentTools := map[string]bool{}
	for _, o := range S.each("Role") {
		if mstr(o.spec, "worker_kind") != "agent" {
			continue
		}
		for _, ref := range mstrs(o.spec, "tools") {
			if strings.Contains(ref, "/") {
				agentTools[ref] = true
				continue
			}
			for k, t := range tools {
				if strings.HasPrefix(k, ref+"/") && t.On() {
					agentTools[k] = true
				}
			}
		}
	}
	for _, o := range S.each("Catalog") {
		for j, r := range mlist(o.spec, "resources") {
			rm, _ := r.(map[string]any)
			exp := modelExposure(rm)
			if exp == "" || exp == "full" {
				continue
			}
			for _, ref := range mstrs(rm, "read_by") {
				if !agentTools[ref] {
					continue
				}
				switch {
				case exp == "none":
					f.add(o, "model_exposure_agent_read", fmt.Sprintf("/resources/%d", j))
				case exp == "aggregates" && !tools[ref].TrustedOutput():
					f.add(o, "model_exposure_not_aggregate", fmt.Sprintf("/resources/%d", j))
				}
			}
		}
	}

	// Export sinks (SEC-10): export_only names exactly one recipient, and the sink of /mydata
	// export sends a copy only to the person.
	for _, o := range S.each("Sink") {
		if eo, ok := o.spec["export_only"].(map[string]any); ok {
			_, a := eo["recipient"]
			_, b := eo["recipient_ref"]
			if a == b {
				f.add(o, "export_only_recipient", "/export_only")
			}
		}
	}
	for _, o := range S.each("Tenant") {
		es := mstr(mmap(o.spec, "privacy"), "export_sink")
		if es == "" {
			continue
		}
		sk := S.get("Sink", es, o.name)
		switch {
		case sk == nil:
			f.add(o, "ref_missing", "/privacy/export_sink")
		case mstr(mmap(sk, "export_only"), "recipient_ref") != "self":
			f.add(o, "export_sink_not_self", "/privacy/export_sink")
		}
	}
}
