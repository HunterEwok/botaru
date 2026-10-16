// SPDX-License-Identifier: AGPL-3.0-only

package core

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"
)

// One dependency step in the catalog (GOV-13 dependency_1, GOV-38 rejected_dependency_1, TRN-7
// script_target). The skeleton names the step but no field of its own, so the step is read from
// the references the manifests already hold, from the dependent object to the object it depends
// on: a catalog resource depends on its machine (resources[].host) and on the tool plugins that
// read it (resources[].read_by, plugin/tool); a worker depends on its role and on the machine it
// runs on; a role depends on the task types it claims. A tenant and the farm depend on nothing
// by one step, since everything of the tenant would depend on them.

// subjectRef is an object in the form of an incident subject: its kind in snake case (resource,
// worker, role, task_type, tool_plugin) and its id.
type subjectRef struct{ Kind, ID string }

// subjectDeps are the objects the incident subject depends on by one step in the catalog.
func subjectDeps(ctx context.Context, tx pgx.Tx, cat *Catalog, tenant, kind, id string) ([]subjectRef, error) {
	var out []subjectRef
	add := func(k, v string) {
		if v != "" && !slices.Contains(out, subjectRef{k, v}) {
			out = append(out, subjectRef{k, v})
		}
	}
	switch kind {
	case "resource":
		spec, _ := tenantCatalog(cat, tenant)
		for _, r := range mlist(spec, "resources") {
			rm, _ := r.(map[string]any)
			if mstr(rm, "id") != id {
				continue
			}
			add("resource", mstr(rm, "host"))
			for _, ref := range mstrs(rm, "read_by") {
				plugin, _, _ := strings.Cut(ref, "/")
				add("tool_plugin", plugin)
			}
		}
	case "worker":
		var role, host string
		err := tx.QueryRow(ctx, `SELECT role, coalesce(host, '') FROM workers WHERE tenant = $1 AND lower(id) = lower($2)`, tenant, id).Scan(&role, &host)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		add("role", role)
		add("resource", host)
	case "role":
		for _, t := range cat.Roles[id].Claims {
			add("task_type", t)
		}
	}
	return out, nil
}

// changeTouchesDep says whether a change names as its target an object the incident subject
// depends on by one step (the rules dependency_1 and rejected_dependency_1).
func changeTouchesDep(ch proposalChange, deps []subjectRef) bool {
	if ch.ManifestChange == nil {
		return false
	}
	for _, op := range ch.ManifestChange.Ops {
		if slices.Contains(deps, subjectRef{snakeKind(op.Target.Kind), op.Target.Name}) {
			return true
		}
	}
	return false
}

// changeRule is the rule by which a change touches the incident subject: direct, the hop rule
// hop, or "" when it does not touch it.
func changeRule(ch proposalChange, kind, id string, deps []subjectRef, direct, hop string) string {
	switch {
	case changeTouches(ch, kind, id):
		return direct
	case changeTouchesDep(ch, deps):
		return hop
	}
	return ""
}
