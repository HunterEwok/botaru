// SPDX-License-Identifier: AGPL-3.0-only

// Command botaru-mockmodel is the mock model provider of the test bench: it answers in the
// Anthropic Messages format by a recorded scenario (spec/bindings/model-providers/mock.md).
// The core accepts its address only when the test bench key is set.
package main

import (
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/HunterEwok/botaru/impl/internal/mockprovider"
	"github.com/HunterEwok/botaru/impl/internal/version"
)

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	listen := flag.String("listen", "127.0.0.1:8409", "the listen address")
	scenario := flag.String("scenario", "", "the scenario file, for example examples/hello-bot/mock-model.yaml")
	flag.Parse()
	if *showVersion {
		fmt.Println(version.String("botaru-mockmodel"))
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	sc, err := mockprovider.Load(*scenario)
	if err != nil {
		log.Error("loading the scenario", "err", err)
		os.Exit(1)
	}
	s := &http.Server{Addr: *listen, Handler: mockprovider.New(sc), ReadHeaderTimeout: 10 * time.Second}
	log.Info("mock model provider started", "listen", *listen, "scenario", sc.ID)
	if err := s.ListenAndServe(); err != nil {
		log.Error("stopped", "err", err)
		os.Exit(1)
	}
}
