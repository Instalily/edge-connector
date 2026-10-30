package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Instalily/edge-connector/internal/connector"
	"github.com/Instalily/edge-connector/internal/enroll"
	"github.com/Instalily/edge-connector/internal/hub"
	"github.com/Instalily/edge-connector/internal/pki"
)

// enrollPKI is one customer's material when connectors enroll: a hub CA
// that signs only the hub's certificate, a connector CA whose key the hub
// signs with, and the token signing key.
type enrollPKI struct {
	dir      string
	hubCA    *pki.Pair
	tokenKey ed25519.PrivateKey
}

func newEnrollPKI(t *testing.T) *enrollPKI {
	t.Helper()
	dir := t.TempDir()
	hubCA, err := pki.NewCA("hub CA", day)
	must(t, err)
	must(t, hubCA.Write(filepath.Join(dir, "hub-ca.crt"), filepath.Join(dir, "hub-ca.key")))
	srv, err := hubCA.Issue(pki.LeafOptions{CommonName: "hub", IPs: []net.IP{net.IPv4(127, 0, 0, 1)}, Server: true, Validity: day})
	must(t, err)
	// The hub sends its CA after its certificate, so connectors can pin it.
	must(t, srv.Write(filepath.Join(dir, "hub.crt"), filepath.Join(dir, "hub.key"), hubCA.Cert))

	connCA, err := pki.NewCA("connector CA", day)
	must(t, err)
	must(t, connCA.Write(filepath.Join(dir, "connector-ca.crt"), filepath.Join(dir, "connector-ca.key")))

	must(t, enroll.GenerateTokenKey(filepath.Join(dir, "token.key"), filepath.Join(dir, "token.pub")))
	key, err := enroll.LoadTokenSigner(filepath.Join(dir, "token.key"))
	must(t, err)
	return &enrollPKI{dir: dir, hubCA: hubCA, tokenKey: key}
}

func (p *enrollPKI) token(t *testing.T, identity string, issued time.Time, lifetime time.Duration) string {
	t.Helper()
	c, err := enroll.NewClaims(identity, p.hubCA.Cert, issued, lifetime)
	must(t, err)
	tok, err := enroll.Sign(p.tokenKey, c)
	must(t, err)
	return tok
}

func startEnrollingHub(t *testing.T, ctx context.Context, p *enrollPKI, lifetime time.Duration, echo string) *hub.Hub {
	t.Helper()
	path := filepath.Join(p.dir, "hub.yaml")
	must(t, os.WriteFile(path, fmt.Appendf(nil, `
tunnel:
  listen: 127.0.0.1:0
  tls:
    ca_file: %[1]s/connector-ca.crt
    cert_file: %[1]s/hub.crt
    key_file: %[1]s/hub.key
  allowed_identities: [connector-1]
enrollment:
  ca_key_file: %[1]s/connector-ca.key
  hub_ca_file: %[1]s/hub-ca.crt
  token_keys_file: %[1]s/token.pub
  certificate_lifetime: %[2]s
open_timeout: 5s
endpoints:
  - {name: echo, listen: "127.0.0.1:0", target: "%[3]s"}
`, p.dir, lifetime, echo), 0o600))
	cfg, err := hub.Load(path)
	must(t, err)
	h, err := hub.New(cfg, logger(t, "hub"))
	must(t, err)
	go h.Serve(ctx)
	return h
}

func startEnrollingConnector(t *testing.T, ctx context.Context, dir string, hubAddr net.Addr, tokenFile, echoPort string) {
	t.Helper()
	path := filepath.Join(dir, "connector.yaml")
	must(t, os.WriteFile(path, fmt.Appendf(nil, `
hub:
  address: %[1]s
  connections: 2
state_dir: %[2]s/state
enrollment:
  token_file: %[3]s
limits:
  dial_timeout: 2s
allow:
  - {cidr: 127.0.0.1/32, ports: [%[4]s]}
`, hubAddr, dir, tokenFile, echoPort), 0o600))
	cfg, err := connector.Load(path)
	must(t, err)
	c, err := connector.New(cfg, logger(t, "connector"))
	must(t, err)
	go c.Run(ctx)
}

