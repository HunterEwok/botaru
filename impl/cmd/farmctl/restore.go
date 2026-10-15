// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/core"
)

// erasureLog runs erasure-log export: the erasure log as JSON for its separate backup (RET-5).
func erasureLog(args []string) error {
	fs := flag.NewFlagSet("erasure-log export", flag.ExitOnError)
	path := fs.String("config", "/etc/botaru/core.yaml", "the core's deployment settings file")
	if len(args) == 0 || args[0] != "export" {
		return errors.New("erasure-log export -config FILE")
	}
	_ = fs.Parse(args[1:])
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	c, closeStore, err := offlineCore(ctx, cfg, false)
	if err != nil {
		return err
	}
	defer closeStore()
	log, err := c.ErasureLogExport(ctx)
	if err != nil {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(log)
}

// restore runs restore begin (on the core machine, before the core starts on a restored store)
// and restore complete (over P2) (RET-6).
func restore(c *client, args []string) error {
	if len(args) == 0 {
		return errors.New("restore begin -config FILE -erasure-log FILE, or restore complete [-spend-pending]")
	}
	switch args[0] {
	case "begin":
		fs := flag.NewFlagSet("restore begin", flag.ExitOnError)
		path := fs.String("config", "/etc/botaru/core.yaml", "the core's deployment settings file")
		logFile := fs.String("erasure-log", "", "the separate erasure log backup from erasure-log export")
		_ = fs.Parse(args[1:])
		if *logFile == "" {
			return errors.New("restore begin needs -erasure-log: the erasure log is replayed before readiness")
		}
		b, err := os.ReadFile(*logFile)
		if err != nil {
			return err
		}
		var log []core.ErasureLogEntry
		if err := json.Unmarshal(b, &log); err != nil {
			return fmt.Errorf("%s: %w", *logFile, err)
		}
		cfg, err := config.Load(*path)
		if err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cc, closeStore, err := offlineCore(ctx, cfg, true)
		if err != nil {
			return err
		}
		defer closeStore()
		n, err := cc.RestoreBegin(ctx, log)
		if err != nil {
			return err
		}
		fmt.Printf("restore begun: %d erasure requests replayed; start the core, reconcile the spend gap, then farmctl restore complete\n", n)
		return nil
	case "complete":
		fs := flag.NewFlagSet("restore complete", flag.ExitOnError)
		pending := fs.Bool("spend-pending", false, "become ready before the provider report: halved daily limits until spend reconcile")
		_ = fs.Parse(args[1:])
		req := map[string]any{}
		if *pending {
			req["spend_pending"] = true
		}
		return c.print("restore.complete", req)
	}
	return errors.New("restore begin|complete")
}

// backupReport runs backup report: the external backup process reports a backup or a restore
// check (RET-8).
func backupReport(c *client, args []string) error {
	fs := flag.NewFlagSet("backup report", flag.ExitOnError)
	kind := fs.String("kind", "backup", "backup or restore_check")
	loc := fs.String("location", "", "the location_id of the deployment description")
	started := fs.String("started", "", "start, RFC 3339")
	finished := fs.String("finished", "", "end, RFC 3339")
	ok := fs.Bool("ok", false, "the backup or check succeeded")
	size := fs.Int64("size", -1, "the size in bytes")
	_ = fs.Parse(args)
	if *loc == "" || *started == "" || *finished == "" {
		return errors.New("backup report -kind K -location L -started T -finished T [-ok] [-size N]")
	}
	req := map[string]any{"kind": *kind, "location_id": *loc, "started_at": *started, "finished_at": *finished, "ok": *ok}
	if *size >= 0 {
		req["size"] = *size
	}
	return c.print("backup.report", req)
}
