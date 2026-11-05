package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Instalily/edge-connector/internal/pipe"
	"github.com/Instalily/edge-connector/internal/protocol"
	"github.com/Instalily/edge-connector/internal/tlsconfig"
)

// Hubs and connectors from before version 2 offer only edge-connector/1.
// These tests stand in for them with minimal peers that speak only that.

// TestHubWithV1Connector checks that a hub serves a version 1 connector
// without opening a control stream, which such a connector would refuse.
func TestHubWithV1Connector(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	issuePKI(t, dir)
	echo := echoServer(t)

	h := startHub(t, ctx, dir, fmt.Sprintf(`
  - {name: echo, listen: "127.0.0.1:0", target: "%s"}
`, echo))
	tc, err := tlsconfig.Client(certFiles(dir, "connector"), "127.0.0.1")
	must(t, err)
	tc.NextProtos = []string{protocol.ALPN}
	conn, err := tls.Dial("tcp", h.TunnelAddr().String(), tc)
	must(t, err)
	defer conn.Close()
	if p := conn.ConnectionState().NegotiatedProtocol; p != protocol.ALPN {
		t.Fatalf("negotiated %q, want %q", p, protocol.ALPN)
	}
	sess, err := yamux.Client(conn, protocol.YamuxConfig(logger(t, "v1-connector")))
	must(t, err)
	var controls atomic.Int32
	go func() {
		for {
			stream, err := sess.AcceptStream()
			if err != nil {
				return
			}
			go func() {
				defer stream.Close()
				var req protocol.Request
				if protocol.ReadFrame(stream, &req) != nil {
					return
				}
				if req.Control != nil {
					controls.Add(1)
					protocol.WriteFrame(stream, protocol.Response{Error: protocol.ErrBadRequest})
					return
				}
				target, err := net.Dial("tcp", req.Target)
				if err != nil {
					protocol.WriteFrame(stream, protocol.Response{Error: protocol.ErrDialFailed})
					return
				}
				protocol.WriteFrame(stream, protocol.Response{OK: true})
				pipe.Join(stream, target)
			}()
		}
	}()
	waitFor(t, func() bool { return h.Ready() })

	if got := roundTrip(t, h.EndpointAddr("echo"), []byte("v1")); string(got) != "v1" {
		t.Fatalf("round trip through a version 1 connector: got %q", got)
	}
	if n := controls.Load(); n != 0 {
		t.Fatalf("hub sent %d control requests to a version 1 connector", n)
	}
}

// TestConnectorWithV1Hub checks that a connector still serves and rotates
// tunnels to a version 1 hub, draining them at once with no notice.
func TestConnectorWithV1Hub(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	issuePKI(t, dir)
	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)

	tc, err := tlsconfig.Server(certFiles(dir, "hub"))
	must(t, err)
	tc.NextProtos = []string{protocol.ALPN}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", tc)
	must(t, err)
	defer ln.Close()
	sessions := make(chan *yamux.Session, 32)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			sess, err := yamux.Server(conn, protocol.YamuxConfig(logger(t, "v1-hub")))
			if err != nil {
				conn.Close()
				continue
			}
			sessions <- sess
		}
	}()
	startConnector(t, ctx, dir, dir, ln.Addr(), fmt.Sprintf(`
  - {cidr: 127.0.0.1/32, ports: [%s]}
`, echoPort), time.Second)

	// Every tunnel carries a stream, and each replaced tunnel closes soon
	// after the connector rotates it, instead of after a notice period.
	var seen []*yamux.Session
	for len(seen) < 6 {
		select {
		case sess := <-sessions:
			seen = append(seen, sess)
			if got := v1RoundTrip(t, sess, echo, "v1 hub"); got != "v1 hub" {
				t.Fatalf("round trip through tunnel %d: got %q", len(seen), got)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("connector opened only %d tunnels in 10s with rotate_after 1s", len(seen))
		}
	}
	select {
	case <-seen[0].CloseChan():
	case <-time.After(5 * time.Second):
		t.Fatal("first tunnel still open 5s after it was replaced")
	}
}

// v1RoundTrip opens a stream as a version 1 hub does and echoes s through it.
func v1RoundTrip(t *testing.T, sess *yamux.Session, target, s string) string {
	t.Helper()
	stream, err := sess.OpenStream()
	must(t, err)
	defer stream.Close()
	stream.SetDeadline(time.Now().Add(10 * time.Second))
	must(t, protocol.WriteFrame(stream, protocol.Request{Target: target}))
	var resp protocol.Response
	must(t, protocol.ReadFrame(stream, &resp))
	if !resp.OK {
		t.Fatalf("connector refused the stream: %s", resp.Error)
	}
	echoLine(t, stream, s)
	return s
}

func certFiles(dir, name string) tlsconfig.Files {
	return tlsconfig.Files{
		CAFile:   filepath.Join(dir, "ca.crt"),
		CertFile: filepath.Join(dir, name+".crt"),
		KeyFile:  filepath.Join(dir, name+".key"),
	}
}
