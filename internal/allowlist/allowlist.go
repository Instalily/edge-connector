// Package allowlist decides which targets a connector may dial. The list is
// the customer's: it lives in the connector's local configuration, and
// nothing the hub sends can change it.
package allowlist

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// Entry is one allowlist rule as configured: a hostname, or a CIDR (a bare
// IP address means just that address), and the ports allowed on it.
//
// A hostname is matched exactly, case-insensitively. "*.corp.local" matches
// any name ending in ".corp.local", but not "corp.local" itself. A target
// that matches no hostname rule is resolved, and only the addresses that fall
// inside a CIDR rule for that port are dialed.
type Entry struct {
	Host  string   `yaml:"host,omitempty"`
	CIDR  string   `yaml:"cidr,omitempty"`
	Ports []uint16 `yaml:"ports"`
}

// ErrDenied means no rule allows the target.
var ErrDenied = errors.New("target not allowed")

// Resolver looks up a hostname's addresses. *net.Resolver satisfies it.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

type rule struct {
	name   string // exact hostname; empty for a CIDR rule
	suffix string // ".corp.local" for "*.corp.local"
	prefix netip.Prefix
	ports  map[uint16]bool
}

// List is a parsed allowlist.
type List struct {
	rules    []rule
	resolver Resolver
}

// New parses entries. It refuses an empty list, which would allow nothing.
func New(entries []Entry, r Resolver) (*List, error) {
	if len(entries) == 0 {
		return nil, errors.New("allowlist: no entries")
	}
	l := &List{resolver: r}
	for i, e := range entries {
		ru, err := parse(e)
		if err != nil {
			return nil, fmt.Errorf("allowlist entry %d: %w", i+1, err)
		}
		l.rules = append(l.rules, ru)
	}
	return l, nil
}

func parse(e Entry) (rule, error) {
	ru := rule{ports: map[uint16]bool{}}
	if len(e.Ports) == 0 {
		return ru, errors.New("ports are required")
	}
	for _, p := range e.Ports {
		if p == 0 {
			return ru, errors.New("port 0 is not a port")
		}
		ru.ports[p] = true
	}
	switch {
	case e.Host != "" && e.CIDR != "":
		return ru, errors.New("set host or cidr, not both")
	case e.Host != "":
		h := normalize(e.Host)
		if _, err := netip.ParseAddr(h); err == nil {
			return ru, fmt.Errorf("%q is an address; use cidr", e.Host)
		}
		if rest, ok := strings.CutPrefix(h, "*."); ok {
			if !validName(rest) {
				return ru, fmt.Errorf("invalid wildcard %q", e.Host)
			}
			ru.suffix = "." + rest
			return ru, nil
		}
		if !validName(h) {
			return ru, fmt.Errorf("invalid hostname %q", e.Host)
		}
		ru.name = h
	case e.CIDR != "":
		var err error
		if strings.Contains(e.CIDR, "/") {
			ru.prefix, err = netip.ParsePrefix(e.CIDR)
		} else {
			var a netip.Addr
			a, err = netip.ParseAddr(e.CIDR)
			ru.prefix = netip.PrefixFrom(a.Unmap(), a.Unmap().BitLen())
		}
		if err != nil {
			return ru, err
		}
		ru.prefix = ru.prefix.Masked()
	default:
		return ru, errors.New("host or cidr is required")
	}
	return ru, nil
}

// Resolve returns the addresses to dial for host:port, or ErrDenied. A
// hostname rule match returns the name itself, to be resolved by the dialer.
// Any other error means resolution failed.
func (l *List) Resolve(ctx context.Context, host string, port uint16) ([]string, error) {
	host = normalize(host)
	ps := strconv.Itoa(int(port))
	if a, err := netip.ParseAddr(host); err == nil {
		a = a.Unmap()
		if a.Zone() != "" || !l.allowsAddr(a, port) {
			return nil, ErrDenied
		}
		return []string{netip.AddrPortFrom(a, port).String()}, nil
	}
	if !validName(host) {
		return nil, ErrDenied
	}
	for _, ru := range l.rules {
		if ru.ports[port] && ru.matchesName(host) {
			return []string{net.JoinHostPort(host, ps)}, nil
		}
	}
	if !l.anyPrefixFor(port) {
		return nil, ErrDenied
	}
	addrs, err := l.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", host, err)
	}
	// Dial the checked addresses, not the name, so a second lookup cannot
	// return an address the rules did not allow.
	var out []string
	for _, a := range addrs {
		if a = a.Unmap(); l.allowsAddr(a, port) {
			out = append(out, netip.AddrPortFrom(a, port).String())
		}
	}
	if len(out) == 0 {
		return nil, ErrDenied
	}
	return out, nil
}

// Rules describes the parsed rules, one per line, for operators to check.
func (l *List) Rules() []string {
	var out []string
	for _, ru := range l.rules {
		target := ru.name
		switch {
		case ru.suffix != "":
			target = "*" + ru.suffix
		case ru.prefix.IsValid():
			target = ru.prefix.String()
		}
		ports := slices.Sorted(maps.Keys(ru.ports))
		out = append(out, fmt.Sprintf("%s ports %v", target, ports))
	}
	return out
}

func (ru rule) matchesName(host string) bool {
	if ru.name != "" {
		return host == ru.name
	}
	return ru.suffix != "" && strings.HasSuffix(host, ru.suffix)
}

func (l *List) allowsAddr(a netip.Addr, port uint16) bool {
	for _, ru := range l.rules {
		if ru.ports[port] && ru.prefix.IsValid() && ru.prefix.Contains(a) {
			return true
		}
	}
	return false
}

func (l *List) anyPrefixFor(port uint16) bool {
	for _, ru := range l.rules {
		if ru.ports[port] && ru.prefix.IsValid() {
			return true
		}
	}
	return false
}

func normalize(host string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
}

// validName accepts DNS names: dot-separated labels of letters, digits,
// hyphens and underscores.
func validName(h string) bool {
	if h == "" || len(h) > 253 {
		return false
	}
	for label := range strings.SplitSeq(h, ".") {
		if label == "" || len(label) > 63 {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}
