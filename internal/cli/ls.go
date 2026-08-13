package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/policy"
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
			writeTable(env.Stdout, infos, publishedPorts())
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

// publishedPorts reads what every running sandbox network is publishing.
//
// It reads the supervisors' state files rather than asking each one over its control socket,
// deliberately. A listing must not be something one wedged background process can hang, and
// `ls` is the command people run when something is already wrong. The state file is rewritten
// by the supervisor after every change, so it is as current as anything short of a round
// trip; `boks ports` is the authoritative view of one sandbox.
func publishedPorts() map[string]string {
	out := map[string]string{}
	for _, st := range enforce.List(policy.StateDir()) {
		if column := portsColumn(st.Ports); column != "" {
			out[st.Sandbox] = column
		}
	}
	return out
}

// writeTable renders the listing with sbx's columns.
//
// PORTS shows what each sandbox publishes right now, in Docker's and sbx's
// `127.0.0.1:8080->3000/tcp` notation. A sandbox that publishes nothing gets a dash rather
// than a blank, for the same reason every other column does: "none" and "not applicable"
// should not look the same. Image, creation time and the rest are in --json and
// 'boks inspect', which is where detail belongs.
func writeTable(w io.Writer, infos []sandbox.Info, published map[string]string) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "SANDBOX\tAGENT\tSTATUS\tPORTS\tWORKSPACE")
	for _, info := range infos {
		status := info.Status
		if info.Ephemeral {
			status += " (ephemeral)"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			info.Name, dash(info.Agent), status, dash(published[info.Name]), dash(info.Workspace()))
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
