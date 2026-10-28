package connector

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Instalily/edge-connector/internal/allowlist"
	"github.com/Instalily/edge-connector/internal/tlsconfig"
	"github.com/Instalily/edge-connector/internal/yamlfile"
)

// Config is the connector's configuration file. The customer owns it.
type Config struct {
	Hub struct {
		// Address is the hub's host:port.
		Address string `yaml:"address"`
		// ServerName is the name to verify on the hub's certificate. It
		// defaults to Address's host.
		ServerName string `yaml:"server_name"`
		// Connections is how many tunnels to keep open, for redundancy.
		Connections int `yaml:"connections"`
	} `yaml:"hub"`
	TLS tlsconfig.Files `yaml:"tls"`
	// Proxy is an http:// or https:// proxy URL. Empty means HTTPS_PROXY and
	// NO_PROXY decide.
	Proxy  string            `yaml:"proxy"`
	Allow  []allowlist.Entry `yaml:"allow"`
	Limits struct {
		MaxStreams  int           `yaml:"max_streams"`
		DialTimeout time.Duration `yaml:"dial_timeout"`
	} `yaml:"limits"`
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
	host, _, err := net.SplitHostPort(c.Hub.Address)
	if err != nil {
		return fmt.Errorf("hub.address: %w", err)
	}
	if c.Hub.ServerName == "" {
		c.Hub.ServerName = host
	}
	if c.Hub.Connections == 0 {
		c.Hub.Connections = 2
	}
	if c.Hub.Connections < 1 || c.Hub.Connections > 8 {
		return errors.New("hub.connections must be between 1 and 8")
	}
	if c.Limits.MaxStreams == 0 {
		c.Limits.MaxStreams = 256
	}
	if c.Limits.MaxStreams < 1 {
		return errors.New("limits.max_streams must be positive")
	}
	if c.Limits.DialTimeout == 0 {
		c.Limits.DialTimeout = 10 * time.Second
	}
	if c.Limits.DialTimeout < 0 {
		return errors.New("limits.dial_timeout must be positive")
	}
	return nil
}
