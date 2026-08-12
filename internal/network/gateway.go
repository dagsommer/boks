package network

import (
	"fmt"
	"io"

	"github.com/containers/gvisor-tap-vsock/pkg/types"
)

// Why the host-side stack is embedded rather than spawned as gvproxy.
//
// The alternative is to exec the gvproxy binary once per sandbox. Running gvisor-tap-vsock
// as a library was measured to be workable — it builds and cross-compiles for darwin/arm64
// and windows/amd64, and adds 115 modules to the graph — and it is better on the two axes
// that matter here:
//
//   - Lifetime. A child process outlives a crashed parent; a goroutine cannot. There is no
//     PID to track, no socket-path race on restart, no orphan to reap after a SIGKILL, and
//     no "is gvproxy installed, and which version" question for `boks doctor` to answer.
//     One stack serves exactly one VM — a second VM on the same stack gets a duplicate
//     address and a third fails to attach — so its lifetime has to be exactly a sandbox's,
//     which is far easier to guarantee inside the process that owns the sandbox.
//   - Control. The closed posture in gatewayConfig is asserted in a typed configuration
//     that a test can read back. Through the binary it would be the *absence* of
//     command-line flags, which no test can assert and any later refactor can silently
//     undo.
//
// The cost is a large dependency — gvisor's netstack — in the Boks binary. That is real and
// accepted: it is the component doing the security-relevant work, and having it pinned in
// go.mod is preferable to depending on whichever gvproxy happens to be on PATH.

// gatewayConfig builds the stack's configuration.
//
// **Every field that could expose the host is set explicitly, to zero.** The spike that
// confirmed this transport observed that the host was unreachable from the guest — but that
// was a property of how the stack happened to be configured, not a guarantee of the
// library. gvisor-tap-vsock can be told to translate an address to the host's loopback, to
// forward host ports inward, to answer on extra gateway addresses, and to proxy the EC2
// metadata service. Boks asserts all four closed here rather than trusting a default, so
// that a version bump that changes a default cannot quietly open the host, and so that a
// test can read the intent back.
//
// Since Boks assembles the stack itself (stack_unix.go) this value no longer reaches a
// library constructor that could act on those four fields: nothing in Boks implements NAT,
// port forwarding, virtual gateway addresses or a metadata proxy at all, so they are closed
// by construction as well as by declaration. What the configuration is still *used* for is
// the addressing, and the two services the gateway runs inside the stack — the resolver and
// the address server. It is kept in this shape because it remains the one place a reader can
// see the whole posture, and because the assertion is cheap to keep and expensive to
// rediscover.
func gatewayConfig(plan Plan) *types.Configuration {
	return &types.Configuration{
		MTU:               plan.MTU,
		Subnet:            plan.Subnet.String(),
		GatewayIP:         plan.Gateway.String(),
		GatewayMacAddress: plan.GatewayMAC,

		// Closed posture, asserted rather than assumed:
		NAT:               map[string]string{}, // no address translated to the host
		Forwards:          map[string]string{}, // no host port forwarded into the guest
		GatewayVirtualIPs: nil,                 // the gateway answers on one address only
		Ec2MetadataAccess: false,               // no proxy to 169.254.169.254

		// The gateway's own resolver answers the guest, because the container's
		// resolv.conf points at it (see Plan.Annotations). Leaving DNS empty means it
		// resolves through the host's resolver rather than serving invented records.
		DNS:              nil,
		DNSSearchDomains: nil,

		Debug:       false,
		CaptureFile: "", // packet capture writes guest traffic to disk; opt-in only
	}
}

func logf(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}
