// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"slices"
	"strings"
)

// ingressSourceMax is the longest source of a source channel: hook:<source>, the chat id and the
// delivery key system:ingress:<source>:<id> must fit their limits (ING-3).
const ingressSourceMax = 32

// ingressToleranceMax is the widest tolerance on a signature timestamp (ING-1).
const ingressToleranceMax = 300

// eventSource is the source of an external event name ext.<source>.<name>.vN, or "".
func eventSource(event string) string {
	parts := strings.Split(event, ".")
	if len(parts) < 3 {
		return ""
	}
	return parts[1]
}

// lintIngress checks the source channels and the routes on events, as ci/check_manifests.py
// does for the skeleton (ING-1, ING-3, ING-4, INJ-7):
//   - a source channel (direction: in) carries an ingress block (ingress_missing);
//   - its source is not monitor, which belongs to the E5 adapter (ingress_reserved_source,
//     MON-5), is at most 32 characters (ingress_source_too_long) and is the source of one
//     channel of the tenant (ingress_source_duplicate);
//   - auth: signature needs a tolerance of at most 5 minutes (ingress_tolerance);
//   - a route on an event of a tls_token source enqueues only a type of tier 0, since a token
//     can be replayed (ingress_token_tier);
//   - a route on an event that an agent type declares does not enqueue a type with alarm.* in
//     its events: an alarm is raised only by code from records code wrote
//     (route_untrusted_source, INJ-7).
func (c *Core) lintIngress(S *lintSet, f *lintFindings) {
	tokenEvents := map[string]map[string]bool{}
	sources := map[[2]string]string{}
	for _, o := range S.each("Channel") {
		ing := mmap(o.spec, "ingress")
		if mstr(o.spec, "direction") == "in" && len(ing) == 0 {
			f.add(o, "ingress_missing", "/ingress")
		}
		if len(ing) == 0 {
			continue
		}
		event := mstr(ing, "event")
		source := eventSource(event)
		switch {
		case source == "monitor":
			f.add(o, "ingress_reserved_source", "/ingress/event")
		case len(source) > ingressSourceMax:
			f.add(o, "ingress_source_too_long", "/ingress/event")
		}
		if source != "" {
			key := [2]string{o.tenant, source}
			if other, ok := sources[key]; !ok {
				sources[key] = o.name
			} else if other != o.name {
				f.add(o, "ingress_source_duplicate", "/ingress/event")
			}
		}
		switch mstr(ing, "auth") {
		case "signature":
			d, ok := parseDuration(mstr(ing, "tolerance"))
			if !ok || d.Seconds() > ingressToleranceMax {
				f.add(o, "ingress_tolerance", "/ingress/tolerance")
			}
		case "tls_token":
			if event != "" {
				if tokenEvents[o.tenant] == nil {
					tokenEvents[o.tenant] = map[string]bool{}
				}
				tokenEvents[o.tenant][event] = true
			}
		}
	}
	agentEvents := map[string]bool{}
	for _, o := range S.each("TaskType") {
		if mstr(o.spec, "worker_kind") == "agent" {
			for _, e := range mstrs(o.spec, "events") {
				agentEvents[e] = true
			}
		}
	}
	for _, o := range S.each("Route") {
		ev := mstr(mmap(o.spec, "on"), "event")
		tt := mstr(mmap(o.spec, "task"), "type")
		if ev == "" || tt == "" {
			continue
		}
		ts := S.get("TaskType", tt, "")
		if tokenEvents[o.tenant][ev] {
			if tier, ok := mnum(ts, "tier"); ts == nil || !ok || tier > 0 {
				f.add(o, "ingress_token_tier", "/task/type")
			}
		}
		if agentEvents[ev] && slices.ContainsFunc(mstrs(ts, "events"), func(e string) bool { return strings.HasPrefix(e, "alarm.") }) {
			f.add(o, "route_untrusted_source", "/on/event")
		}
	}
}
