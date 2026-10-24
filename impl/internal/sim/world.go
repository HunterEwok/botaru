// SPDX-License-Identifier: AGPL-3.0-only

// Package sim is the farm simulator of impl/SIM.md: a stand with the real core and workers whose
// tools work on a virtual world kept on disk under the work directory. The world holds the
// demo machines with their disks and services and the monitoring problems (the metric series
// stay in the stand's memory); the logs, the check results and the queue live beside it. The stubs of the operating system
// programs (systemctl, df, journalctl) read and change the world, so no tool call of a bot
// reaches the machine the simulator runs on.
package sim

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"syscall"
	"time"
)

// World is the state of the virtual machines of the farm, kept in <work>/world.json but for
// the metric series (series below).
type World struct {
	// Hosts are the machines by catalog id.
	Hosts map[string]*Host `json:"hosts"`
	// Problems are the problems of the monitoring system, newest last.
	Problems []*Problem `json:"problems"`
	// Series are the metric samples by "<host>|<item key>", oldest first, at most maxSamples.
	// They stay in the memory of the process that samples them (series), not in world.json.
	Series map[string][]Sample `json:"-"`
	// Faults are the faults in play by name, or by name:host for one machine (the scenario
	// faults of the director).
	Faults map[string]bool `json:"faults"`
	// Home is the tenant of the scenarios.
	Home string `json:"home"`
	// NextEvent numbers the monitoring events.
	NextEvent int64 `json:"next_event"`
	// events are the world events of one update, written to world-events.jsonl.
	events []Event
}

// Host is one virtual machine of one tenant.
type Host struct {
	// Tenant is the tenant the machine belongs to, Kind its kind (web, db, ctd, mon, geo).
	Tenant string `json:"tenant"`
	Kind   string `json:"kind"`
	// Node is its name in the monitoring system and ZabbixID its host id there.
	Node     string              `json:"node"`
	ZabbixID string              `json:"zabbix_id"`
	Disks    map[string]*Disk    `json:"disks"`
	Services map[string]*Service `json:"services"`
}

// Disk is one volume: its size, its free share and the bytes of the pipeline temporary files
// a cleanup can free.
type Disk struct {
	SizeBytes int64   `json:"size_bytes"`
	FreeRatio float64 `json:"free_space_ratio"`
	TmpBytes  int64   `json:"tmp_bytes"`
}

// Fill brings the disk down to the given free share with pipeline temporary files; a disk
// already fuller stays as it is.
func (d *Disk) Fill(free float64) {
	if d.FreeRatio <= free {
		return
	}
	d.TmpBytes += int64((d.FreeRatio - free) * float64(d.SizeBytes))
	d.FreeRatio = free
}

// Service is one unit: active, inactive or failed, with its restarts and the time it last
// changed.
type Service struct {
	Unit     string    `json:"unit"`
	State    string    `json:"state"`
	Restarts int       `json:"restarts"`
	Changed  time.Time `json:"changed"`
	// Broken makes start and restart fail until the director clears it.
	Broken bool `json:"broken,omitempty"`
}

// Problem is a problem of the monitoring system on a node.
type Problem struct {
	EventID   string         `json:"eventid"`
	TriggerID string         `json:"triggerid"`
	Host      string         `json:"host"`
	Severity  int            `json:"severity"`
	Clock     int64          `json:"clock"`
	Open      bool           `json:"open"`
	Values    map[string]any `json:"values,omitempty"`
	// Reason names the world condition the problem stands for (service:<unit>, disk:<mount>).
	Reason string `json:"reason"`
	// Name is the problem's name as a monitoring user wrote the trigger, and Comments the
	// messages of its acknowledgements: text the farm does not trust (kb-and-injection).
	Name     string   `json:"name,omitempty"`
	Comments []string `json:"comments,omitempty"`
}

// Sample is one value of a series.
type Sample struct {
	Clock int64   `json:"clock"`
	Value float64 `json:"value"`
}

// Event is one change of the world, printed as [WORLD].
type Event struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"`
	Host   string    `json:"host,omitempty"`
	Detail string    `json:"detail"`
}

// maxSamples bounds a series: 2880 steps (4 hours at the default step of 5 seconds).
const maxSamples = 2880

// Units of the machines.
const (
	UnitNginx    = "nginx.service"
	UnitAppAPI   = "app-api.service"
	UnitPostgres = "postgresql.service"
	UnitCTDQC    = "ctd-qc-runner-demo.service"
)

// UnitCTDQCOf is the unit of the CTD quality control runner of a tenant.
func UnitCTDQCOf(tenant string) string { return "ctd-qc-runner-" + tenant + ".service" }

// NewWorld is the healthy world of the smoke farm: tenant demo with web1, db1 and ctd1.
func NewWorld(now time.Time) *World {
	f, err := LoadFarm("smoke")
	if err != nil {
		panic(err)
	}
	return NewFarmWorld(f, now)
}

