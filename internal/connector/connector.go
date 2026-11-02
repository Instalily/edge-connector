// Package connector is the appliance that runs in a customer's network. It
// dials out to the hub, keeps its tunnels open, and serves the streams the
// hub opens by dialing targets its local allowlist permits. It accepts no
// inbound connections and never opens a stream toward the hub.
package connector

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Instalily/edge-connector/internal/allowlist"
	"github.com/Instalily/edge-connector/internal/pipe"
	"github.com/Instalily/edge-connector/internal/pki"
	"github.com/Instalily/edge-connector/internal/protocol"
	"github.com/Instalily/edge-connector/internal/proxy"
	"github.com/Instalily/edge-connector/internal/tlsconfig"
)

// Connector keeps tunnels to one hub.
type Connector struct {
	cfg     *Config
	log     *slog.Logger
	allow   *allowlist.List
	tls     *tls.Config
	proxy   *url.URL
	streams chan struct{} // one token per stream being served
	tunnels atomic.Int32
}

// New checks cfg and prepares a connector.
func New(cfg *Config, log *slog.Logger) (*Connector, error) {
	allow, err := allowlist.New(cfg.Allow, net.DefaultResolver)
	if err != nil {
		return nil, err
	}
	// An enrolling connector may have no certificate yet; Run builds its TLS
	// configuration once it does.
	var tc *tls.Config
	if !cfg.Enrolls() {
		if tc, err = tlsconfig.Client(cfg.TLS, cfg.Hub.ServerName); err != nil {
			return nil, err
		}
	}
	var p *url.URL
	if cfg.Proxy != "" {
		p, err = url.Parse(cfg.Proxy)
	} else {
		p, err = proxy.FromEnvironment(cfg.Hub.Address)
	}
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	return &Connector{
		cfg:     cfg,
		log:     log,
		allow:   allow,
		tls:     tc,
		proxy:   p,
		streams: make(chan struct{}, cfg.Limits.MaxStreams),
	}, nil
}

// Rules describes the allowlist the connector enforces.
func (c *Connector) Rules() []string { return c.allow.Rules() }

// Proxy is the proxy the connector reaches the hub through, or nil.
func (c *Connector) Proxy() *url.URL { return c.proxy }

// Ready reports whether at least one tunnel is open.
func (c *Connector) Ready() bool { return c.tunnels.Load() > 0 }

// Run keeps the configured number of tunnels open until ctx ends. An
// enrolling connector first enrolls, if it has no valid certificate, and
// keeps renewing it.
func (c *Connector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	if c.cfg.Enrolls() {
		if !c.enrollUntilDone(ctx) {
			return
		}
		tc, err := tlsconfig.Client(c.state().Files(), c.cfg.Hub.ServerName)
		if err != nil {
			c.log.Error("cannot use enrolled certificate", "error", err)
			return
		}
		c.tls = tc
		wg.Go(func() { c.renewLoop(ctx) })
	}
	for slot := range c.cfg.Hub.Connections {
		wg.Go(func() { c.maintain(ctx, slot) })
	}
	wg.Wait()
}

