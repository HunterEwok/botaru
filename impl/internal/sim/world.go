// SPDX-License-Identifier: AGPL-3.0-only

// Package sim is the farm simulator of impl/SIM.md: a stand with the real core and workers whose
// tools work on a virtual world kept on disk under the work directory. The world holds the
// demo machines with their disks and services, the monitoring problems and the metric series;
// the logs, the check results and the queue live beside it. The stubs of the operating system
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
	"syscall"
	"time"
)

// World is the state of the virtual machines of tenant demo, kept in <work>/world.json.
type World struct {
	// Hosts are the machines by catalog id.
	Hosts map[string]*Host `json:"hosts"`
	// Problems are the problems of the monitoring system, newest last.
	Problems []*Problem `json:"problems"`
	// Series are the metric samples by "<host>/<field>", oldest first, at most maxSamples.
	Series map[string][]Sample `json:"series"`
	// Faults are the faults in play by name (the scenario faults of the director).
	Faults map[string]bool `json:"faults"`
	// NextEvent numbers the monitoring events.
	NextEvent int64 `json:"next_event"`
	// events are the world events of one update, written to world-events.jsonl.
	events []Event
}

// Host is one virtual machine.
type Host struct {
	// Node is its name in the monitoring system.
	Node     string              `json:"node"`
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

// maxSamples bounds a series: a day of samples every 30 seconds.
const maxSamples = 2880

// Units of the demo machines.
const (
	UnitNginx    = "nginx.service"
	UnitAppAPI   = "app-api.service"
	UnitPostgres = "postgresql.service"
	UnitCTDQC    = "ctd-qc-runner-demo.service"
)

// NewWorld is the healthy world of tenant demo: web1 with nginx and app-api, db1 with
// postgresql and the data volume of the pipeline, ctd1 with the CTD quality control runner.
func NewWorld(now time.Time) *World {
	svc := func(unit string) *Service { return &Service{Unit: unit, State: "active", Changed: now} }
	return &World{
		Hosts: map[string]*Host{
			"web1": {Node: "web1.example.org", Disks: map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.82}},
				Services: map[string]*Service{UnitNginx: svc(UnitNginx), UnitAppAPI: svc(UnitAppAPI)}},
			"db1": {Node: "db1.example.org", Disks: map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.71}, "/data": {SizeBytes: 2e12, FreeRatio: 0.82, TmpBytes: 30e9}},
				Services: map[string]*Service{UnitPostgres: svc(UnitPostgres)}},
			"ctd1": {Node: "ctd1.example.org", Disks: map[string]*Disk{"/": {SizeBytes: 40e9, FreeRatio: 0.64}, "/data": {SizeBytes: 4e12, FreeRatio: 0.55}},
				Services: map[string]*Service{UnitCTDQC: svc(UnitCTDQC)}},
		},
		Series:    map[string][]Sample{},
		Faults:    map[string]bool{},
		NextEvent: 70001,
	}
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
	if w.Series == nil {
		w.Series = map[string][]Sample{}
	}
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
