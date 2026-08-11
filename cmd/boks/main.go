// Command boks runs untrusted developer tooling inside isolated microVMs.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/dagsommer/boks/internal/cli"
)

func main() {
	// Cancel on interrupt so sandbox cleanup runs instead of leaking a VM. The signal is
	// also forwarded to the guest process; see internal/sandbox.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	os.Exit(cli.Main(ctx, cli.Env{
		Args:   os.Args[1:],
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}))
}
