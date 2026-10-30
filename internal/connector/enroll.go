package connector

import (
	"context"
	"errors"
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
func (c *Connector) renewLoop(ctx context.Context) {
	st := c.state()
	for {
		cert, err := st.Certificate()
		if err != nil {
			c.log.Error("cannot read enrolled certificate", "error", err)
			return
		}
		lifetime := cert.NotAfter.Sub(cert.NotBefore)
		renewAt := cert.NotBefore.Add(lifetime * 2 / 3)
		// Check at least every minute, and often enough for short lifetimes.
		wait := min(max(time.Until(renewAt), 0), time.Minute, max(lifetime/20, time.Second))
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if time.Now().Before(renewAt) {
			continue
		}
		if err := enroll.Renew(ctx, c.dialHub, c.cfg.Hub.Address, c.cfg.Hub.ServerName, st); err != nil {
			c.log.Error("renewal failed", "error", err, "not_after", cert.NotAfter.UTC().Format(time.RFC3339))
			if time.Now().After(cert.NotAfter) {
				// Too late to renew: enroll again, if a token is still there.
				if err := c.ensureCertificate(ctx); err != nil {
					c.log.Error("re-enrollment failed", "error", err)
				}
			}
			continue
		}
		next, _ := st.Certificate()
		if next != nil {
			c.log.Info("renewed", "identity", next.Subject.CommonName, "not_after", next.NotAfter.UTC().Format(time.RFC3339))
		}
	}
}
