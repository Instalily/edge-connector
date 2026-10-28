// Package connector is the appliance that runs in a customer's network. It
// dials out to the hub, keeps its tunnels open, and serves the streams the
// hub opens by dialing targets its local allowlist permits. It accepts no
// inbound connections and never opens a stream toward the hub.
package connector

import (
	"context"
	"crypto/tls"
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
	tc, err := tlsconfig.Client(cfg.TLS, cfg.Hub.ServerName)
	if err != nil {
		return nil, err
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

// Run keeps the configured number of tunnels open until ctx ends.
func (c *Connector) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for slot := range c.cfg.Hub.Connections {
		wg.Go(func() { c.maintain(ctx, slot) })
	}
	wg.Wait()
}

// maintain reopens one tunnel whenever it closes, backing off exponentially
// with jitter so a fleet of connectors does not reconnect in lockstep.
func (c *Connector) maintain(ctx context.Context, slot int) {
	const minBackoff, maxBackoff = time.Second, time.Minute
	backoff := minBackoff
	for ctx.Err() == nil {
		start := time.Now()
		err := c.tunnel(ctx, slot)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > maxBackoff {
			backoff = minBackoff
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

func (c *Connector) tunnel(ctx context.Context, slot int) error {
	dctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	d := &net.Dialer{KeepAlive: 30 * time.Second}
	raw, err := proxy.Dial(dctx, d, c.proxy, c.cfg.Hub.Address)
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	conn := tls.Client(raw, c.tls)
	if err := conn.HandshakeContext(dctx); err != nil {
		raw.Close()
		return fmt.Errorf("tls handshake: %w", err)
	}
	if p := conn.ConnectionState().NegotiatedProtocol; p != protocol.ALPN {
		conn.Close()
		return fmt.Errorf("hub negotiated protocol %q, want %q", p, protocol.ALPN)
	}
	sess, err := yamux.Client(conn, protocol.YamuxConfig(c.log))
	if err != nil {
		conn.Close()
		return err
	}
	defer sess.Close()
	stop := context.AfterFunc(ctx, func() { sess.Close() })
	defer stop()

	c.tunnels.Add(1)
	defer c.tunnels.Add(-1)
	c.log.Info("tunnel up", "slot", slot, "hub", c.cfg.Hub.Address)
	for {
		stream, err := sess.AcceptStream()
		if err != nil {
			return fmt.Errorf("tunnel closed: %w", err)
		}
		go c.serve(ctx, stream)
	}
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
