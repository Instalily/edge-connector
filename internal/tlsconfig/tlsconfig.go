// Package tlsconfig builds the TLS configurations for both ends of a tunnel:
// TLS 1.3 only, certificates on both sides, and the protocol's ALPNs.
package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"

	"github.com/Instalily/edge-connector/internal/protocol"
)

// Files names the PEM files for one end of a tunnel. CAFile is the CA that
// issued the other end's certificate.
type Files struct {
	CAFile   string `yaml:"ca_file"`
	CertFile string `yaml:"cert_file"`
	KeyFile  string `yaml:"key_file"`
}

// Client returns the connector's TLS configuration for a hub named serverName.
func Client(f Files, serverName string) (*tls.Config, error) {
	pool, err := f.load()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: serverName,
		NextProtos: protocol.ALPNs,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return f.keyPair()
		},
	}, nil
}

// Server returns the hub's TLS configuration. It accepts only connectors
// whose certificates CAFile issued.
func Server(f Files) (*tls.Config, error) {
	pool, err := f.load()
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		NextProtos: protocol.ALPNs,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return f.keyPair()
		},
	}, nil
}

// load checks the key pair and returns the CA pool. The key pair is read
// again on every handshake, so a rotated certificate takes effect on the
// next connection without a restart.
func (f Files) load() (*x509.CertPool, error) {
	if f.CAFile == "" || f.CertFile == "" || f.KeyFile == "" {
		return nil, errors.New("tls: ca_file, cert_file and key_file are all required")
	}
	if _, err := f.keyPair(); err != nil {
		return nil, err
	}
	pem, err := os.ReadFile(f.CAFile)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("tls: no certificates in %s", f.CAFile)
	}
	return pool, nil
}

func (f Files) keyPair() (*tls.Certificate, error) {
	cert, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("tls: %w", err)
	}
	return &cert, nil
}
