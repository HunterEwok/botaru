// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"path/filepath"
	"slices"

	"github.com/HunterEwok/botaru/impl/internal/evals"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
)

// farmRoles are the role folders of examples/roles the stand runs beside hello and ops-reader:
// every bot of the farm except the deferred analyst (sandbox, tier 2) and assistant (DSP-10).
// The job role data-job runs on the simulator's own job worker. The translator and the campaign
// operator have no example role; the stand's own are in roles/ beside this file.
var farmRoles = []string{"data-reader", "data-job", "geo-reader", "pipeline-reader", "reporter",
	"reviewer-a", "reviewer-b", "farm-manager", "farm-manager-reader",
	"impl/internal/sim/roles/translator/role.yaml", "impl/internal/sim/roles/ops-operator/role.yaml"}

// jobTypes are the task types of data-job the core enqueues as system: those of the stand's
// schedules, the report a sensor check enqueues for the reporter and the ingest of a knowledge
// document.
var jobTypes = []string{"data.sensor_watch.v1", "pipeline.watch.v1", "check.http.v1", "data.sensor_check.v1", "report.sensor.v1", "kb.ingest.v1"}

// farmFiles are the example manifests of the commission, the farm manager, the incident
// classes, the sensor note and the analysis after an incident.
var farmFiles = []string{
	"examples/tenants/demo/commission.yaml",
	"examples/tenants/demo/governance.yaml",
	"examples/tenants/farm/governance.yaml",
	"examples/incidents/disk-filling.yaml",
	"examples/incidents/disk-full.yaml",
	"examples/incidents/store-low.yaml",
	"examples/messagetypes/report.sensor_note.v1.yaml",
	// The bench takes the types the example roles claim; farm-manager claims the analysis after
	// an incident only on the stand (farmManifests).
	"examples/tasktypes/manager.postmortem.v1.yaml",
}

// farmSeed gives the people of the seed what the farm bots need: alice and bob may ask the data
// bots, the translator and the operator, both run scripts, alice runs the monthly report (group
// lead), and bob decides the proposals of the commission, runs the merge gate of a manifest
// change, directs the farm manager (group deciders) and uploads manuals to the knowledge corpus.
func farmSeed(sd *evals.Seed) {
	if g := sd.Groups["staff"]; g != nil {
		caps, _ := g["caps"].([]any)
		g["caps"] = append(caps, "task:geo.describe.v1", "task:pipeline.status.v1", "task:script.translate.v1", "task:ops.rollout.v1",
			"kb.write:vendor-manuals")
	}
	sd.Groups["lead"] = map[string]any{"caps": []any{"task:report.monthly.numbers.v1", "task:report.monthly.v1"}, "data_up_to": "internal"}
	sd.Groups["deciders"] = map[string]any{"caps": []any{"proposal.decide:reliability", "proposal.gate:reliability", "manager.direct"}, "data_up_to": "internal"}
	// alice runs the scripts the translator drafts for her: a script with an action, a gate or a
	// step that may control services needs script.run, a privileged capability her code
	// from /me activates (PRM-13).
	sd.Groups["runners"] = map[string]any{"caps": []any{"script.run"}, "data_up_to": "summary"}
	for i := range sd.People {
		if sd.People[i].Name == "bob" {
			for _, g := range []string{"deciders", "runners"} {
				if !slices.Contains(sd.People[i].Groups, g) {
					sd.People[i].Groups = append(sd.People[i].Groups, g)
				}
			}
		}
		if sd.People[i].Name == "alice" {
			for _, g := range []string{"runners", "lead"} {
				if !slices.Contains(sd.People[i].Groups, g) {
					sd.People[i].Groups = append(sd.People[i].Groups, g)
				}
			}
		}
	}
}

