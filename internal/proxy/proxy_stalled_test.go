package proxy

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"
)

// The dial carries a keepalive tuned to the stalled timeout, so a peer that stops
// acknowledging is dropped after about that long; a disabled timeout leaves the defaults.
func TestDialWithKeepAliveDoesNotBreakAConnection(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		if c, err := l.Accept(); err == nil {
			c.Close()
		}
	}()
	ap := netip.MustParseAddrPort(l.Addr().String())
	for _, stalled := range []time.Duration{30 * time.Second, 6 * time.Second, -1} {
		c, err := dialWithKeepAlive(context.Background(), ap, stalled)
		if err != nil {
			t.Fatalf("stalled=%v: %v", stalled, err)
		}
		c.Close()
		if stalled == -1 {
			break
		}
		go func() {
			if c, err := l.Accept(); err == nil {
				c.Close()
			}
		}()
	}
}
