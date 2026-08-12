package network

import (
	"context"
	"errors"
	"io"
	"net"

	"github.com/dagsommer/boks/internal/policy"
)

// gateway on Windows exists only so the package compiles there.
//
// The link is a SOCK_DGRAM UNIX socket, which gvisor-tap-vsock does not implement on
// Windows, and nerdbox has no Windows support either. Failing here with a sentence that
// says so beats failing later with "unsupported scheme".
type gateway struct{}

func (g *gateway) start(context.Context, Plan, *policy.Engine, io.Writer) error {
	return errors.New("network: sandbox networking is not available on Windows; " +
		"the VM runtime does not support Windows yet")
}

func (g *gateway) listen(string) (net.Listener, error) { return nil, ErrNoNetwork }

func (g *gateway) dial(context.Context, string) (net.Conn, error) { return nil, ErrNoNetwork }

func (g *gateway) stop() error { return nil }
