// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-agent is the agent worker of one role: it claims attempts over P1 and calls the
// model only through P4 with the attempt's model handle.
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

	"github.com/HunterEwok/botaru/impl/internal/agent"
	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	coreURL := flag.String("core", "http://127.0.0.1:8401", "the P1 address of the core")
	tokenFile := flag.String("token-file", "", "the file with the worker token from worker.enroll")
	maxTokens := flag.Int64("max-tokens", 1000, "the output limit of one model call; P4 caps it by the profile")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-agent"))
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
	a := agent.New(agent.Config{
		Core:      &p1client.Client{BaseURL: *coreURL, Token: token, HTTP: &http.Client{Timeout: 10 * time.Minute}},
		MaxTokens: *maxTokens, Logger: log,
	})
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("botaru-agent stopped", "err", err)
		os.Exit(1)
	}
}
