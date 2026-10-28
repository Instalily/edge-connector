// Package e2e runs a hub and connectors in process, over real TLS on
// loopback, and checks what a client of the hub's endpoints sees.
package e2e

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Instalily/edge-connector/internal/connector"
	"github.com/Instalily/edge-connector/internal/hub"
	"github.com/Instalily/edge-connector/internal/pki"
)

const day = 24 * time.Hour

// issuePKI writes a customer CA, a hub certificate for 127.0.0.1, and a
// connector certificate into dir.
func issuePKI(t *testing.T, dir string) {
	t.Helper()
	ca, err := pki.NewCA("test CA", day)
	must(t, err)
	must(t, ca.Write(filepath.Join(dir, "ca.crt"), filepath.Join(dir, "ca.key")))
	srv, err := ca.Issue(pki.LeafOptions{CommonName: "hub", IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Server: true, Validity: day})
	must(t, err)
	must(t, srv.Write(filepath.Join(dir, "hub.crt"), filepath.Join(dir, "hub.key")))
	cl, err := ca.Issue(pki.LeafOptions{CommonName: "connector-1", Client: true, Validity: day})
	must(t, err)
	must(t, cl.Write(filepath.Join(dir, "connector.crt"), filepath.Join(dir, "connector.key")))
}

func startHub(t *testing.T, ctx context.Context, dir, endpoints string) *hub.Hub {
	t.Helper()
	path := filepath.Join(dir, "hub.yaml")
	must(t, os.WriteFile(path, fmt.Appendf(nil, `
tunnel:
  listen: 127.0.0.1:0
  tls:
    ca_file: %[1]s/ca.crt
    cert_file: %[1]s/hub.crt
    key_file: %[1]s/hub.key
open_timeout: 5s
endpoints:
%[2]s`, dir, endpoints), 0o600))
	cfg, err := hub.Load(path)
	must(t, err)
	h, err := hub.New(cfg, logger(t, "hub"))
	must(t, err)
	go h.Serve(ctx)
	return h
}

func startConnector(t *testing.T, ctx context.Context, dir, certDir string, hubAddr net.Addr, allow string) {
	t.Helper()
	path := filepath.Join(dir, "connector.yaml")
	must(t, os.WriteFile(path, fmt.Appendf(nil, `
hub:
  address: %[1]s
  connections: 2
tls:
  ca_file: %[2]s/ca.crt
  cert_file: %[3]s/connector.crt
  key_file: %[3]s/connector.key
limits:
  dial_timeout: 2s
allow:
%[4]s`, hubAddr, dir, certDir, allow), 0o600))
	cfg, err := connector.Load(path)
	must(t, err)
	c, err := connector.New(cfg, logger(t, "connector"))
	must(t, err)
	go c.Run(ctx)
}

func TestTunnel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	issuePKI(t, dir)

	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)
	closed := closedPort(t)

	h := startHub(t, ctx, dir, fmt.Sprintf(`
  - {name: echo, listen: "127.0.0.1:0", target: "%[1]s"}
  - {name: by-name, listen: "127.0.0.1:0", target: "localhost:%[2]s"}
  - {name: wrong-port, listen: "127.0.0.1:0", target: "127.0.0.1:%[3]d"}
  - {name: outside, listen: "127.0.0.1:0", target: "10.255.255.1:%[2]s"}
  - {name: down, listen: "127.0.0.1:0", target: "127.0.0.1:%[4]d"}
`, echo, echoPort, mustPort(t, echoPort)+1, closed))
	startConnector(t, ctx, dir, dir, h.TunnelAddr(), fmt.Sprintf(`
  - {cidr: 127.0.0.1/32, ports: [%s, %d]}
`, echoPort, closed))
	waitFor(t, func() bool { return h.Tunnels() == 2 })

	t.Run("round trip with half-close", func(t *testing.T) {
		got := roundTrip(t, h.EndpointAddr("echo"), []byte("hello"))
		if string(got) != "hello" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("hostname resolved and checked against CIDR rules", func(t *testing.T) {
		got := roundTrip(t, h.EndpointAddr("by-name"), []byte("by name"))
		if string(got) != "by name" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("bulk transfers in parallel", func(t *testing.T) {
		var wg sync.WaitGroup
		for range 16 {
			wg.Go(func() {
				payload := make([]byte, 4<<20)
				rand.Read(payload)
				got := roundTrip(t, h.EndpointAddr("echo"), payload)
				if sha256.Sum256(got) != sha256.Sum256(payload) {
					t.Errorf("payload corrupted: sent %d bytes, got %d", len(payload), len(got))
				}
			})
		}
		wg.Wait()
	})

	for _, name := range []string{"wrong-port", "outside", "down"} {
		t.Run("refused: "+name, func(t *testing.T) {
			assertRefused(t, h.EndpointAddr(name))
		})
	}
}

func TestHubRejectsConnectorFromAnotherCA(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir, rogue := t.TempDir(), t.TempDir()
	issuePKI(t, dir)
	issuePKI(t, rogue)

	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)
	h := startHub(t, ctx, dir, fmt.Sprintf(`
  - {name: echo, listen: "127.0.0.1:0", target: "%s"}
`, echo))
	// The rogue connector trusts the real hub's CA but holds a certificate
	// from another CA.
	startConnector(t, ctx, dir, rogue, h.TunnelAddr(), fmt.Sprintf(`
  - {cidr: 127.0.0.1/32, ports: [%s]}
`, echoPort))

	time.Sleep(500 * time.Millisecond)
	if n := h.Tunnels(); n != 0 {
		t.Fatalf("hub accepted %d tunnels from a foreign CA", n)
	}
	assertRefused(t, h.EndpointAddr("echo"))
}

// roundTrip sends payload to an echo endpoint, half-closes, and returns
// everything read back.
func roundTrip(t *testing.T, addr net.Addr, payload []byte) []byte {
	t.Helper()
	conn, err := net.Dial("tcp", addr.String())
	must(t, err)
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	go func() {
		conn.Write(payload)
		conn.(*net.TCPConn).CloseWrite()
	}()
	got, err := io.ReadAll(conn)
	if err != nil {
		t.Errorf("read: %v", err)
	}
	return got
}

// assertRefused checks that the hub closes a connection to addr without
// passing any data.
func assertRefused(t *testing.T, addr net.Addr) {
	t.Helper()
	conn, err := net.Dial("tcp", addr.String())
	must(t, err)
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	conn.Write([]byte("should not arrive"))
	got, err := io.ReadAll(conn)
	if len(got) != 0 {
		t.Fatalf("refused connection returned %q", got)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("connection was left open instead of closed")
	}
}

func echoServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.Copy(c, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// closedPort returns a loopback port with nothing listening.
func closedPort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// logBuf collects logs and prints them only if the test fails. Background
// goroutines may still log after the test ends, when t.Log would panic.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func logger(t *testing.T, component string) *slog.Logger {
	lb := &logBuf{}
	t.Cleanup(func() {
		if t.Failed() {
			lb.mu.Lock()
			t.Log("\n" + lb.b.String())
			lb.mu.Unlock()
		}
	})
	return slog.New(slog.NewTextHandler(lb, nil)).With("component", component)
}

func mustPort(t *testing.T, s string) int {
	var p int
	_, err := fmt.Sscan(s, &p)
	must(t, err)
	return p
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
