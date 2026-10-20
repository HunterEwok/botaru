// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/evals"
)

// The physics of the world: each step samples the metric series, opens and closes the problems
// of the monitoring system, writes the check results and appends the log lines of the moment,
// all from the state of the machines and the faults in play. A bot that restarted a service or
// freed a disk sees the effect in the logs, checks and metrics of the next steps.

// Fault names the director sets in the world.
const (
	// FaultSlowSite makes app-api answer slowly: slow lines in the access and app logs.
	FaultSlowSite = "slow-site"
	// FaultDiskFilling makes the pipeline temporary files on db1:/data grow every step.
	FaultDiskFilling = "disk-filling"
)

// Catalog triggers of the monitoring system (monitor_triggers of examples/tenants/demo/catalog.yaml).
const (
	TriggerServiceDown = "4407"
	TriggerDiskFull    = "4401"
)

// diskLow and diskOK are the free shares at which the disk problem opens and closes.
const (
	diskLow = 0.15
	diskOK  = 0.20
)

// Item keys the monitoring system serves.
func diskItem(mount, kind string) string { return "vfs.fs.size[" + mount + "," + kind + "]" }

const (
	itemHTTP = "net.tcp.service[http]"
	itemCPU  = "system.cpu.load[all,avg1]"
	itemMem  = "vm.memory.utilization"
	// The traffic of the interface eth0, in bytes per second.
	itemNetIn  = "net.if.in[eth0]"
	itemNetOut = "net.if.out[eth0]"
)

// seriesKey is the key of a series: the host and the item key.
func seriesKey(host, item string) string { return host + "|" + item }

// Change is a problem the step opened or closed.
type Change struct {
	Problem Problem
	Opened  bool
}

// Paths of the files the plugins read, under the work directory.
func logPath(work, host, file string) string { return filepath.Join(work, "logs", host, file) }
func checksPath(work string) string          { return filepath.Join(work, "checks", "web1", "checks.json") }

