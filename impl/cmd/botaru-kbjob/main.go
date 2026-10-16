// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-kbjob is the ingest job of the knowledge corpora (kb.ingest.v1): a job worker
// without a model that cuts corpus documents into fragments for the per-tenant word index.
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
	"github.com/HunterEwok/botaru/impl/internal/kbjob"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	coreURL := flag.String("core", "http://127.0.0.1:8401", "the P1 address of the core")
	tokenFile := flag.String("token-file", "", "the file with the worker token from worker.enroll")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-kbjob"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	token, err := config.Secret(*tokenFile)
	if err != nil {
		log.Error("reading the worker token", "err", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	j := &kbjob.Job{Core: &p1client.Client{BaseURL: *coreURL, Token: token, HTTP: &http.Client{Timeout: 2 * time.Minute}}, Log: log}
	if err := j.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("botaru-kbjob stopped", "err", err)
		os.Exit(1)
	}
}