// NewFarmWorld is the healthy world of a farm: a web machine runs nginx and app-api, a db
// machine postgresql and holds the data volume of the pipeline, a ctd machine runs the CTD
// quality control runner of its tenant, and mon and geo machines have a root disk alone. The
// monitoring host ids follow the order of the profile from 10501.
func NewFarmWorld(f *Farm, now time.Time) *World {
	svc := func(unit string) *Service { return &Service{Unit: unit, State: "active", Changed: now} }
	w := &World{Hosts: map[string]*Host{}, Series: map[string][]Sample{}, Faults: map[string]bool{}, Home: f.Home().Name, NextEvent: 70001}
	n := 0
	for _, t := range f.Tenants {
		for _, fh := range t.Hosts {
			h := &Host{Tenant: t.Name, Kind: fh.Kind, Node: fh.ID + ".example.org", ZabbixID: fmt.Sprint(10501 + n), Services: map[string]*Service{}}
			n++
			switch fh.Kind {
			case KindWeb:
				h.Disks = map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.82}}
				h.Services[UnitNginx], h.Services[UnitAppAPI] = svc(UnitNginx), svc(UnitAppAPI)
			case KindDB:
				h.Disks = map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.71}, "/data": {SizeBytes: 2e12, FreeRatio: 0.82, TmpBytes: 30e9}}
				h.Services[UnitPostgres] = svc(UnitPostgres)
			case KindCTD:
				h.Disks = map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.64}, "/data": {SizeBytes: 4e12, FreeRatio: 0.55}}
				u := UnitCTDQCOf(t.Name)
				h.Services[u] = svc(u)
			default:
				h.Disks = map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.77}}
			}
			w.Hosts[fh.ID] = h
		}
	}
	return w
}

// First is a tenant's first machine of a kind in profile order, "" when it has none. The order
// is that of the monitoring host ids.
func (w *World) First(tenant, kind string) string {
	best, bestID := "", 0
	for id, h := range w.Hosts {
		if h.Tenant != tenant || h.Kind != kind {
			continue
		}
		var n int
		fmt.Sscan(h.ZabbixID, &n)
		if best == "" || n < bestID {
			best, bestID = id, n
		}
	}
	return best
}

// TenantNames are the tenants of the machines in name order.
func (w *World) TenantNames() []string {
	var out []string
	for _, h := range w.Hosts {
		if !slices.Contains(out, h.Tenant) {
			out = append(out, h.Tenant)
		}
	}
	slices.Sort(out)
	return out
}

// HostOfID is the machine of a monitoring host id, "" when none.
func (w *World) HostOfID(id string) string {
	for h, x := range w.Hosts {
		if x.ZabbixID == id {
			return h
		}
	}
	return ""
}

// Fault says whether a fault is in play on a machine: name:host names the machine, and the
// plain name, as the scenarios set it, stands for the home tenant's first machine of the kind.
func (w *World) Fault(name, host string) bool {
	if w.Faults[name+":"+host] {
		return true
	}
	h := w.Hosts[host]
	return w.Faults[name] && h != nil && h.Tenant == w.Home && w.First(w.Home, h.Kind) == host
}

// Note records a world event of this update.
func (w *World) Note(kind, host, format string, args ...any) {
	w.events = append(w.events, Event{Time: time.Now().UTC(), Kind: kind, Host: host, Detail: fmt.Sprintf(format, args...)})
}

// Service finds a unit on a host; nil when the host has no such unit.
func (w *World) Service(host, unit string) *Service {
	h := w.Hosts[host]
	if h == nil {
		return nil
	}
	return h.Services[unit]
}

// HostOfNode is the catalog host of a monitoring node, "" when none.
func (w *World) HostOfNode(node string) string {
	for id, h := range w.Hosts {
		if h.Node == node {
			return id
		}
	}
	return ""
}

