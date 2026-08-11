package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/dagsommer/boks/internal/sandbox"
)

func lsCommand(ctx context.Context, env Env) error {
	fs := flag.NewFlagSet("boks ls", flag.ContinueOnError)
	fs.SetOutput(env.Stderr)

	quiet := fs.Bool("q", false, "print only sandbox names")
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
		writeTable(env.Stdout, infos, time.Now())
		return nil
	}
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeTable(w io.Writer, infos []sandbox.Info, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tSTATUS\tIMAGE\tWORKSPACE\tAGE")
	for _, info := range infos {
		status := info.Status
		if info.Ephemeral {
			status += " (ephemeral)"
		}
		workspace := info.Workspace()
		if workspace == "" {
			workspace = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			info.Name, status, info.Image, workspace, humanAge(now.Sub(info.Created)))
	}
	_ = tw.Flush()
}

// humanAge renders a duration the way a listing wants it: one unit, no decimals, so the
// column stays narrow and scannable.
func humanAge(d time.Duration) string {
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
