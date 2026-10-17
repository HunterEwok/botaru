// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"path/filepath"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// farmRoles are the role folders of examples/roles the stand runs beside hello and ops-reader:
// every bot of the farm (the job role data-job runs on the simulator's own job worker).
var farmRoles = []string{"data-reader", "data-job", "geo-reader", "pipeline-reader", "reporter",
	"reviewer-a", "reviewer-b", "farm-manager", "farm-manager-reader"}

// jobTypes are the task types of data-job the schedules of the stand enqueue.
var jobTypes = []string{"data.sensor_watch.v1", "pipeline.watch.v1", "check.http.v1"}

// farmFiles are the example manifests of the commission, the farm manager and the incident
// classes.
var farmFiles = []string{
	"examples/tenants/demo/commission.yaml",
	"examples/tenants/demo/governance.yaml",
	"examples/tenants/farm/governance.yaml",
	"examples/incidents/disk-filling.yaml",
	"examples/incidents/disk-full.yaml",
	"examples/incidents/store-low.yaml",
}

// farmSeed gives the people of the seed what the farm bots need: alice and bob may ask the data
// bots, and bob decides the proposals of the commission (group deciders).
func farmSeed(sd *evals.Seed) {
	if g := sd.Groups["staff"]; g != nil {
		caps, _ := g["caps"].([]any)
		g["caps"] = append(caps, "task:geo.describe.v1", "task:pipeline.status.v1")
	}
	sd.Groups["deciders"] = map[string]any{"caps": []any{"proposal.decide:reliability"}, "data_up_to": "internal"}
	for i := range sd.People {
		if sd.People[i].Name == "bob" && !slices.Contains(sd.People[i].Groups, "deciders") {
			sd.People[i].Groups = append(sd.People[i].Groups, "deciders")
		}
	}
}

// farmManifests adds the commission, the farm manager seat, the incident classes and the
// schedules to the bench set. The incident classes open and resolve at once rather than after
// minutes, so a scenario sees the whole incident; insights says whether the analysis schedule
// runs (it is paused until a scenario needs it, since every run of it costs model calls).
func farmManifests(ms, farm []map[string]any, insights bool) []map[string]any {
	ms = append(ms, farm...)
	for _, m := range ms {
		if m["kind"] == "Tenant" && mapOf(m["metadata"])["name"] == "demo" {
			sp := mapOf(m["spec"])
			sp["farm_manager"] = map[string]any{"enabled": true}
			ins := mapOf(sp["insights"])
			if ins == nil {
				ins = map[string]any{}
				sp["insights"] = ins
			}
			ins["present_to"] = "group:deciders"
			ins["digest"] = map[string]any{"schedule": "* * * * *", "max_items": 5}
		}
	}
	group := func(name string, caps ...any) map[string]any {
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Group", "metadata": map[string]any{"name": name, "tenant": "demo"},
			"spec": map[string]any{"caps": caps, "data": []any{"summary", "internal"}}}
	}
	schedule := func(name, cron, typ string, paused bool) map[string]any {
		return map[string]any{"apiVersion": "botaru/v1alpha1", "kind": "Schedule", "metadata": map[string]any{"name": name, "tenant": "demo"},
			"spec": map[string]any{"cron": cron, "task": map[string]any{"type": typ}, "skip_if_waiting": true, "proposable": false, "paused": paused}}
	}
	sysCaps := []any{"task:ops.insight.v1", "task:review.security.v1", "task:review.operational.v1", "task:review.cost.v1", "task:review.value.v1"}
	for _, t := range jobTypes {
		sysCaps = append(sysCaps, "task:"+t)
	}
	ms = append(ms,
		group("system-ops", sysCaps...),
		group("farm-manager-ops", "task:manager.incident.respond.v1", "control.apply:hold", "control.apply:quarantine",
			"control.apply:drain", "control.apply:preempt", "control.apply:cancel"),
		schedule("ops-insight", "* * * * *", "ops.insight.v1", !insights),
		schedule("pipelines-watch", "*/2 * * * *", "pipeline.watch.v1", false),
		schedule("sensor-watch", "*/5 * * * *", "data.sensor_watch.v1", false),
		schedule("ext-checks", "* * * * *", "check.http.v1", false),
	)
	return ms
}

// readFarm reads the example manifests of farmFiles, the incident classes without their waits
// and their tests of the alarm numbers.
func readFarm(root string) ([]map[string]any, error) {
	var paths []string
	for _, f := range farmFiles {
		paths = append(paths, filepath.Join(root, f))
	}
	docs, err := manifest.ReadFiles(paths)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, d := range docs {
		if d.Kind == "IncidentClass" {
			sp := mapOf(d.Value["spec"])
			delete(mapOf(sp["detect"]), "for")
			delete(mapOf(sp["resolve"]), "for")
			// The free share the disk classes test comes with the alarm of the MON-5 re-check job,
			// which the reference does not run yet: the core raises the alarm of a monitoring
			// problem without numbers, so on the stand the classes open on the alarm alone.
			delete(mapOf(sp["detect"]), "when")
		}
		out = append(out, d.Value)
	}
	return out, nil
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
