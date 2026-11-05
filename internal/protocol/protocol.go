// Package protocol defines what the hub and the connector say to each other.
//
// The connector dials the hub and opens a TLS 1.3 connection in which both
// sides present certificates. ALPN carries the protocol version, so a hub and
// a connector that share no version fail the handshake instead of
// misreading each other. Inside the TLS connection, yamux multiplexes streams.
//
// Only the hub opens streams. Each stream starts with one request frame from
// the hub, naming a target as host:port, and one response frame from the
// connector. After an OK response the stream carries the target connection's
// bytes unchanged in both directions.
//
// Version 2 adds a control stream. On a tunnel that negotiated it, the hub's
// first stream is a request with Control set instead of a target. After an
// OK response the connector keeps it open, and writes a Notice before it
// drains the tunnel. It then keeps serving the tunnel for the notice period
// the hub asked for, so the hub can fail its readiness check and its load
// balancer can stop sending it clients before the tunnel goes.
package protocol

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/hashicorp/yamux"
)

// ALPN protocol versions, newest first. Both sides offer all of them and use
// the newest they share.
const (
	ALPN2 = "edge-connector/2"
	ALPN  = "edge-connector/1"
)

// ALPNs is what both sides offer.
var ALPNs = []string{ALPN2, ALPN}

// MaxFrame bounds a frame's JSON body, so neither side can make the other
// buffer an arbitrary amount before a stream is established.
const MaxFrame = 4096

// Request opens a stream to Target, a host:port the connector resolves and
// dials inside the customer network.
type Request struct {
	Target string `json:"target"`
	// Control, sent only on a version 2 tunnel, opens its control stream.
	Control *Control `json:"control,omitempty"`
}

// Control opens a tunnel's control stream.
type Control struct {
	// DrainNoticeMS is how long, in milliseconds, the connector keeps the
	// tunnel serving after its Notice.
	DrainNoticeMS int64 `json:"drain_notice_ms"`
}

// Notice is what the connector writes on the control stream before it drains
// the tunnel.
type Notice struct {
	Draining bool `json:"draining"`
}

// MaxDrainNotice bounds the notice period a hub may ask for.
const MaxDrainNotice = 5 * time.Minute

// Response answers a Request. Error is one of the Err codes when OK is false.
type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// Error codes in a Response. They are deliberately coarse: the connector logs
// the details locally and does not describe the customer network to the hub.
const (
	ErrDenied     = "denied"
	ErrDialFailed = "dial_failed"
	ErrBadRequest = "bad_request"
	ErrBusy       = "busy"
)

// WriteFrame writes v as a frame: a two-byte big-endian length, then JSON.
func WriteFrame(w io.Writer, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(body) > MaxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(body), MaxFrame)
	}
	buf := make([]byte, 2+len(body))
	binary.BigEndian.PutUint16(buf, uint16(len(body)))
	copy(buf[2:], body)
	_, err = w.Write(buf)
	return err
}

// ReadFrame reads one frame written by WriteFrame into v.
func ReadFrame(r io.Reader, v any) error {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint16(hdr[:])
	if n == 0 || n > MaxFrame {
		return fmt.Errorf("frame length %d out of range", n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		return err
	}
	return json.NewDecoder(bytes.NewReader(body)).Decode(v)
}

// YamuxConfig is the session configuration both sides use. Keepalives detect
// a dead tunnel well inside the idle timeouts of NAT gateways and corporate
// firewalls, which are often five to fifteen minutes.
func YamuxConfig(log *slog.Logger) *yamux.Config {
	cfg := yamux.DefaultConfig()
	cfg.EnableKeepAlive = true
	cfg.KeepAliveInterval = 20 * time.Second
	cfg.ConnectionWriteTimeout = 15 * time.Second
	cfg.StreamOpenTimeout = 15 * time.Second
	// A larger window keeps bulk transfers fast over high-latency links.
	cfg.MaxStreamWindowSize = 1 << 20
	cfg.LogOutput = nil
	cfg.Logger = slog.NewLogLogger(log.Handler(), slog.LevelWarn)
	return cfg
}
