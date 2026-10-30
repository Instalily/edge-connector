package enroll

import (
	"crypto/ecdsa"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Instalily/edge-connector/internal/pki"
	"github.com/Instalily/edge-connector/internal/tlsconfig"
)

// State is the connector's directory of enrollment results:
//
//	identity.pem  its certificate and key, together, so they are always
//	              replaced at once
//	hub-ca.crt    the CA that issued the hub's certificate, pinned at
//	              enrollment
//
// It must persist across restarts: with it, a connector reconnects and
// renews without a token.
type State struct {
	Dir string
}

func (s State) identity() string { return filepath.Join(s.Dir, "identity.pem") }
func (s State) hubCA() string    { return filepath.Join(s.Dir, "hub-ca.crt") }

// Files are the TLS files the tunnel uses.
func (s State) Files() tlsconfig.Files {
	return tlsconfig.Files{CAFile: s.hubCA(), CertFile: s.identity(), KeyFile: s.identity()}
}

// Certificate returns the stored certificate, or an error wrapping
// os.ErrNotExist when the connector has never enrolled.
func (s State) Certificate() (*x509.Certificate, error) {
	if _, err := os.Stat(s.hubCA()); err != nil {
		return nil, err
	}
	return pki.LoadCert(s.identity())
}

// CheckWritable fails unless the connector can save into the directory, so it
// does not spend a token on a certificate it would then lose.
func (s State) CheckWritable() error {
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return fmt.Errorf("state_dir: %w", err)
	}
	f, err := os.CreateTemp(s.Dir, ".write-check.*")
	if err != nil {
		return fmt.Errorf("state_dir %s is not writable (mount a persistent volume there): %w", s.Dir, err)
	}
	f.Close()
	return os.Remove(f.Name())
}

// Save stores a certificate and its key, replacing the previous pair in one
// rename, and, when hubCA is not nil, the hub's CA.
func (s State) Save(key *ecdsa.PrivateKey, cert *x509.Certificate, hubCA *x509.Certificate) error {
	if !cert.PublicKey.(*ecdsa.PublicKey).Equal(&key.PublicKey) {
		return errors.New("certificate does not match the key")
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if hubCA != nil {
		if err := writeAtomic(s.hubCA(), pki.CertPEM(hubCA), 0o644); err != nil {
			return err
		}
	}
	keyPEM, err := pki.KeyPEM(key)
	if err != nil {
		return err
	}
	return writeAtomic(s.identity(), append(pki.CertPEM(cert), keyPEM...), 0o600)
}

func writeAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("save %s: %w", path, err)
	}
	return nil
}
