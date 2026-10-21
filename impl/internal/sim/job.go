// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/kbjob"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// startJob enrolls and runs a job worker of the role data-job in a tenant: the code of the
// schedule tasks, the monthly report numbers and the ingest of knowledge documents, which reads
// the world and uses no model.
func (s *Stand) startJob(ctx, run context.Context, tenant string) error {
	tok, err := s.Bench.Enroll(ctx, map[string]any{"tenant": tenant, "role": "data-job", "kind": "job"})
	if err != nil {
		return err
	}
	c := &p1client.Client{BaseURL: s.Bench.P1(), Token: tok, HTTP: &http.Client{Timeout: 2 * time.Minute}}
	if err := c.Call(ctx, "hello", "", map[string]any{"kind": "job", "implementation": "botaru-sim " + version.Version,
		"capacity": 1, "protocol_versions": []string{"v1"}}, nil); err != nil {
		return err
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		for run.Err() == nil {
			if err := s.jobOnce(run, c, tenant); err != nil && run.Err() == nil {
				s.log.Addf(KindDirector, "the data-job worker of %s: %v", tenant, err)
				select {
				case <-run.Done():
				case <-time.After(2 * time.Second):
				}
			}
		}
	}()
	return nil
}

// jobOnce claims one attempt and completes it with the numbers of the tenant's world.
func (s *Stand) jobOnce(ctx context.Context, c *p1client.Client, tenant string) error {
	w, err := c.Claim(ctx, []string{"attempt"}, 20)
	if err != nil || w == nil {
		return err
	}
	lease := w.Lease()
	var p struct {
		Task struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Body struct {
				Params map[string]any `json:"params"`
			} `json:"body"`
		} `json:"task"`
	}
	_ = json.Unmarshal(w.Payload, &p)
	var result map[string]any
	switch p.Task.Type {
	case "check.http.v1":
		result = s.httpChecks(tenant)
	case "pipeline.watch.v1":
		result = map[string]any{"finished": s.finishedRuns(tenant)}
	case "data.sensor_watch.v1":
		silent, err := s.sensorNotes(ctx, c, lease, tenant, p.Task.ID)
		if err != nil {
			return failJob(ctx, c, lease, err)
		}
		result = map[string]any{"silent": silent}
	case "data.sensor_check.v1":
		sensors := s.sensorNumbers(tenant)
		if len(sensors) == 0 {
			// A catalog without sensors is a fault of the stand, not of the core.
			return c.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": false}, nil)
		}
		issues := 0
		for _, x := range sensors {
			if x["gaps"] != 0 || x["stuck_channel"] != "none" {
				issues++
			}
		}
		// The numbers go to the reporter in params; the code of the core draws the table (flow 5).
		if err := enqueueChild(ctx, c, lease, p.Task.ID, "report.sensor.v1", "Morning sensor summary for the lab chat. 2-3 sentences. Numbers only from params.",
			map[string]any{"sensors": sensors, "date": time.Now().UTC().Format(time.DateOnly)}); err != nil {
			// The known gap only when the report is in the set: data-job's may_enqueue names it,
			// so the refusal comes from the rights of system.
			if strings.Contains(err.Error(), "type_not_allowed") && s.Bench.ResultSchema("report.sensor.v1") != nil {
				s.childRefused.Add(1)
			}
			return failJob(ctx, c, lease, err)
		}
		result = map[string]any{"sensors_checked": len(sensors), "sensors_with_issues": issues}
	case "report.monthly.numbers.v1":
		month, _ := p.Task.Body.Params["month"].(string)
		nums, err := s.monthNumbers(ctx, tenant, month)
		if err != nil {
			return failJob(ctx, c, lease, err)
		}
		params := map[string]any{"month": month}
		for k, v := range nums {
			params[k] = v
		}
		if err := enqueueChild(ctx, c, lease, p.Task.ID, "report.monthly.v1", "Report for the month. One paragraph. Numbers only from params.", params); err != nil {
			return failJob(ctx, c, lease, err)
		}
		result = map[string]any{"month": month, "ctd_qc_runs": nums["ctd_qc_runs"], "lab_runs": nums["lab_runs"], "cost_eur": nums["cost_eur"]}
	case "kb.ingest.v1":
		return kbIngest(ctx, c, lease, w.Payload)
	default:
		return c.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": false}, nil)
	}
	return c.Call(ctx, "complete", lease, map[string]any{"result": result}, nil)
}

