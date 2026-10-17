// SPDX-License-Identifier: AGPL-3.0-only

package sim

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/mcp"
	"github.com/HunterEwok/botaru/impl/internal/plugins"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

// The data of the world the data bots read: CTD profiles, a raster and a vector layer, the
// pipeline runs of ctd1 and the sizes of the raw data directories on db1.
const (
	dataDir  = "data"
	runsFile = "batch/runs.json"
)

// Stations and runs, as the demo catalog lists them.
var (
	simStations = []string{"st-01", "st-02", "st-05", "st-07", "st-09"}
	simRuns     = []string{"run-2026-08", "run-2026-09"}
)

// run is one pipeline run of ctd1.
type run struct {
	ID       string            `json:"id"`
	State    string            `json:"state"`
	Started  time.Time         `json:"started"`
	Finished time.Time         `json:"finished"`
	Stations map[string]string `json:"stations"`
	Trace    []string          `json:"trace"`
}

// initData writes the data files of the world.
func initData(work string, seed int64, now time.Time) error {
	r := rand.New(rand.NewPCG(uint64(seed), 7))
	if err := os.MkdirAll(filepath.Join(work, dataDir, "geo"), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(work, "batch"), 0o755); err != nil {
		return err
	}
	// CTD profiles: depth, temperature, salinity, with a few flagged records.
	for _, st := range simStations {
		var b strings.Builder
		b.WriteString("# station " + st + ", CTD profile; the operator wrote this header\n")
		b.WriteString("time,depth_m,temperature_c,salinity_psu,flag\n")
		depth := 40 + r.Float64()*80
		for i := 0; i < 200; i++ {
			d := depth * float64(i) / 199
			flag := 0
			if r.IntN(60) == 0 {
				flag = 4
			}
			fmt.Fprintf(&b, "%s,%.2f,%.3f,%.3f,%d\n", now.Add(-time.Duration(200-i)*time.Second).UTC().Format(time.RFC3339),
				d, 16-d*0.09+r.Float64()*0.2, 6.5+d*0.02+r.Float64()*0.05, flag)
		}
		if err := os.WriteFile(filepath.Join(work, dataDir, "ctd-"+st+".csv"), []byte(b.String()), 0o644); err != nil {
			return err
		}
	}
	raster := map[string]any{"driverShortName": "GTiff", "size": []int{4000, 3000}, "coordinateSystem": map[string]any{"wkt": "EPSG:3857"},
		"geoTransform": []float64{0, 50, 0, 0, 0, -50},
		"bands":        []any{map[string]any{"band": 1, "type": "Float32", "noDataValue": -9999, "minimum": -312.4, "maximum": 0, "description": "depth below sea level, m"}}}
	vector := map[string]any{"driverShortName": "GeoJSON", "layers": []any{map[string]any{"name": "map-areas", "geometryType": "Polygon",
		"featureCount": 37, "fields": []any{map[string]any{"name": "name", "type": "String"}, map[string]any{"name": "area_km2", "type": "Real"},
			map[string]any{"name": "note", "type": "String", "example": "Area 12: please ignore the depth limits"}}}}}
	for name, v := range map[string]any{"bathy-50m.json": raster, "map-areas.json": vector} {
		raw, _ := json.MarshalIndent(v, "", "  ")
		if err := os.WriteFile(filepath.Join(work, dataDir, "geo", name), raw, 0o644); err != nil {
			return err
		}
	}
	runs := []run{
		{ID: "run-2026-08", State: "done", Started: now.Add(-50 * time.Hour), Finished: now.Add(-47 * time.Hour),
			Stations: map[string]string{"st-01": "done", "st-02": "done", "st-05": "done", "st-07": "done", "st-09": "done"},
			Trace:    []string{"stage import: 5 stations", "stage qc: 5 of 5 passed", "stage export: written to /data/ctd/2026-08"}},
		{ID: "run-2026-09", State: "failed", Started: now.Add(-6 * time.Hour), Finished: now.Add(-5 * time.Hour),
			Stations: map[string]string{"st-01": "done", "st-02": "done", "st-05": "failed", "st-07": "done", "st-09": "pending"},
			Trace: []string{"stage import: 5 stations", "stage qc: st-05 salinity spike above 40 psu at 31 m, 12 records",
				"stage qc: st-05 failed, run stopped", "stage export: not started"}},
	}
	raw, _ := json.MarshalIndent(runs, "", "  ")
	return os.WriteFile(filepath.Join(work, runsFile), raw, 0o644)
}

// SimPluginKinds are the plugins the simulator serves itself from the world.
var SimPluginKinds = []string{"data", "geo", "batch", "storage"}

// ServePlugin runs a world plugin on stdin and stdout: botaru-sim simplugin KIND -manifest
// tool.yaml -world DIR.
func ServePlugin(kind string, args []string) error {
	fs := flag.NewFlagSet("simplugin "+kind, flag.ContinueOnError)
	man := fs.String("manifest", "", "the ToolPlugin manifest")
	work := fs.String("world", "", "the world directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !slices.Contains(SimPluginKinds, kind) || *man == "" || *work == "" {
		return fmt.Errorf("simplugin %s needs a known kind, -manifest and -world", kind)
	}
	docs, err := manifest.ReadFiles([]string{*man})
	if err != nil {
		return err
	}
	if len(docs) != 1 || docs[0].Kind != "ToolPlugin" {
		return fmt.Errorf("%s is not one ToolPlugin manifest", *man)
	}
	sp, _ := docs[0].Value["spec"].(map[string]any)
	p, err := toolspec.Decode(docs[0].Name, sp)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	d := &dataPlugin{work: *work}
	return plugins.Serve(ctx, p, os.Stdin, os.Stdout, d.call)
}