func TestEnrollRenewAndRestart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newEnrollPKI(t)
	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)

	// Six-second certificates: renewed after four, and tunnels closed by the
	// hub after six, so the test sees both within seconds.
	h := startEnrollingHub(t, ctx, p, 6*time.Second, echo)

	connDir := t.TempDir()
	tokenFile := filepath.Join(connDir, "token")
	must(t, os.WriteFile(tokenFile, []byte(p.token(t, "connector-1", time.Now(), time.Hour)+"\n"), 0o600))
	connCtx, stopConnector := context.WithCancel(ctx)
	startEnrollingConnector(t, connCtx, connDir, h.TunnelAddr(), tokenFile, echoPort)

	waitFor(t, func() bool { return h.Tunnels() == 2 })
	if got := roundTrip(t, h.EndpointAddr("echo"), []byte("enrolled")); string(got) != "enrolled" {
		t.Fatalf("got %q", got)
	}

	st := enroll.State{Dir: filepath.Join(connDir, "state")}
	first, err := st.Certificate()
	must(t, err)
	if first.Subject.CommonName != "connector-1" {
		t.Fatalf("certificate names %q, want the token's identity", first.Subject.CommonName)
	}
	info, err := os.Stat(filepath.Join(st.Dir, "identity.pem"))
	must(t, err)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("identity.pem mode %v, want 0600", info.Mode().Perm())
	}

	t.Run("renews before expiry, and tunnels outlive the first certificate", func(t *testing.T) {
		waitFor(t, func() bool {
			c, err := st.Certificate()
			return err == nil && c.SerialNumber.Cmp(first.SerialNumber) != 0
		})
		renewed, err := st.Certificate()
		must(t, err)
		if renewed.PublicKey.(*ecdsa.PublicKey).Equal(first.PublicKey) {
			t.Error("renewal kept the old key; it must rotate it")
		}
		time.Sleep(time.Until(first.NotAfter) + time.Second)
		waitFor(t, func() bool { return h.Tunnels() == 2 })
		if got := roundTrip(t, h.EndpointAddr("echo"), []byte("renewed")); string(got) != "renewed" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("restarts from its state without the token", func(t *testing.T) {
		stopConnector()
		waitFor(t, func() bool { return h.Tunnels() == 0 })
		must(t, os.Remove(tokenFile))
		startEnrollingConnector(t, ctx, connDir, h.TunnelAddr(), tokenFile, echoPort)
		waitFor(t, func() bool { return h.Tunnels() == 2 })
		if got := roundTrip(t, h.EndpointAddr("echo"), []byte("restarted")); string(got) != "restarted" {
			t.Fatalf("got %q", got)
		}
	})
}

func TestEnrollmentRefusals(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := newEnrollPKI(t)
	h := startEnrollingHub(t, ctx, p, time.Hour, echoServer(t))
	addr := h.TunnelAddr().String()
	dial := func(ctx context.Context, addr string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	}
	try := func(token string) error {
		return enroll.Enroll(ctx, dial, addr, "127.0.0.1", token, enroll.State{Dir: t.TempDir()})
	}
	hubCode := func(err error) string {
		var he *enroll.HubError
		if errors.As(err, &he) {
			return he.Code
		}
		return fmt.Sprintf("not a hub refusal: %v", err)
	}

	t.Run("expired token", func(t *testing.T) {
		err := try(p.token(t, "connector-1", time.Now().Add(-2*time.Hour), time.Hour))
		if got := hubCode(err); got != enroll.ErrExpiredToken.Error() {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("token signed by another key", func(t *testing.T) {
		other := newEnrollPKI(t)
		// Same hub CA, so the connector trusts the hub; only the signer differs.
		c, err := enroll.NewClaims("connector-1", p.hubCA.Cert, time.Now(), time.Hour)
		must(t, err)
		tok, err := enroll.Sign(other.tokenKey, c)
		must(t, err)
		if got := hubCode(try(tok)); got != enroll.ErrInvalidToken.Error() {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("identity the hub does not allow", func(t *testing.T) {
		if got := hubCode(try(p.token(t, "connector-2", time.Now(), time.Hour))); got != enroll.ErrIdentityNotAllowed {
			t.Fatalf("got %s", got)
		}
	})

	t.Run("token for another customer's hub", func(t *testing.T) {
		// The token pins a different hub CA, so the connector refuses this
		// hub before sending anything.
		err := try(newEnrollPKI(t).token(t, "connector-1", time.Now(), time.Hour))
		var he *enroll.HubError
		if err == nil || errors.As(err, &he) || !strings.Contains(err.Error(), "does not include the CA") {
			t.Fatalf("got %v, want a client-side pin failure", err)
		}
	})

	t.Run("renewal with a certificate from another CA", func(t *testing.T) {
		st := enroll.State{Dir: t.TempDir()}
		foreignCA, err := pki.NewCA("foreign", day)
		must(t, err)
		foreign, err := foreignCA.Issue(pki.LeafOptions{CommonName: "connector-1", Client: true, Validity: day})
		must(t, err)
		must(t, st.Save(foreign.Key, foreign.Cert, p.hubCA.Cert))
		err = enroll.Renew(ctx, dial, addr, "127.0.0.1", st)
		var he *enroll.HubError
		if err == nil || errors.As(err, &he) {
			t.Fatalf("got %v, want the TLS handshake to fail", err)
		}
	})

	if n := h.Tunnels(); n != 0 {
		t.Fatalf("refused enrollments opened %d tunnels", n)
	}
}
