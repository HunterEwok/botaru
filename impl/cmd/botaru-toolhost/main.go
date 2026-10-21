// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-toolhost is the tool execution adapter (E3) of one machine and tenant: it
// claims tool calls over P1 and runs each in a child process of its plugin. Its settings file
// names the placement and how each plugin starts:
//
//	placement: host
//	plugins:
//	  - name: cmd
//	    command: [/opt/botaru/bin/botaru-plugin, cmd, -manifest, /opt/botaru/src/examples/tools/cmd/tool.yaml, -host, web1]
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/toolhost"
	"github.com/HunterEwok/botaru/impl/internal/version"
	"github.com/HunterEwok/botaru/impl/internal/yamlx"
)

type settings struct {
	Placement string            `yaml:"placement"`
	Plugins   []toolhost.Plugin `yaml:"plugins"`
	KillAfter string            `yaml:"kill_after"`
	// FilesDir is where the copies of the attachments a call names are kept during the call.
	FilesDir string `yaml:"files_dir"`
}

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	coreURL := flag.String("core", "http://127.0.0.1:8401", "the P1 address of the core")
	tokenFile := flag.String("token-file", "", "the file with the worker token from worker.enroll")
	settingsFile := flag.String("config", "/etc/botaru/toolhost.yaml", "the toolhost settings: placement and plugins")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-toolhost"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, *coreURL, *tokenFile, *settingsFile); err != nil {
		log.Error("botaru-toolhost stopped", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, coreURL, tokenFile, settingsFile string) error {
	if os.Geteuid() == 0 {
		return fmt.Errorf("the toolhost does not run as root (DEP-3)")
	}
	token, err := config.Secret(tokenFile)
	if err != nil {
		return fmt.Errorf("reading the worker token: %w", err)
	}
	b, err := os.ReadFile(settingsFile)
	if err != nil {
		return err
	}
	var s settings
	if err := yamlx.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("%s: %w", settingsFile, err)
	}
	var killAfter time.Duration
	if s.KillAfter != "" {
		if killAfter, err = time.ParseDuration(s.KillAfter); err != nil {
			return fmt.Errorf("%s: kill_after: %w", settingsFile, err)
		}
		if killAfter <= 0 {
			return fmt.Errorf("%s: kill_after must be positive", settingsFile)
		}
	}
	h, err := toolhost.New(toolhost.Config{
		Core:      &p1client.Client{BaseURL: coreURL, Token: token, HTTP: &http.Client{Timeout: 2 * time.Minute}},
		Placement: s.Placement, Plugins: s.Plugins, KillAfter: killAfter, FilesDir: s.FilesDir, Logger: log,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := h.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
