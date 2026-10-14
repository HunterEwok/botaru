// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-plugin runs one own plugin of this implementation as an MCP server on its
// standard streams: botaru-plugin cmd|logs|checks|service|queue -manifest tools/<plugin>/tool.yaml
// [flags]. The toolhost starts it as a child process, one process per call (DEP-7).
//
// botaru-plugin polkit -catalog <file> -host <machine> -user <os user> prints the polkit rule
// of the service plugin for one machine of a tenant catalog (spec/bindings/os/systemd.md).
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/plugins"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

type sources map[string]string

func (s sources) String() string { return fmt.Sprint(map[string]string(s)) }

func (s sources) Set(v string) error {
	id, path, ok := strings.Cut(v, "=")
	if !ok || id == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("want <source>=<absolute path>")
	}
	s[id] = filepath.Clean(path)
	return nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(version.String("botaru-plugin"))
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: botaru-plugin cmd|logs|checks|service|queue -manifest FILE [flags], or botaru-plugin polkit -catalog FILE -host H -user U")
		os.Exit(2)
	}
	kind := os.Args[1]
	if kind == "polkit" {
		if err := polkit(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "botaru-plugin:", err)
			os.Exit(1)
		}
		return
	}
	fs := flag.NewFlagSet(kind, flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "the plugin manifest tools/<plugin>/tool.yaml")
	specRoot := fs.String("spec-root", "", "cmd: the skeleton checkout with spec/model/forbidden-programs.yaml (default: found as for the core)")
	host := fs.String("host", "", "cmd: the catalog machine this plugin runs on")
	results := fs.String("results", "", "checks: the results file of the checker")
	systemctl := fs.String("systemctl", "/usr/bin/systemctl", "service: the path of systemctl")
	queue := fs.String("queue", "", "queue: the tenant queue directory, for example /var/lib/botaru/demo/queue")
	src := sources{}
	fs.Var(src, "source", "logs: <source>=<absolute path>, repeated")
	_ = fs.Parse(os.Args[2:])
	if err := run(kind, *manifestPath, *specRoot, *host, *results, *systemctl, *queue, src); err != nil {
		fmt.Fprintln(os.Stderr, "botaru-plugin:", err)
		os.Exit(1)
	}
}

func run(kind, manifestPath, specRoot, host, results, systemctl, queue string, src sources) error {
	docs, err := manifest.ReadFiles([]string{manifestPath})
	if err != nil {
		return err
	}
	if len(docs) != 1 || docs[0].Kind != "ToolPlugin" {
		return fmt.Errorf("%s is not one ToolPlugin manifest", manifestPath)
	}
	specMap, _ := docs[0].Value["spec"].(map[string]any)
	p, err := toolspec.Decode(docs[0].Name, specMap)
	if err != nil {
		return err
	}
	var h plugins.Handler
	switch kind {
	case "cmd":
		root, err := spec.FindRoot(specRoot)
		if err != nil {
			return err
		}
		reg, err := spec.ReadYAMLFile(filepath.Join(root, "spec/model/forbidden-programs.yaml"))
		if err != nil {
			return err
		}
		forbidden := map[string]bool{}
		entries, _ := reg.(map[string]any)["entries"].([]any)
		for _, e := range entries {
			if m, ok := e.(map[string]any); ok {
				if id, ok := m["id"].(string); ok {
					forbidden[id] = true
				}
			}
		}
		c, err := plugins.NewCmd(p, host, forbidden)
		if err != nil {
			return err
		}
		h = c.Call
	case "logs":
		h = (&plugins.Logs{Sources: src}).Call
	case "checks":
		if results == "" {
			return fmt.Errorf("checks needs -results")
		}
		h = (&plugins.Checks{Results: results}).Call
	case "service":
		h = (&plugins.Service{Systemctl: systemctl}).Call
	case "queue":
		if queue == "" || !filepath.IsAbs(queue) {
			return fmt.Errorf("queue needs an absolute -queue directory")
		}
		h = (&plugins.Queue{Dir: queue}).Call
	default:
		return fmt.Errorf("unknown plugin %s", kind)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return plugins.Serve(ctx, p, os.Stdin, os.Stdout, h)
}

// polkit prints the rule for the via: host services of one machine of a catalog manifest.
func polkit(args []string) error {
	fs := flag.NewFlagSet("polkit", flag.ExitOnError)
	catalog := fs.String("catalog", "", "the Catalog manifest of the tenant")
	host := fs.String("host", "", "the catalog machine")
	user := fs.String("user", "", "the OS user of the tenant's action toolhost on the machine")
	_ = fs.Parse(args)
	docs, err := manifest.ReadFiles([]string{*catalog})
	if err != nil {
		return err
	}
	if len(docs) != 1 || docs[0].Kind != "Catalog" {
		return fmt.Errorf("%s is not one Catalog manifest", *catalog)
	}
	spec, _ := docs[0].Value["spec"].(map[string]any)
	hosts, _ := spec["hosts"].([]any)
	units := map[string][]string{}
	found := false
	for _, h := range hosts {
		hm, _ := h.(map[string]any)
		if hm["id"] != *host {
			continue
		}
		found = true
		svcs, _ := hm["services"].([]any)
		for _, s := range svcs {
			sm, _ := s.(map[string]any)
			if via, _ := sm["via"].(string); via != "" && via != "host" {
				continue
			}
			unit, _ := sm["unit"].(string)
			ctl, _ := sm["control"].([]any)
			for _, v := range ctl {
				if vs, ok := v.(string); ok {
					units[unit] = append(units[unit], vs)
				}
			}
		}
	}
	if !found {
		return fmt.Errorf("no machine %s in %s", *host, *catalog)
	}
	rule, err := plugins.PolkitRule(*catalog, *user, units)
	if err != nil {
		return err
	}
	fmt.Print(rule)
	return nil
}
