// Package enroll lets a connector get its own certificate. An operator gives
// the customer a token; the connector generates a key, which never leaves
// it, and sends the hub the token and a certificate request. The hub checks
// the token and returns a short-lived client certificate, which the
// connector renews with the hub before it expires.
//
// A token is "edge1.<payload>.<signature>": base64url JSON claims and an
// Ed25519 signature over "edge1.<payload>". The signing key stays in
// Instalily's secret store; hubs hold only public keys. The claims pin the
// hub's CA by fingerprint, so a token works only at the hub it names, and
// the connector can authenticate that hub before it has any other trust
// material.
package enroll

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

const tokenPrefix = "edge1"

// MaxTokenLifetime bounds how long a token may be valid. Tokens are reusable
// until they expire, so that two replicas can enroll with one, and must stay
// short-lived for that reason.
const MaxTokenLifetime = 7 * 24 * time.Hour

// Clock skew tolerated between the operator who signs and the hub.
const maxSkew = 5 * time.Minute

var identityPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,62}$`)

// Claims are what a token asserts.
type Claims struct {
	// Identity becomes the connector certificate's common name.
	Identity string `json:"sub"`
	// HubCA is Fingerprint of the CA that issued the hub's certificate.
	HubCA    string `json:"hub_ca"`
	IssuedAt int64  `json:"iat"`
	Expires  int64  `json:"exp"`
	// ID names the token in the hub's logs.
	ID string `json:"jti"`
}

// Token errors. A hub returns their codes to the connector.
var (
	ErrInvalidToken = errors.New("invalid_token")
	ErrExpiredToken = errors.New("expired_token")
)

// Fingerprint identifies a certificate: "sha256:" and the hex SHA-256 of its
// DER encoding.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// NewClaims returns claims for identity at the hub whose CA is hubCA, valid
// from now for lifetime.
func NewClaims(identity string, hubCA *x509.Certificate, now time.Time, lifetime time.Duration) (Claims, error) {
	id := make([]byte, 12)
	if _, err := rand.Read(id); err != nil {
		return Claims{}, err
	}
	c := Claims{
		Identity: identity,
		HubCA:    Fingerprint(hubCA),
		IssuedAt: now.Unix(),
		Expires:  now.Add(lifetime).Unix(),
		ID:       hex.EncodeToString(id),
	}
	return c, c.check()
}

func (c Claims) check() error {
	switch {
	case !identityPattern.MatchString(c.Identity):
		return fmt.Errorf("identity %q must be lowercase letters, digits, dots and hyphens", c.Identity)
	case !strings.HasPrefix(c.HubCA, "sha256:") || len(c.HubCA) != len("sha256:")+64:
		return errors.New("hub_ca must be a sha256 fingerprint")
	case c.Expires <= c.IssuedAt:
		return errors.New("a token must expire after it is issued")
	case time.Duration(c.Expires-c.IssuedAt)*time.Second > MaxTokenLifetime:
		return fmt.Errorf("a token may be valid for at most %s", MaxTokenLifetime)
	case c.ID == "":
		return errors.New("a token needs an id")
	}
	return nil
}

// Sign returns the token for c.
func Sign(key ed25519.PrivateKey, c Claims) (string, error) {
	if err := c.check(); err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	signed := tokenPrefix + "." + base64.RawURLEncoding.EncodeToString(payload)
	return signed + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, []byte(signed))), nil
}

// Decode returns a token's claims without checking its signature. The
// connector uses it only to learn which hub CA to trust; the hub verifies.
func Decode(token string) (Claims, error) {
	c, _, _, err := split(token)
	return c, err
}

// Verify checks a token's signature against keys and its validity at now.
func Verify(token string, keys []ed25519.PublicKey, now time.Time) (Claims, error) {
	c, signed, sig, err := split(token)
	if err != nil {
		return Claims{}, err
	}
	valid := false
	for _, k := range keys {
		if ed25519.Verify(k, []byte(signed), sig) {
			valid = true
			break
		}
	}
	if !valid || c.check() != nil {
		return Claims{}, ErrInvalidToken
	}
	if now.Unix() >= c.Expires {
		return Claims{}, ErrExpiredToken
	}
	if time.Unix(c.IssuedAt, 0).After(now.Add(maxSkew)) {
		return Claims{}, ErrInvalidToken
	}
	return c, nil
}

func split(token string) (Claims, string, []byte, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 || parts[0] != tokenPrefix {
		return Claims{}, "", nil, ErrInvalidToken
	}
	payload, err1 := base64.RawURLEncoding.DecodeString(parts[1])
	sig, err2 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || len(sig) != ed25519.SignatureSize {
		return Claims{}, "", nil, ErrInvalidToken
	}
	var c Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Claims{}, "", nil, ErrInvalidToken
	}
	return c, parts[0] + "." + parts[1], sig, nil
}

// GenerateTokenKey creates a token signing key and writes it, and its public
// half, as PEM. Only the owner can read the private key.
func GenerateTokenKey(privFile, pubFile string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return err
	}
	pubDER, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return err
	}
	if err := os.WriteFile(privFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privDER}), 0o600); err != nil {
		return err
	}
	return os.WriteFile(pubFile, pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: pubDER}), 0o644)
}

// LoadTokenSigner reads a private key written by GenerateTokenKey.
func LoadTokenSigner(path string) (ed25519.PrivateKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, fmt.Errorf("%s holds no PRIVATE KEY block", path)
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	priv, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s is not an Ed25519 key", path)
	}
	return priv, nil
}

// LoadTokenVerifiers reads every Ed25519 public key in a PEM file. Listing
// more than one lets the signing key rotate without a gap.
func LoadTokenVerifiers(path string) ([]ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var keys []ed25519.PublicKey
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "PUBLIC KEY" {
			continue
		}
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		pub, ok := k.(ed25519.PublicKey)
		if !ok {
			return nil, fmt.Errorf("%s holds a public key that is not Ed25519", path)
		}
		keys = append(keys, pub)
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%s holds no PUBLIC KEY block", path)
	}
	return keys, nil
}
