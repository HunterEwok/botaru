// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-plugin runs one own plugin of this implementation as an MCP server on its
// standard streams: botaru-plugin cmd|logs|checks|service|queue|monitor -manifest tools/<plugin>/tool.yaml
// [flags]. The toolhost starts it as a child process, one process per call (DEP-7).
//
// botaru-plugin polkit -catalog <file> -host <machine> -user <os user> prints the polkit rule
// of the service plugin for one machine of a tenant catalog (spec/bindings/os/systemd.md).
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/pluginmain"
	"github.com/HunterEwok/botaru/impl/internal/plugins"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "-version" {
		fmt.Println(version.String("botaru-plugin"))
		return
	}
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: botaru-plugin cmd|logs|checks|service|queue|monitor -manifest FILE [flags], or botaru-plugin polkit -catalog FILE -host H -user U")
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
	pluginmain.Exit("botaru-plugin", pluginmain.Main(kind, os.Args[2:]))
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
