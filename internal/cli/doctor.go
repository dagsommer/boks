package cli

import (
	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/doctor"
)

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
			report := doctor.Run(cmd.Context(), doctor.Env{
				ContainerdAddress: dev.address,
				Runtime:           dev.runtimeID,
				Snapshotter:       dev.snapshotter,
			})
			report.Write(env.Stdout)

			if !report.Ready() {
				return &ExitError{Code: 1}
			}
			return nil
		},
	}
}
