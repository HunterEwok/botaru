// SPDX-License-Identifier: AGPL-3.0-only

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/core"
	"github.com/HunterEwok/botaru/impl/internal/spec"
	"github.com/HunterEwok/botaru/impl/internal/store"
)

const holdUsage = `hold set KIND [TENANT [NAME]] -effects E[,E] -reason R [-until TIME]
hold release ID
hold list [TENANT]
hold farm -config FILE -effects E[,E] -reason R [-until TIME] [-by PRINCIPAL]`

// hold runs the hold commands (HLD-1, HLD-2). set, release and list go over P2; hold farm
// writes a hold of scope farm straight to the store with the core's settings, without a
// running core and without a channel (DEP-8).
func hold(c *client, args []string) error {
	if len(args) == 0 {
		return errors.New(holdUsage)
	}
	switch args[0] {
	case "set":
		fs := flag.NewFlagSet("hold set", flag.ExitOnError)
		effects, reason, until := holdFlags(fs)
		_ = fs.Parse(reorder(args[1:]))
		if fs.NArg() < 1 || fs.NArg() > 3 || *effects == "" || *reason == "" {
			return errors.New(holdUsage)
		}
		scope := map[string]any{"kind": fs.Arg(0)}
		if fs.NArg() > 1 {
			scope["tenant"] = fs.Arg(1)
		}
		if fs.NArg() > 2 {
			name := fs.Arg(2)
			if fs.Arg(0) == "worker" {
				// Scope names are lowercase; a worker id from worker enroll is a ULID in
				// uppercase, and the core keeps worker scopes in lowercase.
				name = strings.ToLower(name)
			}
			scope["name"] = name
		}
		req := map[string]any{"scope": scope, "effects": strings.Split(*effects, ","), "reason": *reason, "until": nil}
		if *until != "" {
			req["until"] = *until
		}
		return c.print("hold.set", req)
	case "release":
		if len(args) != 2 {
			return errors.New(holdUsage)
		}
		return c.print("hold.release", map[string]any{"id": args[1]})
	case "list":
		req := map[string]any{}
		if len(args) == 2 {
			req["tenant"] = args[1]
		}
		return c.print("hold.list", req)
	case "farm":
		return holdFarm(args[1:])
	}
	return errors.New(holdUsage)
}

func holdFlags(fs *flag.FlagSet) (effects, reason, until *string) {
	effects = fs.String("effects", "", "effects from spec/model/holds.yaml, comma separated, for example no_claim,cancel_running")
	reason = fs.String("reason", "", "a reason from spec/model/reasons.yaml, for example compromised")
	until = fs.String("until", "", "the end, RFC 3339 (default: until released)")
	return
}

// holdFarm sets a hold of scope farm directly in the store. It needs the core's settings file
// and its secrets, so only the operator on the core machine can run it.
func holdFarm(args []string) error {
	fs := flag.NewFlagSet("hold farm", flag.ExitOnError)
	path := fs.String("config", "/etc/botaru/core.yaml", "the core's deployment settings file")
	by := fs.String("by", "", "the operator principal (default: the operator of farm in the settings)")
	effects, reason, until := holdFlags(fs)
	_ = fs.Parse(reorder(args))
	if fs.NArg() != 0 || *effects == "" || *reason == "" {
		return errors.New(holdUsage)
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	principal := *by
	for _, o := range cfg.Operators {
		if principal == "" && o.Tenant == "farm" {
			principal = o.Principal
		}
	}
	if principal == "" {
		return errors.New("hold farm: the settings name no operator of farm; give -by")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
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
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	c, err := core.New(ctx, core.Config{
		TokenKey: []byte(key), TestBench: cfg.TestBench, Offline: true,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, st, sp, nil)
	if err != nil {
		return fmt.Errorf("hold farm: the store is not ready (the core migrates it at its first start): %w", err)
	}
	req := core.HoldSetRequest{Scope: core.HoldScope{Kind: "farm"}, Effects: strings.Split(*effects, ","), Reason: *reason}
	if *until != "" {
		req.Until = until
	}
	out, err := c.HoldSet(ctx, core.HoldActor{Principal: principal, Rank: "operator", Tenant: "farm", Via: "farmctl"}, req)
	if err != nil {
		return err
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(b))
	return nil
}
