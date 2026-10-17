// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// The stubs stand in for the programs of the operating system that the own plugins run: the cmd
// plugin finds its programs under the stand's program root (-program-root) and the service
// plugin runs the stub systemctl (-systemctl). Each stub is a short script under
// <work>/root/<host> that runs this binary again with the stub's name, the work directory and
// the host in its environment (the cmd plugin clears the environment, so the script sets
// them); the binary then answers from the world and changes it, and never runs the real
// program.

// Stub environment variables.
const (
	StubEnv     = "BOTARU_SIM_STUB"
	StubWorkEnv = "BOTARU_SIM_WORK"
	StubHostEnv = "BOTARU_SIM_HOST"
)

// stubPrograms are the programs that have a stub, by the path the manifests name.
var stubPrograms = map[string]string{"systemctl": "/usr/bin/systemctl", "df": "/usr/bin/df", "journalctl": "/usr/bin/journalctl"}

// shq quotes a string for sh.
func shq(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// WriteStubs writes the stub scripts of a host and returns its program root: the stub of
// /usr/bin/df is <root>/usr/bin/df. exe is the program the scripts run (this binary), with the
// environment entries env before it.
func WriteStubs(work, host, exe string, env []string) (string, error) {
	root := filepath.Join(work, "root", host)
	for name, path := range stubPrograms {
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		vars := []string{StubEnv + "=" + shq(name), StubWorkEnv + "=" + shq(work), StubHostEnv + "=" + shq(host)}
		for _, e := range env {
			k, v, _ := strings.Cut(e, "=")
			vars = append(vars, k+"="+shq(v))
		}
		script := "#!/bin/sh\n" + strings.Join(vars, " ") + " exec " + shq(exe) + ` "$@"` + "\n"
		if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
			return "", err
		}
	}
	return root, nil
}

// StubFromEnv runs the stub the environment names, if any: it reports whether one ran and its
// exit status.
func StubFromEnv(args []string, stdout io.Writer) (bool, int) {
	name := os.Getenv(StubEnv)
	if name == "" {
		return false, 0
	}
	return true, RunStub(name, os.Getenv(StubWorkEnv), os.Getenv(StubHostEnv), args, stdout)
}

// RunStub answers one call of a stub program on a host of the world in work and returns its
// exit status.
func RunStub(name, work, host string, args []string, stdout io.Writer) int {
	if work == "" || host == "" {
		fmt.Fprintln(stdout, name+": the simulator stub has no world")
		return 1
	}
	switch name {
	case "systemctl":
		return systemctl(work, host, args, stdout)
	case "df":
		return df(work, host, stdout)
	case "journalctl":
		return journalctl(work, host, args, stdout)
	}
	fmt.Fprintln(stdout, name+": no such stub")
	return 1
}

// unitArg is the last argument after "--", else the last argument.
func unitArg(args []string) string {
	if i := slices.Index(args, "--"); i >= 0 && i+1 < len(args) {
		return args[len(args)-1]
	}
	if len(args) > 0 {
		return args[len(args)-1]
	}
	return ""
}

// verbArg is the first argument that is not an option or an option's value.
func verbArg(args []string) string {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			return ""
		}
		if a == "-n" {
			i++
			continue
		}
		if !strings.HasPrefix(a, "-") {
			return a
		}
	}
	return ""
}

