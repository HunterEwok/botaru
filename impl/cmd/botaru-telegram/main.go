// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-telegram is the Telegram channel process of one farm channel.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/channel/telegram"
	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/texts"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	coreURL := flag.String("core", "http://127.0.0.1:8401", "the P1 address of the core")
	tokenFile := flag.String("token-file", "", "the file with the worker token from worker.enroll")
	channel := flag.String("channel", "telegram", "the farm channel name from the Channel manifest")
	botTokenFile := flag.String("bot-token-file", "", "the file with the Telegram bot token")
	botAPI := flag.String("bot-api", "https://api.telegram.org", "the Bot API server")
	specRoot := flag.String("spec-root", "", "the skeleton checkout with the message catalog (default: $BOTARU_SPEC_ROOT or a parent folder with spec/VERSION)")
	textsDir := flag.String("texts-dir", "", "a directory with extra locales of the code texts")
	links := flag.String("link-allowlist", "", "comma-separated hosts that model text may link to (the tenant's link_allowlist)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-telegram"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	token, err := config.Secret(*tokenFile)
	if err != nil {
		fail("reading the worker token", err)
	}
	botToken, err := config.Secret(*botTokenFile)
	if err != nil {
		fail("reading the bot token", err)
	}
	root, err := spec.FindRoot(*specRoot)
	if err != nil {
		fail("finding the skeleton", err)
	}
	cat, err := texts.Load(root, *textsDir)
	if err != nil {
		fail("loading texts", err)
	}
	var allow []string
	for _, h := range strings.Split(*links, ",") {
		if h = strings.TrimSpace(h); h != "" {
			allow = append(allow, h)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ch := telegram.New(telegram.Config{
		Core:    &p1client.Client{BaseURL: *coreURL, Token: token, HTTP: &http.Client{Timeout: 2 * time.Minute}},
		Channel: *channel, BotAPI: *botAPI, BotToken: botToken, Texts: cat, LinkAllowlist: allow, Logger: log,
	})
	if err := ch.Run(ctx); err != nil && ctx.Err() == nil {
		fail("botaru-telegram stopped", err)
	}
}