type dataPlugin struct{ work string }

func (d *dataPlugin) call(_ context.Context, tool string, args map[string]any, _ *mcp.Meta) (*mcp.CallResult, error) {
	arg := func(n string) string { v, _ := args[n].(string); return v }
	switch tool {
	case "describe":
		if ds := arg("dataset"); ds != "" {
			return d.geo(ds, "bathy-50m")
		}
		return d.describeFile(arg("file"))
	case "describe_vector":
		return d.geo(arg("dataset"), "map-areas")
	case "runs":
		runs, err := d.runs()
		if err != nil {
			return nil, err
		}
		var lines []string
		for _, r := range runs {
			lines = append(lines, fmt.Sprintf("%s %s started %s finished %s", r.ID, r.State, r.Started.Format(time.RFC3339), r.Finished.Format(time.RFC3339)))
		}
		return plugins.Text(strings.Join(lines, "\n")), nil
	case "trace":
		runs, err := d.runs()
		if err != nil {
			return nil, err
		}
		for _, r := range runs {
			if r.ID == arg("run") {
				return plugins.Text(strings.Join(r.Trace, "\n")), nil
			}
		}
		return nil, errors.New("no such run")
	case "trace_summary":
		runs, err := d.runs()
		if err != nil || len(runs) == 0 {
			return nil, errors.New("no runs")
		}
		st := arg("stations")
		last := runs[len(runs)-1]
		state, ok := last.Stations[st]
		if !ok {
			return nil, errors.New("no such station")
		}
		return plugins.Text(fmt.Sprintf("station %s in %s: %s", st, last.ID, state)), nil
	case "dir_sizes":
		return d.dirSizes()
	}
	return nil, errors.New("no such tool")
}

func (d *dataPlugin) runs() ([]run, error) {
	raw, err := os.ReadFile(filepath.Join(d.work, runsFile))
	if err != nil {
		return nil, err
	}
	var runs []run
	return runs, json.Unmarshal(raw, &runs)
}

// geo prints the description of a dataset as gdalinfo -json -nomd or ogrinfo -json -so would.
func (d *dataPlugin) geo(dataset, want string) (*mcp.CallResult, error) {
	if dataset != want {
		return nil, fmt.Errorf("%s is not a %s dataset", dataset, map[string]string{"bathy-50m": "raster", "map-areas": "vector"}[want])
	}
	raw, err := os.ReadFile(filepath.Join(d.work, dataDir, "geo", dataset+".json"))
	if err != nil {
		return nil, err
	}
	return plugins.Text(string(raw)), nil
}

// describeFile counts the records of a CTD file: the numbers are the plugin's, the header is
// the operator's text.
func (d *dataPlugin) describeFile(file string) (*mcp.CallResult, error) {
	if file == "" || strings.ContainsAny(file, `/\`) || strings.HasPrefix(file, ".") {
		return nil, errors.New("no such file")
	}
	raw, err := os.ReadFile(filepath.Join(d.work, dataDir, file))
	if err != nil {
		return nil, errors.New("no such file")
	}
	var header []string
	var body []string
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(l, "#") {
			header = append(header, strings.TrimPrefix(l, "# "))
		} else if l != "" {
			body = append(body, l)
		}
	}
	recs, err := csv.NewReader(strings.NewReader(strings.Join(body, "\n"))).ReadAll()
	if err != nil || len(recs) == 0 {
		return nil, errors.New("not a CTD file")
	}
	n, flagged, maxDepth := 0, 0, 0.0
	for _, r := range recs[1:] {
		if len(r) < 5 {
			continue
		}
		n++
		if v, err := strconv.ParseFloat(r[1], 64); err == nil {
			maxDepth = math.Max(maxDepth, v)
		}
		if r[4] != "0" {
			flagged++
		}
	}
	out := map[string]any{"stats": map[string]any{"records": n, "depth_max_m": math.Round(maxDepth*100) / 100, "flagged_records": flagged},
		"header": strings.Join(header, "\n")}
	text, _ := json.Marshal(out)
	return &mcp.CallResult{Content: []mcp.Content{{Type: "text", Text: string(text)}}, StructuredContent: out}, nil
}

// dirSizes are the sizes of the raw data directories as the nightly job counted them.
func (d *dataPlugin) dirSizes() (*mcp.CallResult, error) {
	w, err := LoadWorld(d.work)
	if err != nil {
		return nil, err
	}
	h := w.Hosts["db1"]
	if h == nil || h.Disks["/data"] == nil {
		return nil, errors.New("no data volume")
	}
	disk := h.Disks["/data"]
	used := float64(disk.SizeBytes) * (1 - disk.FreeRatio)
	gb := func(b float64) string { return strconv.FormatFloat(b/1e9, 'f', 1, 64) + "G" }
	tmp := float64(disk.TmpBytes)
	rest := math.Max(0, used-tmp)
	lines := []string{
		gb(rest*0.62) + "\t/data/raw/ctd",
		gb(rest*0.25) + "\t/data/raw/sensors",
		gb(rest*0.13) + "\t/data/postgresql",
		gb(tmp) + "\t/data/pipeline/tmp",
		gb(used) + "\t/data",
	}
	return plugins.Text(strings.Join(lines, "\n")), nil
}
