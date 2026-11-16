package connector

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"log/slog"
	"math/big"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Instalily/edge-connector/internal/allowlist"
	"github.com/Instalily/edge-connector/internal/enroll"
	"github.com/Instalily/edge-connector/internal/pki"
)

// refusingHub accepts connections and closes them at once, so every renewal
// fails as fast as it can. It returns its address and how many it accepted.
func refusingHub(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var attempts atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			attempts.Add(1)
			conn.Close()
		}
	}()
	return ln.Addr().String(), &attempts
}

// enrollingConnector is a connector that enrolls into an empty state
// directory, with the hub at hubAddr.
func enrollingConnector(t *testing.T, hubAddr string) *Connector {
	t.Helper()
	cfg := &Config{StateDir: t.TempDir()}
	cfg.Hub.Address = hubAddr
	cfg.Allow = []allowlist.Entry{{CIDR: "127.0.0.1", Ports: []uint16{1}}}
	if err := cfg.normalize(); err != nil {
		t.Fatal(err)
	}
	c, err := New(cfg, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// saveDueCertificate stores a certificate that is past two thirds of its
// lifetime, so due for renewal, with an hour left.
func saveDueCertificate(t *testing.T, st enroll.State) {
	t.Helper()
	ca, err := pki.NewCA("test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	key, _, err := enroll.NewRequest()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "connector-1"},
		NotBefore:    now.Add(-3 * time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Save(key, cert, ca.Cert); err != nil {
		t.Fatal(err)
	}
}

func TestRenewalBacksOffWhenItFails(t *testing.T) {
	addr, attempts := refusingHub(t)
	c := enrollingConnector(t, addr)
	saveDueCertificate(t, c.state())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.renewLoop(ctx)

	// Backing off from one second, with jitter, the attempts come at once,
	// 0.5 to 1s later, then 1 to 2s after that: two or three in two seconds.
	// Without backoff there are thousands.
	time.Sleep(2 * time.Second)
	if n := attempts.Load(); n < 2 || n > 4 {
		t.Fatalf("%d renewal attempts in 2s; want 2 to 4", n)
	}
}

func TestRenewalSurvivesAnUnreadableCertificate(t *testing.T) {
	addr, attempts := refusingHub(t)
	c := enrollingConnector(t, addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		c.renewLoop(ctx)
		close(done)
	}()

	// The state directory is empty, so the certificate can't be read.
	select {
	case <-done:
		t.Fatal("renewLoop gave up when it could not read the certificate")
	case <-time.After(200 * time.Millisecond):
	}

	// Once it can be, renewal carries on.
	saveDueCertificate(t, c.state())
	deadline := time.Now().Add(10 * time.Second)
	for attempts.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no renewal attempt after the certificate became readable")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