// HostNames are the hosts in name order.
func (w *World) HostNames() []string {
	var out []string
	for id := range w.Hosts {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// Sample adds a value to a series and drops the oldest past maxSamples.
func (w *World) Sample(key string, at time.Time, v float64) {
	s := append(w.Series[key], Sample{Clock: at.Unix(), Value: v})
	if len(s) > maxSamples {
		s = s[len(s)-maxSamples:]
	}
	w.Series[key] = s
}

// Ramp moves the samples of a series since a moment in a straight line from their value to
// to, the newest reaching it, so the history shows a change that came over that time.
func (w *World) Ramp(key string, since, now time.Time, to float64) {
	span := now.Sub(since).Seconds()
	if span <= 0 {
		return
	}
	// A copy: readers share the stored slice and rely on its samples never changing.
	s := slices.Clone(w.Series[key])
	for i, x := range s {
		if x.Clock < since.Unix() {
			continue
		}
		f := min(1, float64(x.Clock-since.Unix())/span)
		s[i].Value = x.Value + (to-x.Value)*f
	}
	w.Series[key] = s
	w.Sample(key, now, to)
}

// OpenProblem opens a problem of a trigger on a host for a reason, once: an open problem of the
// same reason stays the one problem. It returns the problem and whether it is new.
func (w *World) OpenProblem(host, trigger, reason string, severity int, at time.Time, values map[string]any) (*Problem, bool) {
	for _, p := range w.Problems {
		if p.Open && p.Reason == reason {
			return p, false
		}
	}
	p := &Problem{EventID: fmt.Sprint(w.NextEvent), TriggerID: trigger, Host: host, Severity: severity, Clock: at.Unix(), Open: true, Values: values, Reason: reason}
	w.NextEvent++
	w.Problems = append(w.Problems, p)
	return p, true
}

// CloseProblem closes the open problem of a reason and returns it, nil when none is open.
func (w *World) CloseProblem(reason string) *Problem {
	for _, p := range w.Problems {
		if p.Open && p.Reason == reason {
			p.Open = false
			return p
		}
	}
	return nil
}

// Paths of the work directory.
const (
	worldFile  = "world.json"
	lockFile   = "world.lock"
	eventsFile = "world-events.jsonl"
)

// LoadWorld reads the world of a work directory without the lock; for a reader that changes
// nothing.
func LoadWorld(dir string) (*World, error) {
	raw, err := os.ReadFile(filepath.Join(dir, worldFile))
	if err != nil {
		return nil, err
	}
	var w World
	if err := json.Unmarshal(raw, &w); err != nil {
		return nil, fmt.Errorf("%s: %w", worldFile, err)
	}
	w.Series = loadSeries(dir)
	if w.Faults == nil {
		w.Faults = map[string]bool{}
	}
	return &w, nil
}

// SaveWorld writes a new world into an empty work directory.
func SaveWorld(dir string, w *World) error {
	if _, err := os.Stat(filepath.Join(dir, worldFile)); err == nil {
		return errors.New("the work directory already holds a world")
	}
	return Update(dir, func(old *World) error {
		*old = *w
		return nil
	})
}

// Update changes the world under the lock of the work directory: the stubs run as other
// processes, so every change reads, changes and replaces world.json while it holds the lock,
// and the file is replaced by a rename, so a reader never sees half of it. The events of the
// change are appended to world-events.jsonl.
func Update(dir string, fn func(*World) error) error {
	lf, err := os.OpenFile(filepath.Join(dir, lockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() { _ = syscall.Flock(int(lf.Fd()), syscall.LOCK_UN) }()
	w, err := LoadWorld(dir)
	if errors.Is(err, fs.ErrNotExist) {
		w = &World{Hosts: map[string]*Host{}, Series: map[string][]Sample{}, Faults: map[string]bool{}}
	} else if err != nil {
		return err
	}
	if err := fn(w); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(w, "", " ")
	if err != nil {
		return err
	}
	if err := writeAtomic(filepath.Join(dir, worldFile), raw); err != nil {
		return err
	}
	storeSeries(dir, w.Series)
	if len(w.events) == 0 {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, eventsFile), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(f)
	for _, e := range w.events {
		if err := enc.Encode(e); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// writeAtomic replaces a file by a rename of a temporary file beside it.
func writeAtomic(path string, raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

// series keeps the metric series of each work directory in the memory of the process: the
// stand's physics samples them and its monitor reads them, both in the stand's process, while
// the stubs, other processes, never need them. Kept in world.json, every step and every stub
// call read and wrote the whole history, and a long run's memory grew with it.
var series = struct {
	mu sync.Mutex
	by map[string]map[string][]Sample
}{by: map[string]map[string][]Sample{}}

// loadSeries returns a map of its own over the series of a work directory. The slices share
// their arrays with the stored ones; the one writer, Sample inside Update (serialized by the
// lock of the work directory, between goroutines too), only appends past every reader's end,
// so no reader sees a value change.
func loadSeries(dir string) map[string][]Sample {
	series.mu.Lock()
	defer series.mu.Unlock()
	out := make(map[string][]Sample, len(series.by[dir]))
	for k, s := range series.by[dir] {
		out[k] = s
	}
	return out
}

// storeSeries keeps the series of a work directory after a change of the world.
func storeSeries(dir string, s map[string][]Sample) {
	series.mu.Lock()
	defer series.mu.Unlock()
	series.by[dir] = s
}

// dropSeries forgets the series of a work directory whose stand ends.
func dropSeries(dir string) {
	series.mu.Lock()
	defer series.mu.Unlock()
	delete(series.by, dir)
}
