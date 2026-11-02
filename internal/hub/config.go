package hub

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Instalily/edge-connector/internal/tlsconfig"
	"github.com/Instalily/edge-connector/internal/yamlfile"
)

// Endpoint exposes one target in the customer network as a local TCP
// listener. Our services connect to Listen; the connector dials Target.
type Endpoint struct {
	Name   string `yaml:"name"`
	Listen string `yaml:"listen"`
	Target string `yaml:"target"`
}

// Config is a hub's configuration. One hub serves one customer.
type Config struct {
	Tunnel struct {
		// Listen is where connectors dial in.
		Listen string `yaml:"listen"`
		// TLS holds the hub's server certificate and the customer CA that
		// issues its connectors' certificates.
		TLS tlsconfig.Files `yaml:"tls"`
		// AllowedIdentities, when set, limits connectors to these
		// certificate common names.
		AllowedIdentities []string `yaml:"allowed_identities"`
	} `yaml:"tunnel"`
	// Enrollment, when set, lets connectors exchange a token for a
	// certificate, and renew it, on the tunnel port.
	Enrollment *Enrollment `yaml:"enrollment"`
	Endpoints  []Endpoint  `yaml:"endpoints"`
	// OpenTimeout bounds how long opening a stream, including the
	// connector's dial, may take.
	OpenTimeout time.Duration `yaml:"open_timeout"`
	// HealthListen is where /healthz and /readyz are served. Empty disables
	// them.
	HealthListen string `yaml:"health_listen"`
	// ShutdownGrace is how long the hub keeps serving after SIGTERM, with
	// both health checks failing, before it stops. It must outlast the load
	// balancers' health checks, so they stop sending it traffic first.
	ShutdownGrace time.Duration `yaml:"shutdown_grace"`
}

// Enrollment configures how the hub issues connector certificates. The
// connector CA is tunnel.tls.ca_file; the hub signs with its key.
type Enrollment struct {
	// CAKeyFile is the private key of tunnel.tls.ca_file.
	CAKeyFile string `yaml:"ca_key_file"`
	// HubCAFile is the CA that issued this hub's certificate. Tokens must
	// name its fingerprint, and tunnel.tls.cert_file must include it after
	// the hub's certificate so connectors can pin it.
	HubCAFile string `yaml:"hub_ca_file"`
	// TokenKeysFile holds the Ed25519 public keys tokens may be signed with.
	TokenKeysFile string `yaml:"token_keys_file"`
	// CertificateLifetime is how long issued certificates last. Connectors
	// renew at two thirds of it.
	CertificateLifetime time.Duration `yaml:"certificate_lifetime"`
}

// Load reads and checks the configuration file at path.
func Load(path string) (*Config, error) {
	var c Config
	if err := yamlfile.Decode(path, &c); err != nil {
		return nil, err
	}
	return &c, c.normalize()
}

func (c *Config) normalize() error {
	if c.Tunnel.Listen == "" {
		return errors.New("tunnel.listen is required")
	}
	if c.OpenTimeout == 0 {
		c.OpenTimeout = 15 * time.Second
	}
	if c.OpenTimeout < 0 {
		return errors.New("open_timeout must be positive")
	}
	if c.ShutdownGrace == 0 {
		c.ShutdownGrace = 15 * time.Second
	}
	if c.ShutdownGrace < 0 || c.ShutdownGrace > 5*time.Minute {
		return errors.New("shutdown_grace must be positive and at most 5m")
	}
	if e := c.Enrollment; e != nil {
		if e.CAKeyFile == "" || e.HubCAFile == "" || e.TokenKeysFile == "" {
			return errors.New("enrollment: ca_key_file, hub_ca_file and token_keys_file are all required")
		}
		if e.CertificateLifetime == 0 {
			e.CertificateLifetime = 7 * 24 * time.Hour
		}
		if e.CertificateLifetime < 0 || e.CertificateLifetime > 30*24*time.Hour {
			return errors.New("enrollment.certificate_lifetime must be positive and at most 720h")
		}
	}
	if len(c.Endpoints) == 0 {
		return errors.New("endpoints: at least one is required")
	}
	names := map[string]bool{}
	// Port 0 asks the system for a free port, so only fixed ports can clash.
	listens := map[string]bool{}
	claim := func(addr string) {
		if _, port, _ := net.SplitHostPort(addr); port != "0" {
			listens[addr] = true
		}
	}
	claim(c.Tunnel.Listen)
	for i, ep := range c.Endpoints {
		switch {
		case ep.Name == "":
			return fmt.Errorf("endpoints[%d]: name is required", i)
		case names[ep.Name]:
			return fmt.Errorf("endpoints[%d]: duplicate name %q", i, ep.Name)
		case ep.Listen == "":
			return fmt.Errorf("endpoint %s: listen is required", ep.Name)
		case listens[ep.Listen]:
			return fmt.Errorf("endpoint %s: listen address %s is already used", ep.Name, ep.Listen)
		}
		if _, _, err := net.SplitHostPort(ep.Target); err != nil {
			return fmt.Errorf("endpoint %s: target: %w", ep.Name, err)
		}
		names[ep.Name] = true
		claim(ep.Listen)
	}
	return nil
}
