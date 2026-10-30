package hub

import (
	"crypto/ed25519"
	"crypto/tls"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/Instalily/edge-connector/internal/enroll"
	"github.com/Instalily/edge-connector/internal/pki"
	"github.com/Instalily/edge-connector/internal/protocol"
)

// issuer signs connector certificates for enrollment and renewal.
type issuer struct {
	ca        *pki.Pair
	hubCA     string // fingerprint tokens must name
	tokenKeys []ed25519.PublicKey
	lifetime  time.Duration
}

func newIssuer(caCertFile string, e *Enrollment) (*issuer, error) {
	ca, err := pki.Load(caCertFile, e.CAKeyFile)
	if err != nil {
		return nil, fmt.Errorf("enrollment: connector CA: %w", err)
	}
	if !ca.Cert.IsCA {
		return nil, fmt.Errorf("enrollment: %s is not a CA certificate", caCertFile)
	}
	hubCA, err := pki.LoadCert(e.HubCAFile)
	if err != nil {
		return nil, fmt.Errorf("enrollment: hub CA: %w", err)
	}
	keys, err := enroll.LoadTokenVerifiers(e.TokenKeysFile)
	if err != nil {
		return nil, fmt.Errorf("enrollment: token keys: %w", err)
	}
	return &issuer{ca: ca, hubCA: enroll.Fingerprint(hubCA), tokenKeys: keys, lifetime: e.CertificateLifetime}, nil
}

// withEnrollment makes base serve enrollment, without a client certificate,
// and renewal, with one, to clients that ask for them by ALPN. Everything
// else gets base: the tunnel.
func withEnrollment(base *tls.Config) *tls.Config {
	enrollCfg := base.Clone()
	enrollCfg.ClientAuth = tls.NoClientCert
	enrollCfg.NextProtos = []string{enroll.ALPNEnroll}

	renewCfg := base.Clone()
	renewCfg.NextProtos = []string{enroll.ALPNRenew}

	out := base.Clone()
	out.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		switch {
		case slices.Contains(hello.SupportedProtos, enroll.ALPNEnroll):
			return enrollCfg, nil
		case slices.Contains(hello.SupportedProtos, enroll.ALPNRenew):
			return renewCfg, nil
		}
		return nil, nil
	}
	return out
}

// handleEnroll exchanges a token for a certificate. identity is empty: the
// connector has no certificate yet.
func (h *Hub) handleEnroll(conn *tls.Conn, log *slog.Logger) {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	var req enroll.Request
	if err := protocol.ReadFrame(conn, &req); err != nil {
		log.Warn("enrollment refused", "error", err)
		return
	}
	claims, err := enroll.Verify(req.Token, h.issuer.tokenKeys, time.Now())
	if err != nil {
		log.Warn("enrollment refused", "reason", err.Error())
		h.replyEnroll(conn, err.Error())
		return
	}
	log = log.With("identity", claims.Identity, "token", claims.ID)
	if claims.HubCA != h.issuer.hubCA {
		log.Warn("enrollment refused", "reason", enroll.ErrWrongHub)
		h.replyEnroll(conn, enroll.ErrWrongHub)
		return
	}
	h.issue(conn, log, claims.Identity, req.CSR, "connector enrolled")
}

// handleRenew issues a new certificate to a connector that presents its
// current one.
func (h *Hub) handleRenew(conn *tls.Conn, log *slog.Logger, identity string) {
	conn.SetDeadline(time.Now().Add(15 * time.Second))
	var req enroll.Request
	if err := protocol.ReadFrame(conn, &req); err != nil {
		log.Warn("renewal refused", "error", err)
		return
	}
	h.issue(conn, log, identity, req.CSR, "connector renewed")
}

func (h *Hub) issue(conn *tls.Conn, log *slog.Logger, identity, csr, event string) {
	if !h.identityAllowed(identity) {
		log.Warn("certificate refused", "reason", enroll.ErrIdentityNotAllowed)
		h.replyEnroll(conn, enroll.ErrIdentityNotAllowed)
		return
	}
	pub, err := enroll.ParseRequest(csr)
	if err != nil {
		log.Warn("certificate refused", "reason", enroll.ErrBadCSR, "error", err)
		h.replyEnroll(conn, enroll.ErrBadCSR)
		return
	}
	cert, err := h.issuer.ca.IssueFor(pub, pki.LeafOptions{
		CommonName: identity,
		Client:     true,
		Validity:   h.issuer.lifetime,
	})
	if err != nil {
		log.Error("certificate refused", "reason", enroll.ErrInternal, "error", err)
		h.replyEnroll(conn, enroll.ErrInternal)
		return
	}
	if err := protocol.WriteFrame(conn, enroll.Response{Certificate: string(pki.CertPEM(cert))}); err != nil {
		log.Warn("certificate not delivered", "error", err)
		return
	}
	log.Info(event, "serial", cert.SerialNumber.Text(16), "not_after", cert.NotAfter.UTC().Format(time.RFC3339))
}

func (h *Hub) replyEnroll(conn *tls.Conn, code string) {
	protocol.WriteFrame(conn, enroll.Response{Error: code})
}

func (h *Hub) identityAllowed(identity string) bool {
	ids := h.cfg.Tunnel.AllowedIdentities
	return len(ids) == 0 || slices.Contains(ids, identity)
}
