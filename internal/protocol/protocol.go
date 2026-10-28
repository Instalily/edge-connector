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

// ALPN is the protocol version both sides must negotiate.
const ALPN = "edge-connector/1"

// MaxFrame bounds a frame's JSON body, so neither side can make the other
// buffer an arbitrary amount before a stream is established.
const MaxFrame = 4096

// Request opens a stream to Target, a host:port the connector resolves and
// dials inside the customer network.
type Request struct {
	Target string `json:"target"`
}

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
