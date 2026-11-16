package connector

import (
	"context"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/hashicorp/yamux"

	"github.com/Instalily/edge-connector/internal/allowlist"
	"github.com/Instalily/edge-connector/internal/protocol"
)

type fakeResolver map[string][]netip.Addr

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	if addrs, ok := f[host]; ok {
		return addrs, nil
	}
	return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
}

// The hub may ask for any name on a port that has a CIDR rule. Its answer
// must not say whether the name exists in the customer's DNS.
func TestRefusalDoesNotSayWhetherANameExists(t *testing.T) {
	allow, err := allowlist.New(
		[]allowlist.Entry{{CIDR: "10.20.0.0/16", Ports: []uint16{5432}}},
		fakeResolver{"exists.corp.local": {netip.MustParseAddr("192.168.7.9")}},
	)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	cfg := &Config{}
	cfg.Limits.DialTimeout = time.Second
	c := &Connector{cfg: cfg, log: log, allow: allow, streams: make(chan struct{}, 4)}

	// A tunnel without TLS: the hub's end opens streams, and the connector's
	// end serves them.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		sess, err := yamux.Client(conn, protocol.YamuxConfig(log))
		if err != nil {
			return
		}
		defer sess.Close()
		for {
			stream, err := sess.AcceptStream()
			if err != nil {
				return
			}
			go c.serve(ctx, stream)
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	hub, err := yamux.Server(conn, protocol.YamuxConfig(log))
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Close()

	refusal := func(target string) string {
		t.Helper()
		stream, err := hub.OpenStream()
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()
		stream.SetDeadline(time.Now().Add(10 * time.Second))
		if err := protocol.WriteFrame(stream, protocol.Request{Target: target}); err != nil {
			t.Fatal(err)
		}
		var resp protocol.Response
		if err := protocol.ReadFrame(stream, &resp); err != nil {
			t.Fatal(err)
		}
		if resp.OK {
			t.Fatalf("%s was allowed", target)
		}
		return resp.Error
	}
	exists := refusal("exists.corp.local:5432")
	missing := refusal("no-such-host.corp.local:5432")
	if exists != protocol.ErrDenied || missing != protocol.ErrDenied {
		t.Fatalf("a name that resolves outside the allowlist got %q, and one that does not resolve %q; want %q for both",
			exists, missing, protocol.ErrDenied)
	}
}
