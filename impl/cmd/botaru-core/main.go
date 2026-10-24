// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-core is the farm core: the P1 listener with the P4 path, the separate P2
// listener and the background loops over a PostgreSQL store.
package main

import (
	"context"
	"crypto/tls"
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
	"github.com/HunterEwok/botaru/impl/internal/httpbound"
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
	// A TLS key pair that does not load stops the start here too, not after the start-up.
	var p1TLS *tls.Config
	if cfg.P1TLSCertFile != "" {
		pair, err := tls.LoadX509KeyPair(cfg.P1TLSCertFile, cfg.P1TLSKeyFile)
		if err != nil {
			return fmt.Errorf("the P1 TLS key pair: %w", err)
		}
		p1TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	}
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
	key, keyID, prevKeys, err := cfg.TokenKeys()
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
	exportKeys := map[string]string{}
	for ref, f := range cfg.ExportKeyFiles {
		k, err := config.Secret(f)
		if err != nil {
			return fmt.Errorf("export key %s: %w", ref, err)
		}
		exportKeys[ref] = k
	}
	c, err := core.New(ctx, core.Config{
		TokenKey: key, KeyID: keyID, PreviousKeys: prevKeys, TestBench: cfg.TestBench, LeaseS: cfg.LeaseS, HeartbeatS: cfg.HeartbeatS,
		DeliveryMaxAttempts: cfg.DeliveryMaxAttempts, Operator: cfg.Operator, FarmHosts: cfg.FarmHosts, Health: cfg.Health, Spend: cfg.Spend, Backup: cfg.Backup, Files: cfg.Files, SecretUses: cfg.SecretUses, DeployFile: cfg.DeployDescription, ProviderKeySecrets: cfg.ProviderKeySecrets(), CoreSecrets: cfg.CoreOwnSecrets(), Workspaces: cfg.Workspaces, ExportKeys: exportKeys, Monitor: cfg.Monitor, Logger: log,
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
	srv, err := api.New(c, api.Config{Operators: cfg.Operators, Programs: cfg.Programs, HealthKeysSHA256: cfg.HealthKeysSHA256, ReportKeysSHA256: cfg.ReportKeysSHA256, TestKeySHA256: cfg.TestKeySHA256, Logger: log}, gw)
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

	// A client gets a bounded time for its request body and two minutes for an idle
	// connection: without a bound a client that sends slowly, or never closes, holds a
	// connection of the core without any credential. P1 takes files and model requests of up
	// to 33 MiB from other machines, so its window is five minutes.
	p1 := httpbound.Server(cfg.P1Listen, srv.P1Handler(), 5*time.Minute, 2*time.Minute)
	p1.TLSConfig = p1TLS
	p2 := httpbound.Server(cfg.P2Listen, srv.P2Handler(), time.Minute, 2*time.Minute)
	servers := map[*http.Server]net.Listener{p1: l1, p2: l2}
	if lt != nil {
		servers[httpbound.Server(cfg.TestListen, srv.TestHandler(), time.Minute, 2*time.Minute)] = lt
	}
	errc := make(chan error, len(servers))
	for s, l := range servers {
		go func() {
			var err error
			if s == p1 && p1TLS != nil {
				err = s.ServeTLS(l, "", "")
			} else {
				err = s.Serve(l)
			}
			if !errors.Is(err, http.ErrServerClosed) {
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
