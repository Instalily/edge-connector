// Package hub is Instalily's end of the tunnels for one customer. It accepts
// connectors' tunnels and exposes each configured target as a plain TCP
// listener: a connection to an endpoint becomes a stream through a tunnel,
// and the connector dials the target if its allowlist permits.
package hub

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Instalily/edge-connector/internal/enroll"
	"github.com/Instalily/edge-connector/internal/pipe"
	"github.com/Instalily/edge-connector/internal/protocol"
	"github.com/Instalily/edge-connector/internal/tlsconfig"
)

// ErrNoTunnel means no connector is connected.
var ErrNoTunnel = errors.New("no connector tunnel is open")

// RefusedError is a connector's refusal to open a stream.
type RefusedError struct{ Code string }

func (e *RefusedError) Error() string { return "connector refused the stream: " + e.Code }

type endpoint struct {
	Endpoint
	ln net.Listener
}

type tunnel struct {
	*yamux.Session
	identity string
	remote   string
}

// Hub serves one customer's connectors and endpoints.
type Hub struct {
	cfg       *Config
	log       *slog.Logger
	tls       *tls.Config
	issuer    *issuer // nil without enrollment
	tunnelLn  net.Listener
	endpoints []*endpoint

	mu      sync.Mutex
	tunnels []*tunnel
	next    int
}

// New checks cfg and binds the tunnel and endpoint listeners.
func New(cfg *Config, log *slog.Logger) (_ *Hub, err error) {
	tc, err := tlsconfig.Server(cfg.Tunnel.TLS)
	if err != nil {
		return nil, err
	}
	h := &Hub{cfg: cfg, log: log, tls: tc}
	if cfg.Enrollment != nil {
		if h.issuer, err = newIssuer(cfg.Tunnel.TLS.CAFile, cfg.Enrollment); err != nil {
			return nil, err
		}
		h.tls = withEnrollment(tc)
	}
	defer func() {
		if err != nil {
			h.closeListeners()
		}
	}()
	if h.tunnelLn, err = net.Listen("tcp", cfg.Tunnel.Listen); err != nil {
		return nil, fmt.Errorf("tunnel listener: %w", err)
	}
	for _, ep := range cfg.Endpoints {
		ln, err := net.Listen("tcp", ep.Listen)
		if err != nil {
			return nil, fmt.Errorf("endpoint %s: %w", ep.Name, err)
		}
		h.endpoints = append(h.endpoints, &endpoint{Endpoint: ep, ln: ln})
	}
	return h, nil
}

// TunnelAddr is the address connectors dial.
func (h *Hub) TunnelAddr() net.Addr { return h.tunnelLn.Addr() }

// EndpointAddr is the address an endpoint listens on, or nil.
func (h *Hub) EndpointAddr(name string) net.Addr {
	for _, ep := range h.endpoints {
		if ep.Name == name {
			return ep.ln.Addr()
		}
	}
	return nil
}

// Tunnels is the number of open tunnels.
func (h *Hub) Tunnels() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.tunnels)
}

// Ready reports whether at least one connector is connected.
func (h *Hub) Ready() bool { return h.Tunnels() > 0 }

// Serve accepts tunnels and endpoint connections until ctx ends.
func (h *Hub) Serve(ctx context.Context) {
	context.AfterFunc(ctx, h.closeListeners)
	var wg sync.WaitGroup
	wg.Go(func() {
		h.accept(h.tunnelLn, func(c net.Conn) { h.handleTunnel(ctx, c) })
	})
	for _, ep := range h.endpoints {
		wg.Go(func() {
			h.accept(ep.ln, func(c net.Conn) { h.handleClient(ctx, ep, c) })
		})
	}
	wg.Wait()
}

