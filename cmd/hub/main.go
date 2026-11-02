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
	"time"

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

	// On SIGTERM the hub first drains: its health checks fail, so load
	// balancers stop sending it connectors and clients, while it keeps
	// serving whatever still arrives. After shutdown_grace it stops. A
	// second signal stops it at once.
	signals, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-signals.Done()
		stop()
		log.Info("draining", "grace", cfg.ShutdownGrace.String())
		h.Drain()
		time.Sleep(cfg.ShutdownGrace)
		cancel()
	}()
	log.Info("starting", "version", version, "tunnel", h.TunnelAddr().String(), "endpoints", len(cfg.Endpoints))
	if cfg.HealthListen != "" {
		go health.Serve(ctx, cfg.HealthListen, h.Live, h.Ready, log)
	}
	h.Serve(ctx)
	log.Info("stopped")
}

func fail(log *slog.Logger, err error) {
	log.Error("cannot start", "error", err)
	os.Exit(1)
}