// systemctl answers is-active, status, start, stop and restart of a unit from the world.
func systemctl(work, host string, args []string, out io.Writer) int {
	verb, unit := verbArg(args), unitArg(args)
	code := 0
	err := Update(work, func(w *World) error {
		s := w.Service(host, unit)
		now := time.Now().UTC()
		switch verb {
		case "is-active":
			if s == nil {
				fmt.Fprintln(out, "inactive")
				code = 3
				return nil
			}
			fmt.Fprintln(out, s.State)
			if s.State != "active" {
				code = 3
			}
		case "status":
			if s == nil {
				fmt.Fprintf(out, "Unit %s could not be found.\n", unit)
				code = 4
				return nil
			}
			active := s.State
			switch s.State {
			case "active":
				active = "active (running)"
			case "failed":
				active = "failed (Result: exit-code)"
			case "inactive":
				active = "inactive (dead)"
			}
			fmt.Fprintf(out, "● %s\n     Loaded: loaded (/lib/systemd/system/%s; enabled; preset: enabled)\n     Active: %s since %s\n",
				unit, unit, active, s.Changed.Format("Mon 2006-01-02 15:04:05 UTC"))
			if s.State != "active" {
				code = 3
			}
		case "start", "restart":
			if s == nil {
				fmt.Fprintf(out, "Failed to %s %s: Unit %s not found.\n", verb, unit, unit)
				code = 5
				return nil
			}
			if s.Broken {
				s.State, s.Changed = "failed", now
				w.Note("service", host, "systemctl %s %s failed: the unit is broken", verb, unit)
				fmt.Fprintf(out, "Job for %s failed because the control process exited with error code.\n", unit)
				code = 1
				return nil
			}
			if verb == "restart" || s.State != "active" {
				s.Restarts++
			}
			before := s.State
			s.State, s.Changed = "active", now
			w.Note("service", host, "systemctl %s %s: %s -> active (restarts %d)", verb, unit, before, s.Restarts)
		case "stop":
			if s == nil {
				fmt.Fprintf(out, "Failed to stop %s: Unit %s not loaded.\n", unit, unit)
				code = 5
				return nil
			}
			before := s.State
			s.State, s.Changed = "inactive", now
			w.Note("service", host, "systemctl stop %s: %s -> inactive", unit, before)
		default:
			fmt.Fprintf(out, "Unknown command verb %s.\n", verb)
			code = 1
		}
		return nil
	})
	if err != nil {
		fmt.Fprintln(out, "systemctl: the world is unreadable")
		return 1
	}
	return code
}

// human writes a byte count the way df -h does.
func human(n int64) string {
	units := []string{"", "K", "M", "G", "T", "P"}
	v := float64(n)
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v < 10 && i > 0 {
		return fmt.Sprintf("%.1f%s", v, units[i])
	}
	return fmt.Sprintf("%.0f%s", v, units[i])
}

// df prints the volumes of the host as df -h -x tmpfs does.
func df(work, host string, out io.Writer) int {
	w, err := LoadWorld(work)
	if err != nil {
		fmt.Fprintln(out, "df: the world is unreadable")
		return 1
	}
	h := w.Hosts[host]
	if h == nil {
		return 1
	}
	fmt.Fprintln(out, "Filesystem      Size  Used Avail Use% Mounted on")
	mounts := make([]string, 0, len(h.Disks))
	for m := range h.Disks {
		mounts = append(mounts, m)
	}
	slices.Sort(mounts)
	for i, m := range mounts {
		d := h.Disks[m]
		free := int64(float64(d.SizeBytes) * d.FreeRatio)
		used := d.SizeBytes - free
		fmt.Fprintf(out, "%-15s %5s %5s %5s %3.0f%% %s\n", fmt.Sprintf("/dev/vda%d", i+1), human(d.SizeBytes), human(used), human(free), 100*(1-d.FreeRatio), m)
	}
	return 0
}

// journalctl prints the world events of the host, or of one unit with -u, as journal lines.
func journalctl(work, host string, args []string, out io.Writer) int {
	unit := ""
	for i, a := range args {
		if (a == "-u" || a == "--unit") && i+1 < len(args) {
			unit = args[i+1]
		}
	}
	f, err := os.Open(filepath.Join(work, eventsFile))
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(out, "-- No entries --")
		return 0
	}
	if err != nil {
		return 1
	}
	defer f.Close()
	n := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Host != host || unit != "" && !strings.Contains(e.Detail, unit) {
			continue
		}
		fmt.Fprintf(out, "%s %s systemd[1]: %s\n", e.Time.Format("Jan 02 15:04:05"), host, e.Detail)
		n++
	}
	if n == 0 {
		fmt.Fprintln(out, "-- No entries --")
	}
	return 0
}
