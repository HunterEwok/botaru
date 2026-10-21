// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
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
	// FaultDiskFilling makes the pipeline temporary files on the data volume of the db machine
	// grow every step.
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

// Paths of the files the plugins read, under the work directory: the logs of a machine, the
// check results of a tenant's first web machine, the oneshot queue and the data of a tenant.
func logPath(work, host, file string) string { return filepath.Join(work, "logs", host, file) }
func checksPath(work, host string) string    { return filepath.Join(work, "checks", host, "checks.json") }
func queueDir(work, tenant string) string    { return filepath.Join(work, "queue", tenant) }

// InitFiles writes the files of the smoke farm (NewWorld).
func InitFiles(work string, seed int64, now time.Time) error {
	f, err := LoadFarm("smoke")
	if err != nil {
		return err
	}
	return InitFarmFiles(work, f, seed, now)
}

// InitFarmFiles writes the world of a farm and its first files: the logs from the eval
// generators (seeded, so a run is repeatable; the home tenant's first web machine takes the seed
// itself, every other one a seed of its own), the data of each tenant and the first check
// results.
func InitFarmFiles(work string, f *Farm, seed int64, now time.Time) error {
	w := NewFarmWorld(f, now)
	webSeed := seed
	for _, h := range w.HostNames() {
		dirs := []string{filepath.Join(work, "logs", h)}
		switch w.Hosts[h].Kind {
		case KindDB:
			dirs = append(dirs, filepath.Join(work, "tmp", h))
		case KindWeb:
			dirs = append(dirs, filepath.Join(work, "checks", h))
		}
		for _, d := range dirs {
			if err := os.MkdirAll(d, 0o755); err != nil {
				return err
			}
		}
	}
	for _, t := range f.Tenants {
		if err := os.MkdirAll(queueDir(work, t.Name), 0o755); err != nil {
			return err
		}
		for i, h := range t.Hosts {
			switch h.Kind {
			case KindWeb:
				s := webSeed
				if t != f.Home() || h.ID != t.First(KindWeb) {
					s = seed + int64(1000*(slices.Index(f.Tenants, t)+1)+i)
				}
				if _, err := evals.GenerateLogs(filepath.Join(work, "logs", h.ID), "nginx", s, now, nil); err != nil {
					return err
				}
				if _, err := evals.GenerateLogs(filepath.Join(work, "logs", h.ID), "app", s, now, nil); err != nil {
					return err
				}
			case KindDB:
				if err := os.WriteFile(logPath(work, h.ID, "postgresql.log"), []byte(now.Add(-time.Hour).UTC().Format("2006-01-02 15:04:05.000 UTC")+" [812] LOG:  checkpoint complete\n"), 0o644); err != nil {
					return err
				}
			}
		}
	}
	// An hour of history, so the first item_history has points.
	for t := now.Add(-time.Hour); t.Before(now); t = t.Add(time.Minute) {
		sample(w, t, rand.New(rand.NewPCG(uint64(seed), uint64(t.Unix()))))
	}
	if err := SaveWorld(work, w); err != nil {
		return err
	}
	for i, t := range f.Tenants {
		s := seed
		if i > 0 {
			s = seed + int64(i)
		}
		if err := initData(work, t.Name, s, now); err != nil {
			return err
		}
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
		hosts := w.HostNames()
		for _, h := range hosts {
			if d := w.Hosts[h].Disks["/data"]; d != nil && w.Hosts[h].Kind == KindDB && w.Fault(FaultDiskFilling, h) && d.FreeRatio > 0.03 {
				grow := int64(float64(d.SizeBytes) * 0.004)
				d.TmpBytes += grow
				d.FreeRatio = max(0.03, d.FreeRatio-0.004)
			}
		}
		sample(w, now, r)
		// The problems of the catalog triggers: nginx down on a web machine, and little free
		// space on the data volume of a db machine.
		for _, h := range hosts {
			s := w.Service(h, UnitNginx)
			if w.Hosts[h].Kind != KindWeb || s == nil {
				continue
			}
			reason := "service:" + h + ":" + UnitNginx
			if s.State != "active" {
				if p, ok := w.OpenProblem(h, TriggerServiceDown, reason, 4, now, nil); ok {
					changes = append(changes, Change{*p, true})
					w.Note("monitor", h, "problem %s opened: trigger %s, %s is %s", p.EventID, p.TriggerID, UnitNginx, s.State)
				}
			} else if p := w.CloseProblem(reason); p != nil {
				changes = append(changes, Change{*p, false})
				w.Note("monitor", h, "problem %s resolved: %s is active", p.EventID, UnitNginx)
			}
		}
		for _, h := range hosts {
			d := w.Hosts[h].Disks["/data"]
			if w.Hosts[h].Kind != KindDB || d == nil {
				continue
			}
			reason := "disk:" + h + ":/data"
			if d.FreeRatio < diskLow {
				if p, ok := w.OpenProblem(h, TriggerDiskFull, reason, 4, now, map[string]any{"free_pct": round1(100 * d.FreeRatio)}); ok {
					changes = append(changes, Change{*p, true})
					w.Note("monitor", h, "problem %s opened: trigger %s, /data free %.1f%%", p.EventID, p.TriggerID, 100*d.FreeRatio)
				}
			} else if d.FreeRatio >= diskOK {
				if p := w.CloseProblem(reason); p != nil {
					changes = append(changes, Change{*p, false})
					w.Note("monitor", h, "problem %s resolved: /data free %.1f%%", p.EventID, 100*d.FreeRatio)
				}
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
	for _, path := range slices.Sorted(maps.Keys(lines)) {
		if err := appendLines(filepath.Join(work, path), lines[path]); err != nil {
			return nil, err
		}
	}
	return changes, writeChecks(work, w2, r)
}

// dataDisk is the data volume of a tenant's first db machine, which the pipeline fills and the
// web machines of the tenant write to; nil when the tenant has none.
func (w *World) dataDisk(tenant string) *Disk {
	if h := w.Hosts[w.First(tenant, KindDB)]; h != nil {
		return h.Disks["/data"]
	}
	return nil
}

func round1(v float64) float64 { return float64(int(v*10+0.5)) / 10 }

// Fragments of generated lines; the addresses are documentation ranges.
var (
	simIPs   = []string{"192.0.2.", "198.51.100.", "203.0.113."}
	simPaths = []string{"/", "/index.html", "/news", "/about", "/static/app.css", "/api/v1/status", "/docs/start"}
	slowURLs = []string{"/api/v1/report", "/api/v1/export", "/search"}
)

// logLines are the lines of this moment by log file: what nginx and app-api write on each web
// machine and postgresql on each db machine in the state the world is in.
func logLines(w *World, now time.Time, r *rand.Rand) map[string][]string {
	out := map[string][]string{}
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
	for _, h := range w.HostNames() {
		if w.Hosts[h].Kind != KindWeb {
			continue
		}
		accessLog, errorLog, appLog := "logs/"+h+"/access.log", "logs/"+h+"/error.log", "logs/"+h+"/app-api.log"
		nginx, app := w.Service(h, UnitNginx), w.Service(h, UnitAppAPI)
		data := w.dataDisk(w.Hosts[h].Tenant)
		appUp := app != nil && app.State == "active"
		diskFull := data != nil && data.FreeRatio < 0.05
		slow := w.Fault(FaultSlowSite, h)
		if nginx != nil && nginx.State == "active" {
			path := simPaths[r.IntN(len(simPaths))]
			switch {
			case !appUp:
				out[accessLog] = append(out[accessLog], access("/api/v1/orders", 502, 60))
				out[errorLog] = append(out[errorLog], nginxErr(`connect() failed (111: Connection refused) while connecting to upstream, client: `+ip()+`, server: www.example.org, request: "GET /api/v1/orders HTTP/1.1", upstream: "http://app.example.org:8081/"`))
			case diskFull:
				out[accessLog] = append(out[accessLog], access("/api/v1/export", 504, 60))
				out[errorLog] = append(out[errorLog], nginxErr(`upstream timed out (110: Connection timed out) while reading response header from upstream, client: `+ip()+`, server: www.example.org, request: "GET /api/v1/export HTTP/1.1", upstream: "http://app.example.org:8081/"`))
			case slow:
				u := slowURLs[r.IntN(len(slowURLs))]
				out[accessLog] = append(out[accessLog], access(u, 200, 2.5+r.Float64()*6))
			default:
				out[accessLog] = append(out[accessLog], access(path, 200, 0.01+r.Float64()*0.5))
			}
		}
		switch {
		case !appUp:
		case diskFull:
			out[appLog] = append(out[appLog], appLine("ERROR", "db write failed: could not extend file: No space left on device rid="+rid()))
		case slow:
			out[appLog] = append(out[appLog], appLine("WARN", fmt.Sprintf("slow request %s %dms rid=%s", slowURLs[r.IntN(len(slowURLs))], 2500+r.IntN(6000), rid())))
		default:
			out[appLog] = append(out[appLog], appLine("INFO", fmt.Sprintf("%s 200 %dms rid=%s", simPaths[r.IntN(len(simPaths))], 3+r.IntN(80), rid())))
		}
	}
	for _, h := range w.HostNames() {
		d := w.Hosts[h].Disks["/data"]
		if pg := w.Service(h, UnitPostgres); pg != nil && pg.State == "active" && d != nil && d.FreeRatio < 0.05 {
			out["logs/"+h+"/postgresql.log"] = append(out["logs/"+h+"/postgresql.log"],
				now.UTC().Format("2006-01-02 15:04:05.000 UTC")+fmt.Sprintf(" [%d] ERROR:  could not extend file \"base/16384/2619\": No space left on device", 800+r.IntN(90)))
		}
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

// writeChecks writes the results of the HTTP checks of each tenant's first web machine as the
// external checker would.
func writeChecks(work string, w *World, r *rand.Rand) error {
	for _, t := range w.TenantNames() {
		h := w.First(t, KindWeb)
		if h == "" {
			continue
		}
		healthy := map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 120 + r.IntN(80)}
		www, api := healthy, map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 60 + r.IntN(40)}
		nginx, app := w.Service(h, UnitNginx), w.Service(h, UnitAppAPI)
		data := w.dataDisk(t)
		switch {
		case nginx == nil || nginx.State != "active":
			www = map[string]any{"ok": 0, "total": 3, "last_status": 0, "ms_max": 10000}
			api = map[string]any{"ok": 0, "total": 3, "last_status": 0, "ms_max": 10000}
		case app == nil || app.State != "active":
			www = map[string]any{"ok": 1, "total": 3, "last_status": 502, "ms_max": 300}
			api = map[string]any{"ok": 0, "total": 3, "last_status": 502, "ms_max": 300}
		case data != nil && data.FreeRatio < 0.05:
			api = map[string]any{"ok": 1, "total": 3, "last_status": 504, "ms_max": 60000}
		case w.Fault(FaultSlowSite, h):
			www = map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 6200 + r.IntN(1500)}
			api = map[string]any{"ok": 2, "total": 3, "last_status": 200, "ms_max": 8400 + r.IntN(1500)}
		}
		raw, err := json.Marshal(map[string]any{"www-health": www, "api-health": api,
			"data-portal": map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 210}, "docs-site": map[string]any{"ok": 3, "total": 3, "last_status": 200, "ms_max": 95}})
		if err != nil {
			return err
		}
		if err := writeAtomic(checksPath(work, h), raw); err != nil {
			return err
		}
	}
	return nil
}
