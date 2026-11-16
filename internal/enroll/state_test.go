package enroll

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"os"
	"testing"
	"time"

	"github.com/Instalily/edge-connector/internal/pki"
)

// The certificate comes from the hub, so Save must refuse one that isn't for
// the connector's key, whatever kind of key it is for, and keep nothing.
func TestSaveRefusesACertificateForAnotherKey(t *testing.T) {
	key, _, err := NewRequest()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := pki.NewCA("test CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	other, err := ca.Issue(pki.LeafOptions{CommonName: "connector-1", Client: true, Validity: time.Hour})
	if err != nil {
		t.Fatal(err)
	}

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "connector-1"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	notECDSA, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}

	for name, cert := range map[string]*x509.Certificate{
		"another ECDSA key": other.Cert,
		"an Ed25519 key":    notECDSA,
	} {
		t.Run(name, func(t *testing.T) {
			st := State{Dir: t.TempDir()}
			if err := st.Save(key, cert, ca.Cert); err == nil {
				t.Fatal("saved a certificate that does not match the key")
			}
			entries, err := os.ReadDir(st.Dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("a refused certificate left %d files in the state directory", len(entries))
			}
		})
	}
}