// farmManifests adds the commission, the farm manager seat, the incident classes and the
// schedules to the bench set, in tenant demo, the template of every tenant of the farm. The
// incident classes open and resolve at once rather than after minutes, so a scenario sees the
// whole incident; insights says whether the analysis schedule runs (it is paused until a
// scenario needs it, since every run of it costs model calls). ops-reader may analyse more
// often than the weekly limit of its example, since a run of the stand plays weeks in minutes.
// The switches of each tenant come with its copy (tenantSwitches).
func farmManifests(ms, farm []map[string]any, insights bool) []map[string]any {
	ms = append(ms, farm...)
	for _, m := range ms {
		sp, name := mapOf(m["spec"]), mapOf(m["metadata"])["name"]
		switch {
		case m["kind"] == "Role" && name == "ops-reader":
			if in := mapOf(sp["insight"]); in != nil {
				in["max_runs"] = map[string]any{"per": "week", "count": 50}
				in["max_per_week"] = 10
			}
			// The sensor notes of data-job (MSG-5), read by the summary of the day.
			sp["messages"] = map[string]any{"receive": []any{map[string]any{"type": "report.sensor_note.v1"}}}
		case m["kind"] == "Role" && name == "data-job":
			to := make([]any, 0, len(NoteTo))
			for _, a := range NoteTo {
				to = append(to, a)
			}
			sp["messages"] = map[string]any{"send": []any{map[string]any{"type": "report.sensor_note.v1", "to": to}}}
		case m["kind"] == "TaskType" && name == "data.sensor_watch.v1":
			// The watch sends sensor notes of class summary, and a message may not be below the
			// mark of its attempt (MSG-6); the example's internal would refuse every note, while
			// the watch reads no tool and returns only a count.
			sp["data_class"] = "summary"
		case m["kind"] == "TaskType" && name == "ops.summary.v1":
			sp["inbox"] = map[string]any{"read": []any{"report.sensor_note.v1"}}
		case m["kind"] == "Role" && name == "farm-manager":
			// The analysis after an incident (MGR-53), which the example role leaves to a farm
			// that runs it.
			claims, _ := sp["claims"].([]any)
			if !slices.Contains(claims, any("manager.postmortem.v1")) {
				sp["claims"] = append(claims, "manager.postmortem.v1")
			}
		case m["kind"] == "Action" && name == "cleanup_pipeline_tmp":
			// The translator's scripts may ask for the cleanup (SCR-2); the example keeps it
			// out of scripts.
			sp["script"] = map[string]any{"allowed": true}
		case m["kind"] == "Tenant" && name == "demo":
			cmds := mapOf(sp["commands"])
			if cmds == nil {
				cmds = map[string]any{}
				sp["commands"] = cmds
			}
			cmds["/script"] = "script.translate.v1"
			cmds["/rollout"] = "ops.rollout.v1"
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
	sysCaps := []any{"msg.send:report.sensor_note.v1", "task:ops.insight.v1", "task:review.security.v1", "task:review.operational.v1", "task:review.cost.v1", "task:review.value.v1"}
	for _, t := range jobTypes {
		sysCaps = append(sysCaps, "task:"+t)
	}
	ms = append(ms,
		group("system-ops", sysCaps...),
		// The seat's group also enqueues the readings the manager asks for (MGR-25) and the
		// directives of people (MGR-4).
		group("farm-manager-ops", "task:manager.incident.respond.v1", "task:manager.postmortem.v1", "task:manager.inspect.v1", "task:manager.directive.v1", "control.apply:hold", "control.apply:quarantine",
			"control.apply:drain", "control.apply:preempt", "control.apply:cancel"),
		schedule("ops-insight", "* * * * *", "ops.insight.v1", !insights),
		schedule("pipelines-watch", "*/2 * * * *", "pipeline.watch.v1", false),
		schedule("sensor-watch", "* * * * *", "data.sensor_watch.v1", false),
		schedule("ext-checks", "* * * * *", "check.http.v1", false),
		// The nightly sensor check runs when a scenario or the load turns it on (SetSchedule).
		schedule(SensorSchedule, "* * * * *", "data.sensor_check.v1", true),
	)
	for _, m := range ms[len(ms)-1:] {
		mapOf(m["spec"])["task"] = map[string]any{"type": "data.sensor_check.v1", "params": map[string]any{"window_h": 24}}
	}
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
			// Without that test both disk classes would open on every disk fault and the farm
			// manager would answer each, so the stand keeps disk_full, the one its scenarios check.
			if mapOf(d.Value["metadata"])["name"] == "disk_filling" {
				continue
			}
		}
		out = append(out, d.Value)
	}
	return out, nil
}

func mapOf(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}
