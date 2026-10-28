package allowlist

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"testing"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range f[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if len(out) == 0 {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func TestResolve(t *testing.T) {
	r := fakeResolver{
		"db.internal":      {"10.20.1.5"},
		"split.internal":   {"10.20.1.6", "192.168.1.1"},
		"outside.internal": {"192.168.1.1"},
		"metadata.local":   {"169.254.169.254"},
		// Names the hostname rules don't match fall through to the CIDR
		// rules, so these resolve outside them.
		"db.corp.local":      {"192.168.5.5"},
		"evil-db.corp.local": {"192.168.5.6"},
	}
	l, err := New([]Entry{
		{Host: "SAP-PRD.corp.local.", Ports: []uint16{3300}},
		{Host: "*.db.corp.local", Ports: []uint16{5432}},
		{CIDR: "10.20.0.0/16", Ports: []uint16{5432, 1433}},
		{CIDR: "172.16.0.9", Ports: []uint16{443}},
	}, r)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		host string
		port uint16
		want []string // nil means denied
	}{
		{"sap-prd.corp.local", 3300, []string{"sap-prd.corp.local:3300"}},
		{"SAP-PRD.CORP.LOCAL.", 3300, []string{"sap-prd.corp.local:3300"}},
		{"sap-prd.corp.local", 3301, nil},
		{"orders.db.corp.local", 5432, []string{"orders.db.corp.local:5432"}},
		{"a.b.db.corp.local", 5432, []string{"a.b.db.corp.local:5432"}},
		{"db.corp.local", 5432, nil}, // the wildcard does not match its own suffix
		{"evil-db.corp.local", 5432, nil},
		{"10.20.3.4", 1433, []string{"10.20.3.4:1433"}},
		{"10.21.3.4", 1433, nil},
		{"::ffff:10.20.3.4", 1433, []string{"10.20.3.4:1433"}},
		{"172.16.0.9", 443, []string{"172.16.0.9:443"}},
		{"172.16.0.10", 443, nil},
		{"db.internal", 5432, []string{"10.20.1.5:5432"}},
		{"split.internal", 5432, []string{"10.20.1.6:5432"}},
		{"outside.internal", 5432, nil},
		{"metadata.local", 5432, nil},
		{"db.internal", 22, nil},
		{"bad host", 5432, nil},
		{"", 5432, nil},
	}
	for _, tt := range tests {
		got, err := l.Resolve(context.Background(), tt.host, tt.port)
		if tt.want == nil {
			if !errors.Is(err, ErrDenied) {
				t.Errorf("%s:%d: got %v, %v; want denied", tt.host, tt.port, got, err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, tt.want) {
			t.Errorf("%s:%d: got %v, %v; want %v", tt.host, tt.port, got, err, tt.want)
		}
	}
}

func TestResolveFailureIsNotDenial(t *testing.T) {
	l, err := New([]Entry{{CIDR: "10.0.0.0/8", Ports: []uint16{5432}}}, fakeResolver{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = l.Resolve(context.Background(), "missing.internal", 5432)
	if err == nil || errors.Is(err, ErrDenied) {
		t.Fatalf("got %v; want a resolution error", err)
	}
}

func TestNewRejectsBadEntries(t *testing.T) {
	bad := [][]Entry{
		nil,
		{{Host: "a.local"}},
		{{Host: "a.local", Ports: []uint16{0}}},
		{{Host: "a.local", CIDR: "10.0.0.0/8", Ports: []uint16{1}}},
		{{Ports: []uint16{1}}},
		{{Host: "10.0.0.1", Ports: []uint16{1}}},
		{{Host: "a.*.local", Ports: []uint16{1}}},
		{{Host: "*", Ports: []uint16{1}}},
		{{CIDR: "10.0.0.0/33", Ports: []uint16{1}}},
	}
	for _, entries := range bad {
		if _, err := New(entries, fakeResolver{}); err == nil {
			t.Errorf("%+v accepted", entries)
		}
	}
}