// InitFiles writes the first logs from the eval generators (seeded, so a run is repeatable) and
// the first check results.
func InitFiles(work string, seed int64, now time.Time) error {
	for _, d := range []string{filepath.Join(work, "logs", "web1"), filepath.Join(work, "logs", "db1"), filepath.Join(work, "checks", "web1"),
		filepath.Join(work, "queue", "demo"), filepath.Join(work, "tmp", "db1")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if _, err := evals.GenerateLogs(filepath.Join(work, "logs", "web1"), "nginx", seed, now, nil); err != nil {
		return err
	}
	if _, err := evals.GenerateLogs(filepath.Join(work, "logs", "web1"), "app", seed, now, nil); err != nil {
		return err
	}
	if err := os.WriteFile(logPath(work, "db1", "postgresql.log"), []byte(now.Add(-time.Hour).UTC().Format("2006-01-02 15:04:05.000 UTC")+" [812] LOG:  checkpoint complete\n"), 0o644); err != nil {
		return err
	}
	w := NewWorld(now)
	// An hour of history, so the first item_history has points.
	for t := now.Add(-time.Hour); t.Before(now); t = t.Add(time.Minute) {
		sample(w, t, rand.New(rand.NewPCG(uint64(seed), uint64(t.Unix()))))
	}
	if err := SaveWorld(work, w); err != nil {
		return err
	}
	if err := initData(work, seed, now); err != nil {
		return err
	}
	return writeChecks(work, w, rand.New(rand.NewPCG(uint64(seed), 1)))
}

// sample adds the values of the moment to the series of every host.
func sample(w *World, at time.Time, r *rand.Rand) {
	for _, id := range w.HostNames() {
		h := w.Hosts[id]
		for m, d := range h.Disks {
			w.Sample(seriesKey(id, diskItem(m, "pused")), at, 100*(1-d.FreeRatio))
			w.Sample(seriesKey(id, diskItem(m, "pfree")), at, 100*d.FreeRatio)
		}
		cpu := 0.3 + r.Float64()*0.4
		w.Sample(seriesKey(id, itemCPU), at, cpu)
		w.Sample(seriesKey(id, itemMem), at, 40+r.Float64()*10)
		// The traffic follows the load; it takes no draw of its own, so the logs of a seed stay
		// as they were.
		w.Sample(seriesKey(id, itemNetIn), at, 2e5+cpu*1e6)
		w.Sample(seriesKey(id, itemNetOut), at, 5e5+cpu*3e6)
		if s := h.Services[UnitNginx]; s != nil {
			up := 0.0
			if s.State == "active" {
				up = 1
			}
			w.Sample(seriesKey(id, itemHTTP), at, up)
		}
	}
}

// Step advances the world by one step at now and returns the problems it opened or closed.
func Step(work string, now time.Time, r *rand.Rand) ([]Change, error) {
	var changes []Change
	var lines map[string][]string
	var w2 *World
	err := Update(work, func(w *World) error {
		changes = nil
		if w.Faults[FaultDiskFilling] {
			d := w.Hosts["db1"].Disks["/data"]
			if d.FreeRatio > 0.03 {
				grow := int64(float64(d.SizeBytes) * 0.004)
				d.TmpBytes += grow
				d.FreeRatio = max(0.03, d.FreeRatio-0.004)
			}
		}
		sample(w, now, r)
		// The problems of the catalog triggers: nginx down, and little free space on db1:/data.
		if s := w.Service("web1", UnitNginx); s != nil {
			reason := "service:web1:" + UnitNginx
			if s.State != "active" {
				if p, ok := w.OpenProblem("web1", TriggerServiceDown, reason, 4, now, nil); ok {
					changes = append(changes, Change{*p, true})
					w.Note("monitor", "web1", "problem %s opened: trigger %s, %s is %s", p.EventID, p.TriggerID, UnitNginx, s.State)
				}
			} else if p := w.CloseProblem(reason); p != nil {
				changes = append(changes, Change{*p, false})
				w.Note("monitor", "web1", "problem %s resolved: %s is active", p.EventID, UnitNginx)
			}
		}
		d := w.Hosts["db1"].Disks["/data"]
		reason := "disk:db1:/data"
		if d.FreeRatio < diskLow {
			if p, ok := w.OpenProblem("db1", TriggerDiskFull, reason, 4, now, map[string]any{"free_pct": round1(100 * d.FreeRatio)}); ok {
				changes = append(changes, Change{*p, true})
				w.Note("monitor", "db1", "problem %s opened: trigger %s, /data free %.1f%%", p.EventID, p.TriggerID, 100*d.FreeRatio)
			}
		} else if d.FreeRatio >= diskOK {
			if p := w.CloseProblem(reason); p != nil {
				changes = append(changes, Change{*p, false})
				w.Note("monitor", "db1", "problem %s resolved: /data free %.1f%%", p.EventID, 100*d.FreeRatio)
			}
		}
		lines = logLines(w, now, r)
		c := *w
		w2 = &c
		return nil
	})
	if err != nil {
		return nil, err
	}
	for path, ls := range lines {
		if err := appendLines(filepath.Join(work, path), ls); err != nil {
			return nil, err
		}
	}
	return changes, writeChecks(work, w2, r)
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// Fragments of generated lines; the addresses are documentation ranges.
var (
	simIPs   = []string{"192.0.2.", "198.51.100.", "203.0.113."}
	simPaths = []string{"/", "/index.html", "/news", "/about", "/static/app.css", "/api/v1/status", "/docs/start"}
	slowURLs = []string{"/api/v1/report", "/api/v1/export", "/search"}
)

// logLines are the lines of this moment by log file: what nginx, app-api and postgresql write in
// the state the world is in.
func logLines(w *World, now time.Time, r *rand.Rand) map[string][]string {
	out := map[string][]string{}
	nginx, app := w.Service("web1", UnitNginx), w.Service("web1", UnitAppAPI)
	data := w.Hosts["db1"].Disks["/data"]
	ip := func() string { return simIPs[r.IntN(len(simIPs))] + fmt.Sprint(2+r.IntN(250)) }
	rid := func() string { return fmt.Sprintf("r%08x", r.Uint32()) }
	access := func(path string, status int, rt float64) string {
		return fmt.Sprintf(`%s - - [%s] "GET %s HTTP/1.1" %d %d "-" "Mozilla/5.0 (X11; Linux x86_64; rv:128.0) Gecko/20100101 Firefox/128.0" rt=%.3f rid=%s`,
			ip(), now.UTC().Format("02/Jan/2006:15:04:05 -0700"), path, status, 500+r.IntN(20000), rt, rid())
	}
	nginxErr := func(msg string) string {
		return fmt.Sprintf("%s [error] %d#0: *%d %s", now.UTC().Format("2006/01/02 15:04:05"), 1200+r.IntN(50), 20000+r.IntN(9000), msg)
	}
	appLine := func(level, msg string) string {
		return fmt.Sprintf("%s app-api[%d]: %s %s", now.UTC().Format(time.RFC3339), 4100+r.IntN(20), level, msg)
	}
	appUp := app != nil && app.State == "active"
	diskFull := data.FreeRatio < 0.05
	if nginx != nil && nginx.State == "active" {
		path := simPaths[r.IntN(len(simPaths))]
		switch {
		case !appUp:
			out["logs/web1/access.log"] = append(out["logs/web1/access.log"], access("/api/v1/orders", 502, 60))
			out["logs/web1/error.log"] = append(out["logs/web1/error.log"], nginxErr(`connect() failed (111: Connection refused) while connecting to upstream, client: `+ip()+`, server: www.example.org, request: "GET /api/v1/orders HTTP/1.1", upstream: "http://app.example.org:8081/"`))
		case diskFull:
			out["logs/web1/access.log"] = append(out["logs/web1/access.log"], access("/api/v1/export", 504, 60))
			out["logs/web1/error.log"] = append(out["logs/web1/error.log"], nginxErr(`upstream timed out (110: Connection timed out) while reading response header from upstream, client: `+ip()+`, server: www.example.org, request: "GET /api/v1/export HTTP/1.1", upstream: "http://app.example.org:8081/"`))
		case w.Faults[FaultSlowSite]:
			u := slowURLs[r.IntN(len(slowURLs))]
			out["logs/web1/access.log"] = append(out["logs/web1/access.log"], access(u, 200, 2.5+r.Float64()*6))
		default:
			out["logs/web1/access.log"] = append(out["logs/web1/access.log"], access(path, 200, 0.01+r.Float64()*0.5))
		}
	}
	switch {
	case !appUp:
	case diskFull:
		out["logs/web1/app-api.log"] = append(out["logs/web1/app-api.log"], appLine("ERROR", "db write failed: could not extend file: No space left on device rid="+rid()))
	case w.Faults[FaultSlowSite]:
		out["logs/web1/app-api.log"] = append(out["logs/web1/app-api.log"], appLine("WARN", fmt.Sprintf("slow request %s %dms rid=%s", slowURLs[r.IntN(len(slowURLs))], 2500+r.IntN(6000), rid())))
	default:
		out["logs/web1/app-api.log"] = append(out["logs/web1/app-api.log"], appLine("INFO", fmt.Sprintf("%s 200 %dms rid=%s", simPaths[r.IntN(len(simPaths))], 3+r.IntN(80), rid())))
	}
	if pg := w.Service("db1", UnitPostgres); pg != nil && pg.State == "active" && diskFull {
		out["logs/db1/postgresql.log"] = append(out["logs/db1/postgresql.log"],
			now.UTC().Format("2006-01-02 15:04:05.000 UTC")+fmt.Sprintf(" [%d] ERROR:  could not extend file \"base/16384/2619\": No space left on device", 800+r.IntN(90)))
	}
	return out
}

// appendLines appends lines to a log file.
func appendLines(path string, lines []string) error {
	if len(lines) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// AppendLog appends a line to a log of the world; the director plants lines with it.
func AppendLog(work, host, file, line string) error {
	return appendLines(logPath(work, host, file), []string{line})
}

// writeChecks writes the results of the HTTP checks of web1 as the external checker would.
func writeChecks(work string, w *World, r *rand.Rand) error {
	healthy := map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 120 + r.IntN(80)}
	www, api := healthy, map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 60 + r.IntN(40)}
	nginx, app := w.Service("web1", UnitNginx), w.Service("web1", UnitAppAPI)
	switch {
	case nginx == nil || nginx.State != "active":
		www = map[string]any{"ok": 0, "total": 3, "last_status": 0, "ms_max": 10000}
		api = map[string]any{"ok": 0, "total": 3, "last_status": 0, "ms_max": 10000}
	case app == nil || app.State != "active":
		www = map[string]any{"ok": 1, "total": 3, "last_status": 502, "ms_max": 300}
		api = map[string]any{"ok": 0, "total": 3, "last_status": 502, "ms_max": 300}
	case w.Hosts["db1"].Disks["/data"].FreeRatio < 0.05:
		api = map[string]any{"ok": 1, "total": 3, "last_status": 504, "ms_max": 60000}
	case w.Faults[FaultSlowSite]:
		www = map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 6200 + r.IntN(1500)}
		api = map[string]any{"ok": 2, "total": 3, "last_status": 200, "ms_max": 8400 + r.IntN(1500)}
	}
	raw, err := json.Marshal(map[string]any{"www-health": www, "api-health": api,
		"data-portal": map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 210}, "docs-site": map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 95}})
	if err != nil {
		return err
	}
	return writeAtomic(checksPath(work), raw)
}
