// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-ingress is the webhook source driver of one source channel (port E1 with
// direction: in, spec/bindings/channels/webhook.md). It runs as its own OS user with only its
// own secrets: the worker token of the channel, the signing secrets or the token hash of the
// source, and the TLS key of its listener (SEC-1, SEC-2).
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/ingress"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	coreURL := flag.String("core", "http://127.0.0.1:8401", "the P1 address of the core")
	tokenFile := flag.String("token-file", "", "the file with the worker token from worker.enroll (kind channel, target the source channel)")
	tenant := flag.String("tenant", "", "the tenant of the source channel")
	channel := flag.String("channel", "", "the name of the source channel (a Channel with direction: in)")
	manifests := flag.String("manifests", "", "comma-separated files or folders of the tenant git that hold the Channel manifest")
	listen := flag.String("listen", ":8443", "the address of the listener; the path is /hooks/<source>")
	certFile := flag.String("tls-cert", "", "the TLS certificate of the listener")
	keyFile := flag.String("tls-key", "", "the TLS key of the listener")
	secretFiles := flag.String("secret-files", "", "auth: signature: comma-separated files with the signing secrets whsec_... (two during a rotation)")
	sourceTokenFiles := flag.String("source-token-files", "", "auth: tls_token: comma-separated files with the source tokens, or with sha256:<hex> of them (two during a rotation)")
	listenerPerMinute := flag.Int("listener-per-minute", 0, "the ceiling on all requests to the path in a sliding minute (0: ten times ingress.limits.per_minute)")
	specRoot := flag.String("spec-root", "", "the skeleton checkout with spec/model/formats.yaml (default: $BOTARU_SPEC_ROOT or a parent folder with spec/VERSION)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-ingress"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	if *tenant == "" || *channel == "" || *manifests == "" {
		fail("flags", errors.New("-tenant, -channel and -manifests are required"))
	}
	if *certFile == "" || *keyFile == "" {
		fail("flags", errors.New("the listener is served over TLS only: -tls-cert and -tls-key"))
	}
	token, err := config.Secret(*tokenFile)
	if err != nil {
		fail("reading the worker token", err)
	}
	ch, err := ingress.LoadChannel(strings.Split(*manifests, ","), *tenant, *channel)
	if err != nil {
		fail("reading the source channel", err)
	}
	root, err := spec.FindRoot(*specRoot)
	if err != nil {
		fail("finding the skeleton", err)
	}
	sp, err := spec.Load(root)
	if err != nil {
		fail("loading the skeleton", err)
	}
	formats, err := ingress.LoadFormats(sp)
	if err != nil {
		fail("loading the formats", err)
	}
	var keys [][]byte
	for _, f := range strings.Split(*secretFiles, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		s, err := config.Secret(f)
		if err != nil {
			fail("reading a signing secret", err)
		}
		k, err := ingress.SigningKey(s)
		if err != nil {
			fail("reading a signing secret", err)
		}
		keys = append(keys, k)
	}
	var hashes [][sha256.Size]byte
	for _, f := range strings.Split(*sourceTokenFiles, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		s, err := config.Secret(f)
		if err != nil {
			fail("reading the source token", err)
		}
		h, err := ingress.TokenHash(s)
		if err != nil {
			fail("reading the source token", err)
		}
		hashes = append(hashes, h)
	}
	core := &p1client.Client{BaseURL: *coreURL, Token: token, HTTP: &http.Client{Timeout: time.Minute}}
	d, err := ingress.New(ingress.Config{Core: core, Channel: *channel, Spec: ch, Secrets: keys, TokenHashes: hashes,
		ListenerPerMinute: *listenerPerMinute, Formats: formats, Logger: log})
	if err != nil {
		fail("configuring the source", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	for {
		err := d.Hello(ctx)
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		if r := p1client.Reason(err); r == "blocked" || r == "unauthenticated" || r == "version_unsupported" {
			fail("hello", err)
		}
		log.Warn("hello failed", "err", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	srv := &http.Server{Addr: *listen, Handler: d, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("botaru-ingress listening", "source", d.Path(), "addr", *listen)
	if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fail("listener", err)
	}
}
