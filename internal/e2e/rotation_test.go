package e2e

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func TestTunnelRotation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	issuePKI(t, dir)
	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)

	h := startHub(t, ctx, dir, fmt.Sprintf(`
  - {name: echo, listen: "127.0.0.1:0", target: "%s"}
`, echo))
	// Count the tunnels the connector opens on the way to the hub.
	var opened atomic.Int32
	front := forward(t, h.TunnelAddr(), &opened)
	startConnector(t, ctx, dir, dir, front, fmt.Sprintf(`
  - {cidr: 127.0.0.1/32, ports: [%s]}
`, echoPort), time.Second)
	waitFor(t, func() bool { return h.Tunnels() == 2 })

	// A stream opened now outlives its tunnel's rotation.
	long, err := net.Dial("tcp", h.EndpointAddr("echo").String())
	must(t, err)
	defer long.Close()
	long.SetDeadline(time.Now().Add(30 * time.Second))
	echoLine(t, long, "before")

	// Tunnels rotate about every second. Endpoint connections must keep
	// working throughout, and the hub must never be left without one.
	for end := time.Now().Add(4 * time.Second); time.Now().Before(end); {
		if n := h.Tunnels(); n == 0 {
			t.Fatal("hub had no tunnel during rotation")
		}
		if got := roundTrip(t, h.EndpointAddr("echo"), []byte("during")); string(got) != "during" {
			t.Fatalf("round trip during rotation: got %q", got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if n := opened.Load(); n < 6 {
		t.Fatalf("connector opened %d tunnels in 4s; want rotation to have replaced each at least twice", n)
	}

	t.Run("a stream survives its tunnel draining", func(t *testing.T) {
		echoLine(t, long, "after")
	})

	t.Run("a drained tunnel closes once its stream ends", func(t *testing.T) {
		long.Close()
		waitFor(t, func() bool { return h.Tunnels() == 2 })
	})
}

// echoLine writes s to an echo connection and checks it comes back.
func echoLine(t *testing.T, conn net.Conn, s string) {
	t.Helper()
	_, err := conn.Write([]byte(s))
	must(t, err)
	got := make([]byte, len(s))
	_, err = io.ReadFull(conn, got)
	must(t, err)
	if string(got) != s {
		t.Fatalf("got %q, want %q", got, s)
	}
}

// forward relays TCP connections to target, counting them.
func forward(t *testing.T, target net.Addr, count *atomic.Int32) net.Addr {
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
			count.Add(1)
			go func() {
				defer c.Close()
				up, err := net.Dial("tcp", target.String())
				if err != nil {
					return
				}
				defer up.Close()
				go func() {
					io.Copy(up, c)
					up.Close()
				}()
				io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr()
}
