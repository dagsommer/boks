package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/doctor"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

func doctorCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks doctor", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	address := fs.String("containerd-address", runtimecfg.DefaultAddress(), "containerd socket to probe")
	runtimeID := fs.String("runtime", runtimecfg.Runtime, "containerd runtime handler to check for")
	snapshot := fs.String("snapshotter", runtimecfg.Snapshotter, "containerd snapshotter to check for")

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks doctor [flags]

Checks the host for everything a sandbox needs, and explains how to fix what is missing.
Exits non-zero if sandboxes cannot start.

Flags:
`)
		fs.PrintDefaults()
	}

	if err := fs.Parse(env.Args); err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}

	report := doctor.Run(ctx, doctor.Env{
		ContainerdAddress: *address,
		Runtime:           *runtimeID,
		Snapshotter:       *snapshot,
	})
	report.Write(env.Stdout)

	if !report.Ready() {
		return &ExitError{Code: 1}
	}
	return nil
}
