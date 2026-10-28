// Package proxy reaches the hub through a corporate HTTP proxy with CONNECT,
// which many customer networks require for outbound traffic.
package proxy

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"time"
)

// FromEnvironment returns the proxy that HTTPS_PROXY and NO_PROXY select
// for addr, or nil for a direct connection.
func FromEnvironment(addr string) (*url.URL, error) {
	return http.ProxyFromEnvironment(&http.Request{URL: &url.URL{Scheme: "https", Host: addr}})
}

// Dial connects to addr, through proxyURL when it is not nil. The proxy may
// be http:// or https://, with basic credentials in the URL.
func Dial(ctx context.Context, d *net.Dialer, proxyURL *url.URL, addr string) (net.Conn, error) {
	if proxyURL == nil {
		return d.DialContext(ctx, "tcp", addr)
	}
	if proxyURL.Scheme != "http" && proxyURL.Scheme != "https" {
		return nil, fmt.Errorf("proxy: unsupported scheme %q", proxyURL.Scheme)
	}
	proxyAddr := proxyURL.Host
	if proxyURL.Port() == "" {
		proxyAddr = net.JoinHostPort(proxyURL.Hostname(), map[string]string{"http": "80", "https": "443"}[proxyURL.Scheme])
	}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy: %w", err)
	}
	if proxyURL.Scheme == "https" {
		tc := tls.Client(conn, &tls.Config{ServerName: proxyURL.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			conn.Close()
			return nil, fmt.Errorf("proxy: %w", err)
		}
		conn = tc
	}
	if err := connect(ctx, conn, proxyURL, addr); err != nil {
		conn.Close()
		return nil, err
	}
	return conn, nil
}

func connect(ctx context.Context, conn net.Conn, proxyURL *url.URL, addr string) error {
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
		defer conn.SetDeadline(time.Time{})
	}
	req := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: addr},
		Host:   addr,
		Header: make(http.Header),
	}
	if u := proxyURL.User; u != nil {
		pw, _ := u.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)))
	}
	if err := req.Write(conn); err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return fmt.Errorf("proxy: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("proxy: CONNECT %s: %s", addr, resp.Status)
	}
	// The hub speaks only after our TLS ClientHello, so nothing may follow
	// the proxy's response yet.
	if br.Buffered() > 0 {
		return errors.New("proxy: unexpected data after CONNECT response")
	}
	return nil
}
