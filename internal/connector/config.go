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
		// RotateAfter is roughly how long a tunnel lasts before the connector
		// replaces it, opening the new one before letting the old one drain.
		// Rotation spreads tunnels across a hub's instances, so a new
		// instance gets tunnels before an old one stops.
		RotateAfter time.Duration `yaml:"rotate_after"`
	} `yaml:"hub"`
	// TLS holds certificate files issued by hand. Leave it empty to enroll
	// instead.
	TLS tlsconfig.Files `yaml:"tls"`
	// StateDir is where an enrolled connector keeps its key, certificate and
	// the hub's CA. It must persist across restarts.
	StateDir   string `yaml:"state_dir"`
	Enrollment struct {
		// TokenFile holds the enrollment token. It is read only while the
		// connector has no valid certificate, and may be removed after.
		TokenFile string `yaml:"token_file"`
	} `yaml:"enrollment"`
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

// Enrolls reports whether the connector gets its certificate by enrollment.
func (c *Config) Enrolls() bool { return c.StateDir != "" }

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
	static := c.TLS != (tlsconfig.Files{})
	switch {
	case static && (c.StateDir != "" || c.Enrollment.TokenFile != ""):
		return errors.New("set tls for certificates issued by hand, or state_dir and enrollment to enroll, not both")
	case !static && c.StateDir == "":
		return errors.New("set tls, or state_dir (with enrollment.token_file for the first start) to enroll")
	}
	if c.Hub.Connections == 0 {
		c.Hub.Connections = 2
	}
	if c.Hub.Connections < 1 || c.Hub.Connections > 8 {
		return errors.New("hub.connections must be between 1 and 8")
	}
	if c.Hub.RotateAfter == 0 {
		c.Hub.RotateAfter = 5 * time.Minute
	}
	if c.Hub.RotateAfter < time.Second || c.Hub.RotateAfter > 24*time.Hour {
		return errors.New("hub.rotate_after must be between 1s and 24h")
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
