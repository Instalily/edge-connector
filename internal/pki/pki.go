// Package pki issues the certificates a tunnel uses: one CA per customer,
// a server certificate for that customer's hub, and a client certificate for
// each of its connectors. ECDSA P-256 throughout.
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"time"
)

// Pair is a certificate and its private key.
type Pair struct {
	Cert *x509.Certificate
	Key  *ecdsa.PrivateKey
}

// LeafOptions describes a certificate to issue. Set Server for a hub and
// Client for a connector.
type LeafOptions struct {
	CommonName string
	DNSNames   []string
	IPs        []net.IP
	Server     bool
	Client     bool
	Validity   time.Duration
}

// NewCA creates a self-signed CA that can issue leaf certificates only.
func NewCA(name string, validity time.Duration) (*Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tmpl, err := template(name, validity)
	if err != nil {
		return nil, err
	}
	tmpl.IsCA = true
	tmpl.BasicConstraintsValid = true
	tmpl.MaxPathLenZero = true
	tmpl.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	return sign(tmpl, tmpl, &key.PublicKey, key, key)
}

// Issue signs a new leaf certificate, with a new key, with ca.
func (ca *Pair) Issue(o LeafOptions) (*Pair, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	cert, err := ca.IssueFor(&key.PublicKey, o)
	if err != nil {
		return nil, err
	}
	return &Pair{Cert: cert, Key: key}, nil
}

// IssueFor signs a leaf certificate for a key the caller holds, such as one
// from a certificate request.
func (ca *Pair) IssueFor(pub *ecdsa.PublicKey, o LeafOptions) (*x509.Certificate, error) {
	if !o.Server && !o.Client {
		return nil, errors.New("pki: a certificate must be for a server, a client, or both")
	}
	tmpl, err := template(o.CommonName, o.Validity)
	if err != nil {
		return nil, err
	}
	tmpl.KeyUsage = x509.KeyUsageDigitalSignature
	tmpl.DNSNames = o.DNSNames
	tmpl.IPAddresses = o.IPs
	if o.Server {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageServerAuth)
	}
	if o.Client {
		tmpl.ExtKeyUsage = append(tmpl.ExtKeyUsage, x509.ExtKeyUsageClientAuth)
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, pub, ca.Key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// CertPEM encodes a certificate as PEM.
func CertPEM(c *x509.Certificate) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})
}

// KeyPEM encodes an ECDSA key as PKCS #8 PEM.
func KeyPEM(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// LoadCert reads the first certificate in a PEM file.
func LoadCert(path string) (*x509.Certificate, error) {
	der, err := readPEM(path, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(der)
}

// Write saves the certificate and key as PEM. Only the owner can read the key.
// Any chain certificates follow the certificate in certFile, so the peer can
// see which CA issued it.
func (p *Pair) Write(certFile, keyFile string, chain ...*x509.Certificate) error {
	keyPEM, err := KeyPEM(p.Key)
	if err != nil {
		return err
	}
	certPEM := CertPEM(p.Cert)
	for _, c := range chain {
		certPEM = append(certPEM, CertPEM(c)...)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		return err
	}
	return os.WriteFile(certFile, certPEM, 0o644)
}

// Load reads a pair written by Write.
func Load(certFile, keyFile string) (*Pair, error) {
	cert, err := readPEM(certFile, "CERTIFICATE")
	if err != nil {
		return nil, err
	}
	c, err := x509.ParseCertificate(cert)
	if err != nil {
		return nil, err
	}
	keyDER, err := readPEM(keyFile, "PRIVATE KEY")
	if err != nil {
		return nil, err
	}
	k, err := x509.ParsePKCS8PrivateKey(keyDER)
	if err != nil {
		return nil, err
	}
	key, ok := k.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("pki: %s is not an ECDSA key", keyFile)
	}
	return &Pair{Cert: c, Key: key}, nil
}

func template(cn string, validity time.Duration) (*x509.Certificate, error) {
	if cn == "" {
		return nil, errors.New("pki: a common name is required")
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	return &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn},
		// Tolerate clock skew between the issuing host and the peers, by no
		// more than a tenth of the validity, so renewal timing based on
		// NotBefore stays close to the issue time.
		NotBefore: now.Add(-min(5*time.Minute, validity/10)),
		NotAfter:  now.Add(validity),
	}, nil
}

func sign(tmpl, parent *x509.Certificate, pub *ecdsa.PublicKey, signer, key *ecdsa.PrivateKey) (*Pair, error) {
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, pub, signer)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &Pair{Cert: cert, Key: key}, nil
}

func readPEM(path, typ string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != typ {
		return nil, fmt.Errorf("pki: %s holds no %s block", path, typ)
	}
	return block.Bytes, nil
}
