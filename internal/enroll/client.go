package enroll

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/Instalily/edge-connector/internal/protocol"
)

// Dial opens a TCP connection to the hub, directly or through a proxy.
type Dial func(ctx context.Context, addr string) (net.Conn, error)

// HubError is the hub's refusal, by code.
type HubError struct{ Code string }

func (e *HubError) Error() string { return "hub refused: " + e.Code }

// Enroll exchanges a token for a certificate and stores it in st. The hub is
// trusted only if its certificate chain includes the CA the token pins.
func Enroll(ctx context.Context, dial Dial, addr, serverName, token string, st State) error {
	claims, err := Decode(token)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	var hubCA *x509.Certificate
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: serverName,
		NextProtos: []string{ALPNEnroll},
		// Verification happens below, against the pinned CA, which no system
		// pool holds.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			ca, err := pinnedCA(cs, claims.HubCA)
			if err != nil {
				return err
			}
			hubCA = ca
			return nil
		},
	}
	return exchange(ctx, dial, addr, cfg, ALPNEnroll, token, st, func() *x509.Certificate { return hubCA })
}

// Renew replaces the stored certificate and key with new ones, using the
// current certificate to authenticate.
func Renew(ctx context.Context, dial Dial, addr, serverName string, st State) error {
	files := st.Files()
	caPEM, err := os.ReadFile(files.CAFile)
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return fmt.Errorf("no certificates in %s", files.CAFile)
	}
	cert, err := tls.LoadX509KeyPair(files.CertFile, files.KeyFile)
	if err != nil {
		return err
	}
	cfg := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		ServerName:   serverName,
		NextProtos:   []string{ALPNRenew},
		RootCAs:      pool,
		Certificates: []tls.Certificate{cert},
	}
	return exchange(ctx, dial, addr, cfg, ALPNRenew, "", st, func() *x509.Certificate { return nil })
}

func exchange(ctx context.Context, dial Dial, addr string, cfg *tls.Config, alpn, token string, st State, hubCA func() *x509.Certificate) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	raw, err := dial(ctx, addr)
	if err != nil {
		return fmt.Errorf("dial hub: %w", err)
	}
	conn := tls.Client(raw, cfg)
	defer conn.Close()
	if err := conn.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("tls handshake: %w", err)
	}
	if p := conn.ConnectionState().NegotiatedProtocol; p != alpn {
		return fmt.Errorf("hub does not offer %s (negotiated %q): is enrollment configured on it?", alpn, p)
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	}

	key, csr, err := NewRequest()
	if err != nil {
		return err
	}
	if err := protocol.WriteFrame(conn, Request{Token: token, CSR: csr}); err != nil {
		return err
	}
	var resp Response
	if err := protocol.ReadFrame(conn, &resp); err != nil {
		return fmt.Errorf("read hub response: %w", err)
	}
	if resp.Error != "" {
		return &HubError{Code: resp.Error}
	}
	cert, err := parseCertPEM(resp.Certificate)
	if err != nil {
		return err
	}
	return st.Save(key, cert, hubCA())
}

// pinnedCA finds the CA certificate with fingerprint pin among those the hub
// sent, and verifies the hub's certificate against it.
func pinnedCA(cs tls.ConnectionState, pin string) (*x509.Certificate, error) {
	if len(cs.PeerCertificates) == 0 {
		return nil, errors.New("hub sent no certificate")
	}
	for _, c := range cs.PeerCertificates[1:] {
		if !c.IsCA || Fingerprint(c) != pin {
			continue
		}
		roots := x509.NewCertPool()
		roots.AddCert(c)
		_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{
			DNSName:   cs.ServerName,
			Roots:     roots,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			return nil, fmt.Errorf("hub certificate: %w", err)
		}
		return c, nil
	}
	return nil, errors.New("hub's certificate chain does not include the CA the token names: wrong hub, or its certificate file lacks the CA")
}

func parseCertPEM(s string) (*x509.Certificate, error) {
	block, _ := pem.Decode([]byte(s))
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("hub returned no certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}
