package cli

import (
	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/sandbox"
)

func newInspectCommand(env Env, dev *devFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect [flags] SANDBOX...",
		Short: "Print sandbox details as JSON",
		Long: `Prints everything Boks knows about a sandbox, as JSON: status, image, runtime, snapshotter,
creation time, workspaces, default command, environment and process id.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("a sandbox name is required; run 'boks ls' to see what exists")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			// Several names print as an array, so the output stays valid JSON
			// either way.
			if len(args) == 1 {
				info, err := sandbox.Inspect(cmd.Context(), dev.address, args[0])
				if err != nil {
					return err
				}
				return writeJSON(env.Stdout, info)
			}

			infos := make([]sandbox.Info, 0, len(args))
			for _, name := range args {
				info, err := sandbox.Inspect(cmd.Context(), dev.address, name)
				if err != nil {
					return err
				}
				infos = append(infos, info)
			}
			return writeJSON(env.Stdout, infos)
		},
	}
}
