package e2e

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/Instalily/edge-connector/internal/health"
)

// TestHubDrain checks what a hub does while it shuts down: its health checks
// fail, so load balancers stop sending it traffic, but it keeps serving
// what still reaches it until it stops.
func TestHubDrain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dir := t.TempDir()
	issuePKI(t, dir)
	echo := echoServer(t)
	_, echoPort, _ := net.SplitHostPort(echo)

	h := startHub(t, ctx, dir, fmt.Sprintf(`
  - {name: echo, listen: "127.0.0.1:0", target: "%s"}
`, echo))
	healthAddr := fmt.Sprintf("127.0.0.1:%d", closedPort(t))
	go health.Serve(ctx, healthAddr, h.Live, h.Ready, logger(t, "health"))
	startConnector(t, ctx, dir, dir, h.TunnelAddr(), fmt.Sprintf(`
  - {cidr: 127.0.0.1/32, ports: [%s]}
`, echoPort), 0)
	waitFor(t, func() bool { return h.Tunnels() == 2 })

	status := func(path string) int {
		resp, err := (&http.Client{Timeout: 2 * time.Second}).Get("http://" + healthAddr + path)
		if err != nil {
			return 0
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	waitFor(t, func() bool { return status("/healthz") == http.StatusOK })
	if got := status("/readyz"); got != http.StatusOK {
		t.Fatalf("/readyz with tunnels: %d, want 200", got)
	}

	h.Drain()
	for _, path := range []string{"/healthz", "/readyz"} {
		if got := status(path); got != http.StatusServiceUnavailable {
			t.Errorf("%s while draining: %d, want 503", path, got)
		}
	}
	if got := roundTrip(t, h.EndpointAddr("echo"), []byte("draining")); string(got) != "draining" {
		t.Fatalf("round trip while draining: got %q", got)
	}
	if n := h.Tunnels(); n != 2 {
		t.Fatalf("draining hub has %d tunnels, want it to keep both", n)
	}
}
