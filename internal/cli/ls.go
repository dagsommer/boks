package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/sandbox"
)

func newLsCommand(env Env, dev *devFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "ls [flags]",
		Aliases: []string{"list"},
		Short:   "List sandboxes",
		Long: `Lists sandboxes. A sandbox exists from 'boks create' or 'boks run' until 'boks rm';
stopped ones keep their filesystem and are listed too.`,
		Args: noArgs,
	}
	var (
		quiet  bool
		asJSON bool
	)
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "print only sandbox names")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the full listing as JSON")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if quiet && asJSON {
			return usagef("--quiet and --json cannot be combined")
		}

		infos, err := sandbox.List(cmd.Context(), dev.address)
		if err != nil {
			return err
		}

		switch {
		case asJSON:
			return writeJSON(env.Stdout, infos)
		case quiet:
			for _, info := range infos {
				fmt.Fprintln(env.Stdout, info.Name)
			}
			return nil
		default:
			writeTable(env.Stdout, infos)
			return nil
		}
	}
	return cmd
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// writeTable renders the listing with sbx's columns, plus one Boks adds.
//
// PORTS is empty because nothing publishes ports yet. The column is still there: a listing
// whose shape changes when a feature lands is a listing nobody can write a script against,
// and an empty column says "none" where a missing one says nothing at all. Image, creation
// time and the rest are in --json and 'boks inspect', which is where detail belongs.
//
// MODE is the addition, and it earns a column rather than a detail view because it answers
// the one question a listing should not make anybody go and look up: whether the thing in
// that row writes to the files on this machine. It sits next to WORKSPACE, which is the
// directory the answer is about.
func writeTable(w io.Writer, infos []sandbox.Info) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SANDBOX\tAGENT\tSTATUS\tPORTS\tMODE\tWORKSPACE")
	for _, info := range infos {
		status := info.Status
		if info.Ephemeral {
			status += " (ephemeral)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			info.Name, dash(info.Agent), status, "", info.Filesystem.Mode, dash(info.Workspace()))
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
