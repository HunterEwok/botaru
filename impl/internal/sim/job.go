// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

// startJob enrolls and runs the job worker of the role data-job: the code of the schedule
// tasks, which reads the world and uses no model (the role's kb.ingest.v1 is not run here).
func (s *Stand) startJob(ctx, run context.Context) error {
	tok, err := s.Bench.Enroll(ctx, map[string]any{"tenant": "demo", "role": "data-job", "kind": "job"})
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
			if err := s.jobOnce(run, c); err != nil && run.Err() == nil {
				s.log.Addf(KindDirector, "the data-job worker: %v", err)
				select {
				case <-run.Done():
				case <-time.After(2 * time.Second):
				}
			}
		}
	}()
	return nil
}

// jobOnce claims one attempt and completes it with the numbers of the world.
func (s *Stand) jobOnce(ctx context.Context, c *p1client.Client) error {
	w, err := c.Claim(ctx, []string{"attempt"}, 20)
	if err != nil || w == nil {
		return err
	}
	lease := w.Lease()
	var p struct {
		Task struct {
			Type string `json:"type"`
		} `json:"task"`
	}
	_ = json.Unmarshal(w.Payload, &p)
	var result map[string]any
	switch p.Task.Type {
	case "check.http.v1":
		result = s.httpChecks()
	case "pipeline.watch.v1":
		result = map[string]any{"finished": s.finishedRuns()}
	case "data.sensor_watch.v1":
		result = map[string]any{"silent": 0}
	default:
		return c.Call(ctx, "fail", lease, map[string]any{"reason": "runtime_error", "retryable": false}, nil)
	}
	return c.Call(ctx, "complete", lease, map[string]any{"result": result}, nil)
}

// httpChecks are the results of the checks file as the external checker wrote them.
func (s *Stand) httpChecks() map[string]any {
	checks := []any{}
	raw, err := os.ReadFile(checksPath(s.cfg.Work))
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

// finishedRuns counts the oneshot runs done since the last watch.
func (s *Stand) finishedRuns() int {
	files, _ := filepath.Glob(filepath.Join(s.cfg.Work, "queue", "demo", "done", "*.json"))
	s.mu.Lock()
	defer s.mu.Unlock()
	n := max(0, len(files)-s.watched)
	s.watched = len(files)
	return min(n, 1000)
}
