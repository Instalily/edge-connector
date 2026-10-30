package enroll

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Instalily/edge-connector/internal/pki"
)

func testKeys(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func testClaims(t *testing.T, now time.Time, lifetime time.Duration) Claims {
	t.Helper()
	ca, err := pki.NewCA("hub CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewClaims("connector-1", ca.Cert, now, lifetime)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestTokenRoundTrip(t *testing.T) {
	pub, priv := testKeys(t)
	now := time.Now()
	c := testClaims(t, now, time.Hour)
	tok, err := Sign(priv, c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(tok, "edge1.") {
		t.Fatalf("token %q lacks its prefix", tok)
	}
	got, err := Verify(tok, []ed25519.PublicKey{pub}, now)
	if err != nil {
		t.Fatal(err)
	}
	if got != c {
		t.Fatalf("got %+v, want %+v", got, c)
	}
	if d, _ := Decode(tok); d != c {
		t.Fatalf("Decode got %+v", d)
	}
}

func TestVerifyRejects(t *testing.T) {
	pub, priv := testKeys(t)
	otherPub, _ := testKeys(t)
	now := time.Now()
	tok, err := Sign(priv, testClaims(t, now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(tok, ".")
	future, _ := Sign(priv, testClaims(t, now.Add(time.Hour), time.Hour))

	tests := []struct {
		name  string
		token string
		keys  []ed25519.PublicKey
		now   time.Time
		want  error
	}{
		{"wrong key", tok, []ed25519.PublicKey{otherPub}, now, ErrInvalidToken},
		{"second key verifies", tok, []ed25519.PublicKey{otherPub, pub}, now, nil},
		{"expired", tok, []ed25519.PublicKey{pub}, now.Add(time.Hour), ErrExpiredToken},
		{"not yet issued", future, []ed25519.PublicKey{pub}, now, ErrInvalidToken},
		{"tampered payload", parts[0] + "." + parts[1] + "x." + parts[2], []ed25519.PublicKey{pub}, now, ErrInvalidToken},
		{"wrong prefix", "edge2." + parts[1] + "." + parts[2], []ed25519.PublicKey{pub}, now, ErrInvalidToken},
		{"garbage", "not a token", []ed25519.PublicKey{pub}, now, ErrInvalidToken},
	}
	for _, tt := range tests {
		_, err := Verify(tt.token, tt.keys, tt.now)
		if !errors.Is(err, tt.want) {
			t.Errorf("%s: got %v, want %v", tt.name, err, tt.want)
		}
	}
}

func TestClaimsLimits(t *testing.T) {
	ca, err := pki.NewCA("hub CA", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewClaims("connector-1", ca.Cert, time.Now(), MaxTokenLifetime+time.Hour); err == nil {
		t.Error("a token valid for longer than the maximum was accepted")
	}
	if _, err := NewClaims("Connector One", ca.Cert, time.Now(), time.Hour); err == nil {
		t.Error("an identity with spaces and capitals was accepted")
	}
}

func TestTokenKeyFiles(t *testing.T) {
	dir := t.TempDir()
	privFile, pubFile := filepath.Join(dir, "token.key"), filepath.Join(dir, "token.pub")
	if err := GenerateTokenKey(privFile, pubFile); err != nil {
		t.Fatal(err)
	}
	priv, err := LoadTokenSigner(privFile)
	if err != nil {
		t.Fatal(err)
	}
	pubs, err := LoadTokenVerifiers(pubFile)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tok, err := Sign(priv, testClaims(t, now, time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(tok, pubs, now); err != nil {
		t.Fatal(err)
	}
}