func (h *Hub) accept(ln net.Listener, handle func(net.Conn)) {
	for {
		conn, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			h.log.Error("accept failed", "listener", ln.Addr().String(), "error", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go handle(conn)
	}
}

func (h *Hub) handleTunnel(ctx context.Context, raw net.Conn) {
	log := h.log.With("remote", raw.RemoteAddr().String())
	conn := tls.Server(raw, h.tls)
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err := conn.HandshakeContext(hctx)
	cancel()
	if err != nil {
		log.Warn("tunnel rejected", "error", err)
		raw.Close()
		return
	}
	st := conn.ConnectionState()
	// Enrollment is the one protocol whose client has no certificate yet.
	if st.NegotiatedProtocol == enroll.ALPNEnroll && h.issuer != nil {
		defer conn.Close()
		h.handleEnroll(conn, log)
		return
	}
	if len(st.PeerCertificates) == 0 {
		log.Warn("tunnel rejected", "error", "no client certificate")
		conn.Close()
		return
	}
	leaf := st.PeerCertificates[0]
	identity := leaf.Subject.CommonName
	log = log.With("identity", identity)
	switch {
	case st.NegotiatedProtocol == enroll.ALPNRenew && h.issuer != nil:
		defer conn.Close()
		h.handleRenew(conn, log, identity)
		return
	case st.NegotiatedProtocol != protocol.ALPN:
		log.Warn("tunnel rejected", "error", fmt.Sprintf("protocol %q", st.NegotiatedProtocol))
		conn.Close()
		return
	case !h.identityAllowed(identity):
		log.Warn("tunnel rejected", "error", "identity not allowed")
		conn.Close()
		return
	}
	sess, err := yamux.Server(conn, protocol.YamuxConfig(h.log))
	if err != nil {
		conn.Close()
		return
	}
	t := &tunnel{Session: sess, identity: identity, remote: raw.RemoteAddr().String()}
	h.add(t)
	defer h.remove(t)
	defer sess.Close()
	log.Info("tunnel up", "certificate_expires", leaf.NotAfter.UTC().Format(time.RFC3339))

	// A tunnel lasts no longer than the certificate that opened it. The
	// connector reconnects with its renewed one.
	expiry := time.AfterFunc(time.Until(leaf.NotAfter), func() {
		log.Info("certificate expired; closing tunnel")
		sess.Close()
	})
	defer expiry.Stop()

	// Streams flow only from the hub to the connector. A connector that
	// opens one is misbehaving or compromised: drop its tunnel.
	go func() {
		s, err := sess.AcceptStream()
		if err != nil {
			return
		}
		log.Error("connector opened a stream; closing its tunnel", "stream", s.StreamID())
		sess.Close()
	}()

	select {
	case <-sess.CloseChan():
	case <-ctx.Done():
	}
	log.Info("tunnel down")
}

func (h *Hub) handleClient(ctx context.Context, ep *endpoint, conn net.Conn) {
	defer conn.Close()
	log := h.log.With("endpoint", ep.Name, "target", ep.Target, "client", conn.RemoteAddr().String())
	stream, identity, err := h.open(ctx, ep.Target)
	if err != nil {
		log.Warn("endpoint connection refused", "error", err)
		return
	}
	log = log.With("identity", identity, "stream", stream.StreamID())
	start := time.Now()
	log.Info("stream open")
	toTarget, fromTarget := pipe.Join(conn, stream)
	log.Info("stream closed",
		"bytes_to_target", toTarget,
		"bytes_from_target", fromTarget,
		"duration", time.Since(start).Round(time.Millisecond).String())
}

// open opens a stream to target, trying each tunnel in turn until one
// carries it. A connector's refusal is final: every connector for a
// customer enforces the same allowlist.
func (h *Hub) open(ctx context.Context, target string) (*yamux.Stream, string, error) {
	var lastErr error = ErrNoTunnel
	for range max(h.Tunnels(), 1) {
		t := h.pick()
		if t == nil {
			break
		}
		stream, err := h.request(ctx, t, target)
		var refused *RefusedError
		if err == nil || errors.As(err, &refused) {
			return stream, t.identity, err
		}
		lastErr = err
	}
	return nil, "", lastErr
}

func (h *Hub) request(ctx context.Context, t *tunnel, target string) (*yamux.Stream, error) {
	stream, err := t.OpenStream()
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(h.cfg.OpenTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	stream.SetDeadline(deadline)
	if err := protocol.WriteFrame(stream, protocol.Request{Target: target}); err != nil {
		stream.Close()
		return nil, err
	}
	var resp protocol.Response
	if err := protocol.ReadFrame(stream, &resp); err != nil {
		stream.Close()
		return nil, err
	}
	if !resp.OK {
		stream.Close()
		return nil, &RefusedError{Code: resp.Error}
	}
	stream.SetDeadline(time.Time{})
	return stream, nil
}

// pick returns the next open tunnel, round-robin.
func (h *Hub) pick() *tunnel {
	h.mu.Lock()
	defer h.mu.Unlock()
	for range len(h.tunnels) {
		t := h.tunnels[h.next%len(h.tunnels)]
		h.next++
		if !t.IsClosed() {
			return t
		}
	}
	return nil
}

func (h *Hub) add(t *tunnel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tunnels = append(h.tunnels, t)
}

func (h *Hub) remove(t *tunnel) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.tunnels = slices.DeleteFunc(h.tunnels, func(x *tunnel) bool { return x == t })
}

func (h *Hub) closeListeners() {
	if h.tunnelLn != nil {
		h.tunnelLn.Close()
	}
	for _, ep := range h.endpoints {
		ep.ln.Close()
	}
}
