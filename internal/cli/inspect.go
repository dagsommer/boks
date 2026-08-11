package cli

import (
	"context"
	"flag"
	"fmt"

	"github.com/dagsommer/boks/internal/sandbox"
)

func inspectCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks inspect", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	address := addressFlag(fs)

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks inspect [flags] <name>

Prints everything Boks knows about a sandbox, as JSON: status, image, runtime, snapshotter,
creation time, workspaces, default command, environment and process id.

Flags:
`)
		fs.PrintDefaults()
	}

	names, err := parseInterspersed(fs, env.Args)
	if err != nil {
		if err == flag.ErrHelp {
			return flagErrHelp
		}
		return err
	}
	if len(names) == 0 {
		fs.Usage()
		return fmt.Errorf("a sandbox name is required; run 'boks ls' to see what exists")
	}

	// Several names print as an array, so the output stays valid JSON either way.
	if len(names) == 1 {
		info, err := sandbox.Inspect(ctx, *address, names[0])
		if err != nil {
			return err
		}
		return writeJSON(env.Stdout, info)
	}

	infos := make([]sandbox.Info, 0, len(names))
	for _, name := range names {
		info, err := sandbox.Inspect(ctx, *address, name)
		if err != nil {
			return err
		}
		infos = append(infos, info)
	}
	return writeJSON(env.Stdout, infos)
}
