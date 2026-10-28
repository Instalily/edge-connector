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
	Endpoints []Endpoint `yaml:"endpoints"`
	// OpenTimeout bounds how long opening a stream, including the
	// connector's dial, may take.
	OpenTimeout time.Duration `yaml:"open_timeout"`
	// HealthListen is where /healthz and /readyz are served. Empty disables
	// them.
	HealthListen string `yaml:"health_listen"`
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
