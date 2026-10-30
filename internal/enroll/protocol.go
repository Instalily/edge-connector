package enroll

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"errors"
)

// ALPN protocols on the hub's tunnel port. Enrollment needs no client
// certificate; renewal needs the current one.
const (
	ALPNEnroll = "edge-enroll/1"
	ALPNRenew  = "edge-renew/1"
)

// Request is the connector's one message: a certificate request, and for
// enrollment the token.
type Request struct {
	Token string `json:"token,omitempty"`
	// CSR is the base64 DER certificate request.
	CSR string `json:"csr"`
}

// Response is the hub's one message: the PEM certificate, or an error code.
type Response struct {
	Certificate string `json:"certificate,omitempty"`
	Error       string `json:"error,omitempty"`
}

// Error codes in a Response beyond the token errors.
const (
	ErrWrongHub           = "wrong_hub"
	ErrIdentityNotAllowed = "identity_not_allowed"
	ErrBadCSR             = "bad_csr"
	ErrInternal           = "internal"
)

// NewRequest generates a P-256 key and a certificate request for it. The
// hub sets the certificate's name, so the request names nothing.
func NewRequest() (*ecdsa.PrivateKey, string, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, "", err
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return nil, "", err
	}
	return key, base64.StdEncoding.EncodeToString(der), nil
}

// ParseRequest checks a certificate request's signature and key, and
// returns its public key.
func ParseRequest(csr string) (*ecdsa.PublicKey, error) {
	der, err := base64.StdEncoding.DecodeString(csr)
	if err != nil {
		return nil, err
	}
	req, err := x509.ParseCertificateRequest(der)
	if err != nil {
		return nil, err
	}
	if err := req.CheckSignature(); err != nil {
		return nil, err
	}
	pub, ok := req.PublicKey.(*ecdsa.PublicKey)
	if !ok || pub.Curve != elliptic.P256() {
		return nil, errors.New("certificate request key must be ECDSA P-256")
	}
	return pub, nil
}
