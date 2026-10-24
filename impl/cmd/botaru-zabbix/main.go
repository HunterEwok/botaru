// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-zabbix is the monitoring adapter process of one tenant for Zabbix 7.0 and
// later (port E5, spec/bindings/monitor/zabbix.md).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/config"
	"github.com/HunterEwok/botaru/impl/internal/httpbound"
	"github.com/HunterEwok/botaru/impl/internal/manifest"
	"github.com/HunterEwok/botaru/impl/internal/monitor/zabbix"
	"github.com/HunterEwok/botaru/impl/internal/p1client"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	coreURL := flag.String("core", "http://127.0.0.1:8401", "the P1 address of the core")
	tokenFile := flag.String("token-file", "", "the file with the worker token from worker.enroll (kind monitor)")
	tenant := flag.String("tenant", "", "the tenant of the worker token")
	target := flag.String("target", "zabbix", "the target name of the enroll record")
	api := flag.String("api", "", "the Zabbix API address, for example https://zabbix.example.org/api_jsonrpc.php")
	apiTokenFile := flag.String("api-token-file", "", "the file with the API token of the adapter's own Zabbix user")
	farmHost := flag.String("farm-host", "botaru-core", "the farm host of the export items")
	catalog := flag.String("catalog", "", "the tenant catalog file or folder: hosts[].monitor_host binds Zabbix hosts to catalog hosts")
	listen := flag.String("listen", "", "the address of the problem webhook, for example :8443; empty: no webhook (the farm process)")
	certFile := flag.String("tls-cert", "", "the TLS certificate of the webhook")
	keyFile := flag.String("tls-key", "", "the TLS key of the webhook")
	webhookTokenFile := flag.String("webhook-token-file", "", "the file with the token the Zabbix media type sends")
	maintenancePoll := flag.Int("maintenance-poll-s", 60, "maintenance_poll_s of the deployment binding: how often the maintenance status of the catalog nodes is read, at most 300 (MON-9)")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-zabbix"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	fail := func(msg string, err error) {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
	if *tenant == "" || *api == "" {
		fail("flags", errors.New("-tenant and -api are required"))
	}
	// The API token travels in every call: plain HTTP only to this machine.
	if u, err := url.Parse(*api); err != nil || u.Scheme != "https" && (u.Scheme != "http" || !config.Loopback(hostPort(u))) {
		fail("flags", errors.New("-api is an https:// address, or http:// only to this machine"))
	}
	if *maintenancePoll < 1 || *maintenancePoll > 300 {
		fail("flags", errors.New("-maintenance-poll-s is between 1 and 300"))
	}
	token, err := config.Secret(*tokenFile)
	if err != nil {
		fail("reading the worker token", err)
	}
	apiToken, err := config.Secret(*apiTokenFile)
	if err != nil {
		fail("reading the Zabbix API token", err)
	}
	hosts := map[string]string{}
	if *catalog != "" {
		if hosts, err = catalogHosts(*catalog, *tenant); err != nil {
			fail("reading the catalog", err)
		}
	}
	var webhookToken string
	if *listen != "" {
		if *certFile == "" || *keyFile == "" {
			fail("flags", errors.New("the webhook is served over TLS only: -tls-cert and -tls-key"))
		}
		if webhookToken, err = config.Secret(*webhookTokenFile); err != nil {
			fail("reading the webhook token", err)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	a := zabbix.New(zabbix.Config{
		Core:   &p1client.Client{BaseURL: *coreURL, Token: token, HTTP: &http.Client{Timeout: 2 * time.Minute}},
		Tenant: *tenant, Target: *target, API: *api, APIToken: apiToken, FarmHost: *farmHost, Hosts: hosts,
		WebhookToken: webhookToken, Logger: log, MaintenanceEvery: time.Duration(*maintenancePoll) * time.Second,
	})
	if *listen != "" {
		// The webhook bounds the request and the idle connection, so an outsider without the
		// token cannot hold its connections open.
		srv := httpbound.Server(*listen, a.Webhook(), 30*time.Second, time.Minute)
		go func() {
			if err := srv.ListenAndServeTLS(*certFile, *keyFile); err != nil && !errors.Is(err, http.ErrServerClosed) {
				fail("webhook", err)
			}
		}()
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
	}
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		fail("botaru-zabbix stopped", err)
	}
}

// catalogHosts reads hosts[].monitor_host of the tenant catalog: Zabbix host name -> host id.
func catalogHosts(path, tenant string) (map[string]string, error) {
	docs, err := manifest.ReadFiles([]string{path})
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, d := range docs {
		if d.Kind != "Catalog" || d.Tenant != tenant {
			continue
		}
		spec, _ := d.Value["spec"].(map[string]any)
		hosts, _ := spec["hosts"].([]any)
		for _, h := range hosts {
			hm, _ := h.(map[string]any)
			node, _ := hm["monitor_host"].(string)
			id, _ := hm["id"].(string)
			if node != "" && id != "" {
				out[node] = id
			}
		}
	}
	return out, nil
}

// hostPort is the host and port of an address, with the default port of its scheme.
func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return net.JoinHostPort(u.Hostname(), "443")
	}
	return net.JoinHostPort(u.Hostname(), "80")
}
