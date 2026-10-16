// SPDX-License-Identifier: AGPL-3.0-only

// Package pluginmain runs one own plugin of this implementation as an MCP server on the
// standard streams, from the arguments of botaru-plugin: <kind> -manifest tools/<plugin>/tool.yaml
// [flags]. botaru-plugin and the eval test bench (which starts its own binary as the plugin
// process of its toolhosts) share it, so the bench runs the plugins unchanged (DEP-7).
package pluginmain

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/plugins"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/toolspec"
)

type sources map[string]string

// items are -item <field>=<Zabbix key> overrides of the monitor plugin.
type items map[string]string

func (s items) String() string { return fmt.Sprint(map[string]string(s)) }

func (s items) Set(v string) error {
	field, key, ok := strings.Cut(v, "=")
	if _, known := plugins.DefaultMonitorItems[field]; !ok || !known || key == "" {
		return fmt.Errorf("want <field>=<item key> with a field of item_history")
	}
	s[field] = key
	return nil
}

// monitorFlags are the flags of the monitor plugin.
type monitorFlags struct {
	api, tokenFile, catalog, tenant string
	items                           items
}

func (s sources) String() string { return fmt.Sprint(map[string]string(s)) }

func (s sources) Set(v string) error {
	id, path, ok := strings.Cut(v, "=")
	if !ok || id == "" || !filepath.IsAbs(path) {
		return fmt.Errorf("want <source>=<absolute path>")
	}
	s[id] = filepath.Clean(path)
	return nil
}

// UsageError is a bad flag; the flag package has already printed the usage.
type UsageError struct{ Err error }

func (e *UsageError) Error() string { return e.Err.Error() }
func (e *UsageError) Unwrap() error { return e.Err }

// Exit ends the process for the outcome of Main, with the codes of flag.ExitOnError: 0 for
// success and -h, 2 for a bad flag, and 1, after printing the error with prefix, otherwise.
func Exit(prefix string, err error) {
	var u *UsageError
	switch {
	case err == nil, errors.Is(err, flag.ErrHelp):
		os.Exit(0)
	case errors.As(err, &u):
		os.Exit(2)
	}
	fmt.Fprintln(os.Stderr, prefix+":", err)
	os.Exit(1)
}

// Main parses the flags of one plugin kind and serves it until its input ends.
func Main(kind string, args []string) error {
	fs := flag.NewFlagSet(kind, flag.ContinueOnError)
	manifestPath := fs.String("manifest", "", "the plugin manifest tools/<plugin>/tool.yaml")
	specRoot := fs.String("spec-root", "", "cmd: the skeleton checkout with spec/model/forbidden-programs.yaml (default: found as for the core)")
	host := fs.String("host", "", "cmd: the catalog machine this plugin runs on")
	results := fs.String("results", "", "checks: the results file of the checker")
	systemctl := fs.String("systemctl", "/usr/bin/systemctl", "service: the path of systemctl")
	queue := fs.String("queue", "", "queue: the tenant queue directory, for example /var/lib/botaru/demo/queue")
	src := sources{}
	fs.Var(src, "source", "logs: <source>=<absolute path>, repeated")
	mon := monitorFlags{items: items{}}
	fs.StringVar(&mon.api, "api", "", "monitor: the Zabbix API address, for example https://zabbix.example.org/api_jsonrpc.php")
	fs.StringVar(&mon.tokenFile, "api-token-file", "", "monitor: the file with the API token of the tenant's read-only Zabbix user")
	fs.StringVar(&mon.catalog, "catalog", "", "monitor: the tenant catalog file or folder (hosts[].monitor_host, monitor_triggers)")
	fs.StringVar(&mon.tenant, "tenant", "", "monitor: the tenant of the catalog")
	fs.Var(mon.items, "item", "monitor: <field>=<Zabbix item key>, overrides a key of item_history, repeated")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &UsageError{err}
	}
	return run(kind, *manifestPath, *specRoot, *host, *results, *systemctl, *queue, src, mon)
}

func run(kind, manifestPath, specRoot, host, results, systemctl, queue string, src sources, mon monitorFlags) error {
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
	case "monitor":
		m, err := monitorPlugin(mon)
		if err != nil {
			return err
		}
		h = m.Call
	default:
		return fmt.Errorf("unknown plugin %s", kind)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return plugins.Serve(ctx, p, os.Stdin, os.Stdout, h)
}

// monitorPlugin builds the monitor read plugin from the tenant catalog: only the hosts with a
// monitor_host and the triggers of monitor_triggers (MON-6).
func monitorPlugin(f monitorFlags) (*plugins.Monitor, error) {
	if f.api == "" || f.catalog == "" || f.tenant == "" {
		return nil, fmt.Errorf("monitor needs -api, -catalog and -tenant")
	}
	token, err := config.Secret(f.tokenFile)
	if err != nil {
		return nil, err
	}
	docs, err := manifest.ReadFiles([]string{f.catalog})
	if err != nil {
		return nil, err
	}
	m := &plugins.Monitor{API: f.api, Token: token, Hosts: map[string]string{}, Triggers: map[string]plugins.MonitorTrigger{}, Items: map[string]string{}}
	for k, v := range plugins.DefaultMonitorItems {
		m.Items[k] = v
	}
	for k, v := range f.items {
		m.Items[k] = v
	}
	found := false
	for _, d := range docs {
		if d.Kind != "Catalog" || d.Tenant != f.tenant {
			continue
		}
		found = true
		spec, _ := d.Value["spec"].(map[string]any)
		hosts, _ := spec["hosts"].([]any)
		for _, h := range hosts {
			hm, _ := h.(map[string]any)
			node, _ := hm["monitor_host"].(string)
			id, _ := hm["id"].(string)
			if node != "" && id != "" {
				m.Hosts[id] = node
			}
		}
		trigs, _ := spec["monitor_triggers"].([]any)
		for _, t := range trigs {
			tm, _ := t.(map[string]any)
			id, _ := tm["triggerid"].(string)
			class, _ := tm["class"].(string)
			item, _ := tm["item"].(string)
			if id != "" {
				m.Triggers[id] = plugins.MonitorTrigger{Class: class, Item: item}
			}
		}
	}
	if !found {
		return nil, fmt.Errorf("no catalog of tenant %s in %s", f.tenant, f.catalog)
	}
	return m, nil
}
