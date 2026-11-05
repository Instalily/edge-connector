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
	"sync/atomic"
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
	// draining is set when the connector gives notice that it will drain
	// the tunnel. The tunnel still carries streams until then.
	draining atomic.Bool
}

// Hub serves one customer's connectors and endpoints.
type Hub struct {
	cfg       *Config
	log       *slog.Logger
	tls       *tls.Config
	issuer    *issuer // nil without enrollment
	tunnelLn  net.Listener
	endpoints []*endpoint
	draining  atomic.Bool

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

// Ready reports whether the hub should get endpoint connections: it has at
// least one tunnel that isn't about to drain, and it isn't draining itself.
func (h *Hub) Ready() bool {
	if !h.Live() {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.ContainsFunc(h.tunnels, func(t *tunnel) bool { return !t.draining.Load() })
}

// Live reports whether the hub should get new connections at all: false once
// it is draining.
func (h *Hub) Live() bool { return !h.draining.Load() }

// Drain makes the hub report itself unhealthy, so load balancers stop sending
// it connectors and clients, while it keeps serving whatever still arrives.
func (h *Hub) Drain() { h.draining.Store(true) }

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
	case !slices.Contains(protocol.ALPNs, st.NegotiatedProtocol):
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
	// A version 2 tunnel's first stream is its control stream, set up before
	// the tunnel carries anything else.
	if st.NegotiatedProtocol == protocol.ALPN2 {
		ctl, err := h.openControl(sess)
		if err != nil {
			log.Warn("tunnel rejected", "error", fmt.Errorf("control stream: %w", err))
			sess.Close()
			return
		}
		go h.watchControl(ctl, t, log)
	}
	h.add(t)
	defer h.remove(t)
	defer sess.Close()
	log.Info("tunnel up", "protocol", st.NegotiatedProtocol, "certificate_expires", leaf.NotAfter.UTC().Format(time.RFC3339))

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

// openControl opens a version 2 tunnel's control stream and waits for the
// connector to accept it.
func (h *Hub) openControl(sess *yamux.Session) (*yamux.Stream, error) {
	stream, err := sess.OpenStream()
	if err != nil {
		return nil, err
	}
	stream.SetDeadline(time.Now().Add(h.cfg.OpenTimeout))
	req := protocol.Request{Control: &protocol.Control{DrainNoticeMS: h.cfg.DrainNotice.Milliseconds()}}
	if err := protocol.WriteFrame(stream, req); err != nil {
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

// watchControl marks t draining when its connector gives notice, until the
// control stream closes.
func (h *Hub) watchControl(ctl *yamux.Stream, t *tunnel, log *slog.Logger) {
	defer ctl.Close()
	for {
		var n protocol.Notice
		if err := protocol.ReadFrame(ctl, &n); err != nil {
			return
		}
		if n.Draining && !t.draining.Swap(true) {
			log.Info("tunnel draining", "notice", h.cfg.DrainNotice.String())
		}
	}
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
	for _, t := range h.candidates() {
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

// candidates lists the open tunnels to try for a stream, round-robin, with
// those about to drain last.
func (h *Hub) candidates() []*tunnel {
	h.mu.Lock()
	defer h.mu.Unlock()
	var staying, draining []*tunnel
	for i := range len(h.tunnels) {
		t := h.tunnels[(h.next+i)%len(h.tunnels)]
		switch {
		case t.IsClosed():
		case t.draining.Load():
			draining = append(draining, t)
		default:
			staying = append(staying, t)
		}
	}
	h.next++
	return append(staying, draining...)
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
