package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/dagsommer/boks/internal/sandbox"
)

func lsCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks ls", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	quiet := fs.Bool("quiet", false, "print only sandbox names")
	fs.BoolVar(quiet, "q", false, "alias for -quiet")
	asJSON := fs.Bool("json", false, "print the full listing as JSON")
	address := addressFlag(fs)

	fs.Usage = func() {
		fmt.Fprint(env.Stderr, `Usage: boks ls [flags]

Lists sandboxes. A sandbox exists from 'boks create' or 'boks run' until 'boks rm';
stopped ones keep their filesystem and are listed too.

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
	if fs.NArg() > 0 {
		return fmt.Errorf("unexpected argument %q; boks ls takes no arguments", fs.Arg(0))
	}
	if *quiet && *asJSON {
		return fmt.Errorf("-q and -json cannot be combined")
	}

	infos, err := sandbox.List(ctx, *address)
	if err != nil {
		return err
	}

	switch {
	case *asJSON:
		return writeJSON(env.Stdout, infos)
	case *quiet:
		for _, info := range infos {
			fmt.Fprintln(env.Stdout, info.Name)
		}
		return nil
	default:
		writeTable(env.Stdout, infos)
		return nil
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTable renders the listing with sbx's columns.
//
// PORTS is empty because nothing publishes ports yet. The column is still there: a listing
// whose shape changes when a feature lands is a listing nobody can write a script against,
// and an empty column says "none" where a missing one says nothing at all. Image, creation
// time and the rest are in -json and 'boks inspect', which is where detail belongs.
func writeTable(w io.Writer, infos []sandbox.Info) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SANDBOX\tAGENT\tSTATUS\tPORTS\tWORKSPACE")
	for _, info := range infos {
		status := info.Status
		if info.Ephemeral {
			status += " (ephemeral)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			info.Name, dash(info.Agent), status, "", dash(info.Workspace()))
	}
	_ = tw.Flush()
}

// dash marks a value a sandbox does not have, as opposed to one that does not exist yet.
func dash(v string) string {
	if v == "" {
		return "-"
	}
	return v
}
