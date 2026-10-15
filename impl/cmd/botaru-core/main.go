// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-core is the farm core: the P1 listener with the P4 path, the separate P2
// listener and the background loops over a PostgreSQL store.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // tenant time zones on hosts without a zone database

	"github.com/HunterEwok/botaru/impl/internal/api"
	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/gateway"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
	"github.com/HunterEwok/botaru/impl/internal/texts"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	configPath := flag.String("config", "/etc/botaru/core.yaml", "the deployment settings file")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-core"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*configPath, log); err != nil {
		log.Error("botaru-core stopped", "err", err)
		os.Exit(1)
	}
}

func run(path string, log *slog.Logger) error {
	cfg, err := config.Load(path)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind both listeners before touching the store: a second core started by mistake on
	// the same addresses must stop here, before its start-up ends the leases of the core
	// that is already running.
	l1, err := net.Listen("tcp", cfg.P1Listen)
	if err != nil {
		return err
	}
	defer l1.Close()
	l2, err := net.Listen("tcp", cfg.P2Listen)
	if err != nil {
		return err
	}
	defer l2.Close()
	var lt net.Listener
	if cfg.TestListen != "" {
		if lt, err = net.Listen("tcp", cfg.TestListen); err != nil {
			return err
		}
		defer lt.Close()
	}

	dsn, err := config.Secret(cfg.StoreDSNFile)
	if err != nil {
		return err
	}
	key, err := config.Secret(cfg.TokenKeyFile)
	if err != nil {
		return err
	}
	root, err := spec.FindRoot(cfg.SpecRoot)
	if err != nil {
		return err
	}
	sp, err := spec.Load(root)
	if err != nil {
		return err
	}
	tx, err := texts.Load(root, cfg.TextsDir)
	if err != nil {
		return err
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	c, err := core.New(ctx, core.Config{
		TokenKey: []byte(key), TestBench: cfg.TestBench, LeaseS: cfg.LeaseS, HeartbeatS: cfg.HeartbeatS,
		DeliveryMaxAttempts: cfg.DeliveryMaxAttempts, Operator: cfg.Operator, FarmHosts: cfg.FarmHosts, Health: cfg.Health, Spend: cfg.Spend, Backup: cfg.Backup, Logger: log,
	}, st, sp, tx)
	if err != nil {
		return err
	}
	providers := map[string]gateway.Provider{}
	for name, p := range cfg.Providers {
		keys := map[string]string{}
		for tenant, f := range p.KeyFiles {
			k, err := config.Secret(f)
			if err != nil {
				return fmt.Errorf("provider %s, tenant %s: %w", name, tenant, err)
			}
			keys[tenant] = k
		}
		providers[name] = gateway.Provider{Binding: p.Binding, BaseURL: p.BaseURL, Keys: keys}
	}
	gw, err := gateway.New(c, gateway.Config{Providers: providers, TestBench: cfg.TestBench, Logger: log})
	if err != nil {
		return err
	}
	srv, err := api.New(c, api.Config{Operators: cfg.Operators, HealthKeysSHA256: cfg.HealthKeysSHA256, TestKeySHA256: cfg.TestKeySHA256, Logger: log}, gw)
	if err != nil {
		return err
	}
	if err := c.RecoverLeases(ctx); err != nil {
		return err
	}
	// A new core start ends the drain of the rollout (LCY-6).
	if err := c.ClearDrain(ctx); err != nil {
		return err
	}
	if lt != nil {
		// With the test-control port the loops tick only by loop.tick (test-control.md: loops
		// do not start by themselves).
		go c.RunManual(ctx)
	} else {
		go c.Run(ctx)
	}

	p1 := &http.Server{Addr: cfg.P1Listen, Handler: srv.P1Handler(), ReadHeaderTimeout: 10 * time.Second}
	p2 := &http.Server{Addr: cfg.P2Listen, Handler: srv.P2Handler(), ReadHeaderTimeout: 10 * time.Second}
	servers := map[*http.Server]net.Listener{p1: l1, p2: l2}
	if lt != nil {
		servers[&http.Server{Addr: cfg.TestListen, Handler: srv.TestHandler(), ReadHeaderTimeout: 10 * time.Second}] = lt
	}
	errc := make(chan error, len(servers))
	for s, l := range servers {
		go func() {
			if err := s.Serve(l); !errors.Is(err, http.ErrServerClosed) {
				errc <- err
			}
		}()
	}
	log.Info("botaru-core started", "version", version.Version, "p1", cfg.P1Listen, "p2", cfg.P2Listen, "epoch", c.Epoch(), "test_bench", cfg.TestBench, "test_port", cfg.TestListen)
	select {
	case <-ctx.Done():
	case err = <-errc:
	}
	shut, cancel := context.WithTimeout(context.Background(), 70*time.Second)
	defer cancel()
	for s := range servers {
		_ = s.Shutdown(shut)
	}
	return err
}