// failJob ends an attempt the job could not do, to be retried.
func failJob(ctx context.Context, c *p1client.Client, lease string, err error) error {
	if e := c.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": true}, nil); e != nil {
		return e
	}
	return err
}

// enqueueChild enqueues the report task of a job (the role's may_enqueue), once per job task.
func enqueueChild(ctx context.Context, c *p1client.Client, lease, parent, typ, goal string, params map[string]any) error {
	return c.Call(ctx, "enqueue", lease, map[string]any{"body": map[string]any{"type": typ, "goal": goal, "params": params},
		"idempotency_key": "sim:" + typ + ":" + parent}, nil)
}

// NoteTo are the addresses of the sensor notes of data-job: the inbox of ops-reader. A bot
// group address cannot be among them: a role is a global manifest, and the lint of global
// manifests refuses any group:<name> in one (TEN-6, global_tenant_data).
var NoteTo = []string{"role:ops-reader"}

// sensorNotes sends a sensor note (report.sensor_note.v1) for every sensor of the tenant's
// catalog the world keeps silent, to each address of NoteTo, and returns how many are silent.
// The notes are published when the watch completes (MSG-9).
func (s *Stand) sensorNotes(ctx context.Context, c *p1client.Client, lease, tenant, task string) (int, error) {
	w, err := LoadWorld(s.cfg.Work)
	if err != nil {
		return 0, err
	}
	silent := 0
	for _, id := range catalogList(s.cfg.Work, tenant, "sensors") {
		if !w.Faults[SensorFault(tenant, id)] {
			continue
		}
		silent++
		for _, to := range NoteTo {
			key := "sim:note:" + task + ":" + id + ":" + to
			if err := c.Call(ctx, "msg.send", lease, map[string]any{"type": "report.sensor_note.v1", "to": to,
				"fields": map[string]any{"sensor": id, "issue": "gap", "hours": 1}, "idempotency_key": key}, nil); err != nil {
				return 0, fmt.Errorf("msg.send to %s: %w", to, err)
			}
		}
	}
	return silent, nil
}

// sensorNumbers are the nightly numbers of the sensors in a tenant's catalog: the first sensor
// is fine, every further one had two gaps in the night and a humidity channel stuck at one value.
func (s *Stand) sensorNumbers(tenant string) []map[string]any {
	out := []map[string]any{}
	for i, id := range catalogList(s.cfg.Work, tenant, "sensors") {
		x := map[string]any{"id": id, "completeness_pct": 99.6, "gaps": 0, "out_of_range": 0, "stuck_channel": "none"}
		if i > 0 {
			x["completeness_pct"], x["gaps"], x["stuck_channel"] = 71.2, 2, "humidity"
		}
		out = append(out, x)
		if len(out) == 50 {
			break
		}
	}
	return out
}

// catalogList is the values of a list resource of a tenant's catalog.
func catalogList(work, tenant, list string) []string {
	raw, err := os.ReadFile(catalogFile(work, tenant))
	var m struct {
		Spec struct {
			Resources []struct {
				List   string   `json:"list"`
				Values []string `json:"values"`
			} `json:"resources"`
		} `json:"spec"`
	}
	if err != nil || json.Unmarshal(raw, &m) != nil {
		return nil
	}
	var out []string
	for _, r := range m.Spec.Resources {
		if r.List == list {
			out = append(out, r.Values...)
		}
	}
	return out
}

