package connector

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"time"

	"github.com/Instalily/edge-connector/internal/enroll"
	"github.com/Instalily/edge-connector/internal/proxy"
)

func (c *Connector) state() enroll.State { return enroll.State{Dir: c.cfg.StateDir} }

func (c *Connector) dialHub(ctx context.Context, addr string) (net.Conn, error) {
	d := &net.Dialer{KeepAlive: 30 * time.Second}
	return proxy.Dial(ctx, d, c.proxy, addr)
}

// enrollUntilDone returns once the connector holds a valid certificate,
// enrolling with the token if it must, or false when ctx ends first.
func (c *Connector) enrollUntilDone(ctx context.Context) bool {
	const maxBackoff = time.Minute
	backoff := time.Second
	for {
		err := c.ensureCertificate(ctx)
		if err == nil {
			return true
		}
		wait := backoff/2 + rand.N(backoff/2)
		c.log.Error("not enrolled", "error", err, "retry_in", wait.Round(time.Millisecond).String())
		select {
		case <-ctx.Done():
			return false
		case <-time.After(wait):
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// ensureCertificate enrolls unless the state directory already holds a
// certificate with a usable part of its lifetime left: a tenth of it, or a
// minute if that is less.
func (c *Connector) ensureCertificate(ctx context.Context) error {
	st := c.state()
	cert, err := st.Certificate()
	switch {
	case err == nil && time.Until(cert.NotAfter) > min(time.Minute, cert.NotAfter.Sub(cert.NotBefore)/10):
		c.log.Info("using enrolled certificate", "identity", cert.Subject.CommonName, "not_after", cert.NotAfter.UTC().Format(time.RFC3339))
		return nil
	case err == nil:
		c.log.Warn("enrolled certificate has expired; enrolling again", "not_after", cert.NotAfter.UTC().Format(time.RFC3339))
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	if err := st.CheckWritable(); err != nil {
		return err
	}
	if c.cfg.Enrollment.TokenFile == "" {
		return errors.New("no valid certificate in state_dir and no enrollment.token_file: get a new token from Instalily")
	}
	data, err := os.ReadFile(c.cfg.Enrollment.TokenFile)
	if err != nil {
		return err
	}
	if err := enroll.Enroll(ctx, c.dialHub, c.cfg.Hub.Address, c.cfg.Hub.ServerName, strings.TrimSpace(string(data)), st); err != nil {
		return err
	}
	cert, err = st.Certificate()
	if err != nil {
		return err
	}
	c.log.Info("enrolled", "identity", cert.Subject.CommonName, "not_after", cert.NotAfter.UTC().Format(time.RFC3339))
	return nil
}

// renewLoop renews the certificate once two thirds of its lifetime has
// passed. Tunnels pick up the new certificate when they next connect, which
// the hub forces when the old one expires.
//
// A failure is retried with exponential backoff and jitter, as maintain
// retries a tunnel. Without it, a connector whose renewals the hub refuses
// (one removed from allowed_identities, say) would open a new connection to
// the hub as fast as each refusal came back. No failure ends the loop: a
// connector that stopped renewing would lose its tunnels when its
// certificate expired, and stay down until restarted.
func (c *Connector) renewLoop(ctx context.Context) {
	const minBackoff, maxBackoff = time.Second, time.Minute
	st := c.state()
	backoff := minBackoff
	for {
		wait, err := c.renewIfDue(ctx, st)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			wait = backoff/2 + rand.N(backoff/2)
			c.log.Error("renewal failed", "error", err, "retry_in", wait.Round(time.Millisecond).String())
			backoff = min(backoff*2, maxBackoff)
		} else {
			backoff = minBackoff
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// renewIfDue renews the certificate if two thirds of its lifetime has passed,
// and returns how long to wait before looking again.
func (c *Connector) renewIfDue(ctx context.Context, st enroll.State) (time.Duration, error) {
	cert, err := st.Certificate()
	if err != nil {
		return 0, fmt.Errorf("read enrolled certificate: %w", err)
	}
	lifetime := cert.NotAfter.Sub(cert.NotBefore)
	// Look at least every minute, and often enough for short lifetimes.
	interval := min(time.Minute, max(lifetime/20, time.Second))
	renewAt := cert.NotBefore.Add(lifetime * 2 / 3)
	if until := time.Until(renewAt); until > 0 {
		return min(until, interval), nil
	}
	if err := enroll.Renew(ctx, c.dialHub, c.cfg.Hub.Address, c.cfg.Hub.ServerName, st); err != nil {
		notAfter := cert.NotAfter.UTC().Format(time.RFC3339)
		if time.Now().Before(cert.NotAfter) {
			return 0, fmt.Errorf("certificate expires %s: %w", notAfter, err)
		}
		// Too late to renew: enroll again, if a token is still there.
		if eerr := c.ensureCertificate(ctx); eerr != nil {
			return 0, fmt.Errorf("certificate expired %s: %w; enrolling again: %w", notAfter, err, eerr)
		}
		return interval, nil
	}
	if next, err := st.Certificate(); err == nil {
		c.log.Info("renewed", "identity", next.Subject.CommonName, "not_after", next.NotAfter.UTC().Format(time.RFC3339))
	}
	// Not straight back, even after a success: to a clock far ahead of the
	// hub's, every new certificate looks due at once.
	return interval, nil
}
