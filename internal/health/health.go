// Package health serves the liveness and readiness endpoints that container
// platforms probe.
package health

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"
)

// Serve answers /healthz while live returns true and /readyz while ready
// returns true, until ctx ends.
func Serve(ctx context.Context, addr string, live, ready func() bool, log *slog.Logger) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !live() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready() {
			http.Error(w, "no tunnel", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	// Probes are tiny and quick, so tight timeouts cost nothing and keep a
	// slow or idle client from holding connections open.
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	context.AfterFunc(ctx, func() {
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	})
	if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("health server stopped", "error", err)
	}
}