// monthNumbers are the numbers of a tenant's monthly report: its CTD QC runs and how many passed,
// the oneshot runs of its queue and the spend of its tasks so far.
func (s *Stand) monthNumbers(ctx context.Context, tenant, month string) (map[string]any, error) {
	var runs []run
	if raw, err := os.ReadFile(runsFile(s.cfg.Work, tenant)); err == nil {
		_ = json.Unmarshal(raw, &runs)
	}
	ok := 0
	for _, r := range runs {
		if r.State == "done" {
			ok++
		}
	}
	done, _ := filepath.Glob(filepath.Join(queueDir(s.cfg.Work, tenant), "done", "*.json"))
	var micros int64
	if err := s.Bench.Store().Pool.QueryRow(ctx, `SELECT coalesce(sum(eur_micros + coalesce(correction_eur_micros, 0)), 0) FROM spend
		WHERE tenant = $1 AND unit <> 'task'`, tenant).Scan(&micros); err != nil {
		return nil, err
	}
	return map[string]any{"ctd_qc_runs": len(runs), "ctd_qc_ok": ok, "lab_runs": min(len(done), 100000), "lab_ok": min(len(done), 100000),
		"cost_eur": math.Round(float64(micros)/1e4) / 100}, nil
}

// kbIngest cuts the document of an ingest task into fragments and writes them, as the reference
// ingest job does (kbjob): the core takes the corpus and the document from the leased task.
func kbIngest(ctx context.Context, c *p1client.Client, lease string, payload json.RawMessage) error {
	var p struct {
		KBDocument *struct {
			ID      string `json:"id"`
			Corpus  string `json:"corpus"`
			FileRef string `json:"file_ref"`
		} `json:"kb_document"`
	}
	if json.Unmarshal(payload, &p) != nil || p.KBDocument == nil {
		return c.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": false}, nil)
	}
	var content []byte
	if err := c.Call(ctx, "file.get", lease, map[string]any{"file_ref": p.KBDocument.FileRef}, &content); err != nil {
		return failJob(ctx, c, lease, err)
	}
	frags, skipped := kbjob.Cut(content)
	if frags == nil {
		frags = []kbjob.Fragment{}
	}
	if err := c.Call(ctx, "kb.index_put", lease, map[string]any{"corpus": p.KBDocument.Corpus, "file": p.KBDocument.ID, "fragments": frags}, nil); err != nil {
		return failJob(ctx, c, lease, err)
	}
	result := map[string]any{"fragments": len(frags)}
	if skipped != "" {
		result["skipped"] = skipped
	}
	return c.Call(ctx, "complete", lease, map[string]any{"result": result}, nil)
}

// httpChecks are the results of the checks file of a tenant as the external checker wrote them;
// none for a tenant without a web machine.
func (s *Stand) httpChecks(tenant string) map[string]any {
	checks := []any{}
	raw, err := os.ReadFile(checksPath(s.cfg.Work, s.farm.Tenant(tenant).First(KindWeb)))
	var all map[string]struct {
		LastStatus int `json:"last_status"`
		MsMax      int `json:"ms_max"`
	}
	if err == nil && json.Unmarshal(raw, &all) == nil {
		names := make([]string, 0, len(all))
		for n := range all {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			checks = append(checks, map[string]any{"check": n, "status": all[n].LastStatus, "ms": min(all[n].MsMax, 60000)})
		}
	}
	return map[string]any{"checks": checks}
}

// finishedRuns counts the oneshot runs of a tenant done since its last watch.
func (s *Stand) finishedRuns(tenant string) int {
	files, _ := filepath.Glob(filepath.Join(queueDir(s.cfg.Work, tenant), "done", "*.json"))
	s.mu.Lock()
	defer s.mu.Unlock()
	n := max(0, len(files)-s.watched[tenant])
	s.watched[tenant] = len(files)
	return min(n, 1000)
}