// maintain keeps one tunnel open. It reopens a tunnel that closes, backing
// off exponentially with jitter so a fleet of connectors does not reconnect
// in lockstep, and replaces a tunnel that is due to rotate.
func (c *Connector) maintain(ctx context.Context, slot int) {
	const minBackoff, maxBackoff = time.Second, time.Minute
	backoff := minBackoff
	for ctx.Err() == nil {
		t, err := c.open(ctx, slot)
		for t != nil {
			start := time.Now()
			t, err = c.run(ctx, slot, t)
			if time.Since(start) > maxBackoff {
				backoff = minBackoff
			}
		}
		if ctx.Err() != nil {
			return
		}
		wait := backoff/2 + rand.N(backoff/2)
		c.log.Warn("tunnel down", "slot", slot, "error", err, "retry_in", wait.Round(time.Millisecond).String())
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// tunnel is one open session to the hub, counted in c.tunnels from when it
// opens until it closes or starts to drain.
type tunnel struct {
	sess     *yamux.Session
	rotateAt time.Time
}

func (c *Connector) open(ctx context.Context, slot int) (*tunnel, error) {
	// Read the certificate before the handshake: if a renewal lands in
	// between, the tunnel uses the newer certificate and only rotates early.
	cert, _ := pki.LoadCert(c.tlsFiles().CertFile)

	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := c.dialHub(dctx, c.cfg.Hub.Address)
	if err != nil {
		return nil, fmt.Errorf("dial hub: %w", err)
	}
	conn := tls.Client(raw, c.tls)
	if err := conn.HandshakeContext(dctx); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls handshake: %w", err)
	}
	if p := conn.ConnectionState().NegotiatedProtocol; p != protocol.ALPN {
		conn.Close()
		return nil, fmt.Errorf("hub negotiated protocol %q, want %q", p, protocol.ALPN)
	}
	sess, err := yamux.Client(conn, protocol.YamuxConfig(c.log))
	if err != nil {
		conn.Close()
		return nil, err
	}
	c.tunnels.Add(1)
	c.log.Info("tunnel up", "slot", slot, "hub", c.cfg.Hub.Address)
	return &tunnel{sess: sess, rotateAt: c.rotateAt(cert)}, nil
}

// rotateAt picks when a new tunnel rotates: after about RotateAfter, give or
// take a quarter, so tunnels opened together don't rotate together. It is no
// later than shortly before the certificate expires, because the hub closes
// a tunnel when its certificate does; rotating first moves the tunnel onto
// the renewed certificate without a gap. A certificate already that close to
// expiry hasn't been renewed, and rotating wouldn't help.
func (c *Connector) rotateAt(cert *x509.Certificate) time.Time {
	d := c.cfg.Hub.RotateAfter
	at := time.Now().Add(d*3/4 + rand.N(d/2))
	if cert == nil {
		return at
	}
	margin := min(time.Minute, cert.NotAfter.Sub(cert.NotBefore)/10)
	latest := cert.NotAfter.Add(-margin - rand.N(margin))
	if time.Until(latest) > margin && latest.Before(at) {
		return latest
	}
	return at
}

// run serves the hub's streams on t until it closes, or until it is due to
// rotate and a replacement is open, which it returns; t then drains in the
// background.
func (c *Connector) run(ctx context.Context, slot int, t *tunnel) (*tunnel, error) {
	closed := make(chan error, 1)
	go func() {
		for {
			stream, err := t.sess.AcceptStream()
			if err != nil {
				closed <- err
				return
			}
			go c.serve(ctx, stream)
		}
	}()
	stop := context.AfterFunc(ctx, func() { t.sess.Close() })
	timer := time.NewTimer(time.Until(t.rotateAt))
	defer timer.Stop()
	for {
		select {
		case err := <-closed:
			stop()
			t.sess.Close()
			c.tunnels.Add(-1)
			return nil, fmt.Errorf("tunnel closed: %w", err)
		case <-timer.C:
			next, err := c.open(ctx, slot)
			if err != nil {
				d := min(c.cfg.Hub.RotateAfter, time.Minute)
				retry := d/2 + rand.N(d/2)
				c.log.Warn("cannot rotate tunnel; keeping it", "slot", slot, "error", err, "retry_in", retry.Round(time.Millisecond).String())
				timer.Reset(retry)
				continue
			}
			c.tunnels.Add(-1)
			go c.drain(slot, t, closed, stop)
			return next, nil
		}
	}
}

// drain stops the hub opening streams on a replaced tunnel, and closes it once
// the streams it carries finish. The hub closes it anyway when its
// certificate expires.
func (c *Connector) drain(slot int, t *tunnel, closed <-chan error, stop func() bool) {
	defer stop()
	t.sess.GoAway()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for t.sess.NumStreams() > 0 {
		select {
		case <-closed:
			return
		case <-tick.C:
		}
	}
	t.sess.Close()
	c.log.Info("tunnel rotated", "slot", slot)
}

func (c *Connector) tlsFiles() tlsconfig.Files {
	if c.cfg.Enrolls() {
		return c.state().Files()
	}
	return c.cfg.TLS
}

// serve handles one stream the hub opened: read the target, check it
// against the allowlist, dial it, and join the two. Every outcome is logged
// here, in the customer's environment, as the audit trail.
func (c *Connector) serve(ctx context.Context, stream *yamux.Stream) {
	defer stream.Close()
	log := c.log.With("stream", stream.StreamID())

	select {
	case c.streams <- struct{}{}:
		defer func() { <-c.streams }()
	default:
		log.Warn("stream refused", "reason", protocol.ErrBusy)
		reply(stream, protocol.ErrBusy)
		return
	}

	stream.SetDeadline(time.Now().Add(c.cfg.Limits.DialTimeout + 5*time.Second))
	var req protocol.Request
	if err := protocol.ReadFrame(stream, &req); err != nil {
		log.Warn("stream refused", "reason", protocol.ErrBadRequest, "error", err)
		return
	}
	log = log.With("target", req.Target)
	host, portStr, err := net.SplitHostPort(req.Target)
	port, perr := strconv.ParseUint(portStr, 10, 16)
	if err != nil || perr != nil || port == 0 {
		log.Warn("stream refused", "reason", protocol.ErrBadRequest)
		reply(stream, protocol.ErrBadRequest)
		return
	}

	addrs, err := c.allow.Resolve(ctx, host, uint16(port))
	if errors.Is(err, allowlist.ErrDenied) {
		log.Warn("stream refused", "reason", protocol.ErrDenied)
		reply(stream, protocol.ErrDenied)
		return
	}
	if err != nil {
		log.Warn("stream refused", "reason", protocol.ErrDialFailed, "error", err)
		reply(stream, protocol.ErrDialFailed)
		return
	}
	target, err := c.dial(ctx, addrs)
	if err != nil {
		log.Warn("stream refused", "reason", protocol.ErrDialFailed, "error", err)
		reply(stream, protocol.ErrDialFailed)
		return
	}
	if err := protocol.WriteFrame(stream, protocol.Response{OK: true}); err != nil {
		target.Close()
		return
	}
	stream.SetDeadline(time.Time{})

	start := time.Now()
	log.Info("stream open", "remote", target.RemoteAddr().String())
	toTarget, fromTarget := pipe.Join(stream, target)
	log.Info("stream closed",
		"bytes_to_target", toTarget,
		"bytes_from_target", fromTarget,
		"duration", time.Since(start).Round(time.Millisecond).String())
}

func (c *Connector) dial(ctx context.Context, addrs []string) (net.Conn, error) {
	var errs []error
	for _, a := range addrs {
		dctx, cancel := context.WithTimeout(ctx, c.cfg.Limits.DialTimeout)
		conn, err := (&net.Dialer{}).DialContext(dctx, "tcp", a)
		cancel()
		if err == nil {
			return conn, nil
		}
		errs = append(errs, err)
	}
	return nil, errors.Join(errs...)
}

func reply(stream *yamux.Stream, code string) {
	protocol.WriteFrame(stream, protocol.Response{Error: code})
}
