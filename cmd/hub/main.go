// Command hub is Instalily's end of one customer's tunnels.
//
//	hub -config /etc/edge-hub/config.yaml
package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Instalily/edge-connector/internal/health"
	"github.com/Instalily/edge-connector/internal/hub"
)

var version = "dev"

func main() {
	configPath := flag.String("config", "/etc/edge-hub/config.yaml", "configuration file")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("component", "hub")
	cfg, err := hub.Load(*configPath)
	if err != nil {
		fail(log, err)
	}
	h, err := hub.New(cfg, log)
	if err != nil {
		fail(log, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "version", version, "tunnel", h.TunnelAddr().String(), "endpoints", len(cfg.Endpoints))
	if cfg.HealthListen != "" {
		go health.Serve(ctx, cfg.HealthListen, h.Ready, log)
	}
	h.Serve(ctx)
	log.Info("stopped")
}

func fail(log *slog.Logger, err error) {
	log.Error("cannot start", "error", err)
	os.Exit(1)
}
