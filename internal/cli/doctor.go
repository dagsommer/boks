package cli

import (
	"io"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/doctor"
)

// reportHealth prints a report and exits on the verdict that printing it produced.
//
// The exit status is taken from the value Write returned, never recomputed from the report:
// this command's whole job is to tell the truth about a host, and a `$?` of 0 under a
// summary that reads "Not ready" is that job's worst possible failure. One decision, printed
// and exited on, cannot disagree with itself.
func reportHealth(w io.Writer, report doctor.Report) error {
	if verdict := report.Write(w); !verdict.Ready {
		return &ExitError{Code: 1}
	}
	return nil
}

func newDoctorCommand(env Env, dev *devFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor [flags]",
		Short: "Check host prerequisites for running sandboxes",
		Long: `Checks the host for everything a sandbox needs, and explains how to fix what is missing.
Exits non-zero if sandboxes cannot start.

Which containerd, runtime and snapshotter are checked follows --containerd-address,
--runtime and --snapshotter, the developer flags described in 'boks --help'.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return reportHealth(env.Stdout, doctor.Run(cmd.Context(), doctor.Env{
				ContainerdAddress: dev.address,
				Runtime:           dev.runtimeID,
				Snapshotter:       dev.snapshotter,
			}))
		},
	}
}
