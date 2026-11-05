package e2e

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// TestDrainNotice moves a connector's tunnels from one hub to another, as a
// load balancer does during a rollout. When the old hub's last tunnel is
// about to drain, the hub stops being ready but keeps serving for the notice
// period, so its load balancer can stop sending it clients first.
func TestDrainNotice(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	issuePKI(t, dir)
	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)

	const notice = 1500 * time.Millisecond
	endpoints := fmt.Sprintf(`
  - {name: echo, listen: "127.0.0.1:0", target: "%s"}
`, echo)
	oldHub := startHubNotice(t, ctx, dir, endpoints, notice)
	newHub := startHubNotice(t, ctx, dir, endpoints, notice)

	// New tunnels go to whichever hub the "load balancer" points at.
	var current atomic.Pointer[net.Addr]
	oldAddr, newAddr := oldHub.TunnelAddr(), newHub.TunnelAddr()
	current.Store(&oldAddr)
	var opened atomic.Int32
	front := forward(t, func() net.Addr { return *current.Load() }, &opened)
	startConnector(t, ctx, dir, dir, front, fmt.Sprintf(`
  - {cidr: 127.0.0.1/32, ports: [%s]}
`, echoPort), 2*time.Second)
	waitFor(t, func() bool { return oldHub.Tunnels() == 2 && oldHub.Ready() })

	current.Store(&newAddr)
	waitFor(t, func() bool { return !oldHub.Ready() })
	notReady := time.Now()
	if n := oldHub.Tunnels(); n == 0 {
		t.Fatal("old hub stopped being ready only once its tunnels were gone")
	}

	// Until the notice runs out, the old hub still carries what reaches it.
	for time.Since(notReady) < notice*2/3 {
		if got := roundTrip(t, oldHub.EndpointAddr("echo"), []byte("noticed")); string(got) != "noticed" {
			t.Fatalf("round trip %s after the old hub stopped being ready: got %q", time.Since(notReady).Round(time.Millisecond), got)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if oldHub.Ready() {
		t.Fatal("old hub became ready again with no new tunnels")
	}

	waitFor(t, func() bool { return oldHub.Tunnels() == 0 && newHub.Ready() })
	if got := roundTrip(t, newHub.EndpointAddr("echo"), []byte("moved")); string(got) != "moved" {
		t.Fatalf("round trip on the new hub: got %q", got)
	}
}
