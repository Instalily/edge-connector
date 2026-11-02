// Command connector is the appliance customers run inside their network.
//
//	connector -config /etc/edge-connector/config.yaml
//	connector -config /etc/edge-connector/config.yaml -check
//
// -check validates the configuration, prints the allowlist it enforces, and
// exits without connecting. -probe URL exits 0 if the URL answers 200, for
// container healthchecks in an image with no shell or curl:
//
//	connector -probe http://127.0.0.1:8080/readyz
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

	"github.com/Instalily/edge-connector/internal/connector"
	"github.com/Instalily/edge-connector/internal/health"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/edge-connector/config.yaml", "configuration file")
	check := flag.Bool("check", false, "validate the configuration, print the allowlist, and exit")
	probe := flag.String("probe", "", "exit 0 if this health URL answers 200, else 1")
	flag.Parse()

	if *probe != "" {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(*probe)
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "connector")
	cfg, err := connector.Load(*configPath)
	if err != nil {
		fail(log, err)
	}
	c, err := connector.New(cfg, log)
	if err != nil {
		fail(log, err)
	}
	if *check {
		fmt.Printf("configuration OK\nhub: %s (verifying %s)\n", cfg.Hub.Address, cfg.Hub.ServerName)
		if cfg.Enrolls() {
			fmt.Printf("certificate: enrolled, kept in %s\n", cfg.StateDir)
		} else {
			fmt.Printf("certificate: %s\n", cfg.TLS.CertFile)
		}
		fmt.Println("allow:")
		for _, r := range c.Rules() {
			fmt.Println("  " + r)
		}
		return
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	proxyURL := ""
	if p := c.Proxy(); p != nil {
		proxyURL = p.Redacted()
	}
	log.Info("starting", "version", version, "hub", cfg.Hub.Address, "proxy", proxyURL, "allow", c.Rules())
	if cfg.HealthListen != "" {
		go health.Serve(ctx, cfg.HealthListen, func() bool { return true }, c.Ready, log)
	}
	c.Run(ctx)
	log.Info("stopped")
}

func fail(log *slog.Logger, err error) {
	log.Error("cannot start", "error", err)
	os.Exit(1)
}
