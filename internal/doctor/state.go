package doctor

import (
	"context"
	"fmt"

	"github.com/dagsommer/boks/internal/purge"
)

// stateCheck reports where Boks keeps its host-side state and how much of the disk it has
// taken.
//
// It is the one line in this report that is not about whether a sandbox can start, and it
// earns its place for the reason the report exists: doctor is where someone looks when
// something about their Boks installation is not what they expected, and "a gigabyte and a
// half of my disk is missing" is exactly that. Measured on Windows on 2026-08-16, a single
// try of Boks left 1,768.8 MB behind, in a directory nothing on screen had ever named.
//
// It can only be ok, warn or skip. A disk-space figure is not a reason to tell someone their
// host cannot run sandboxes, and doctor's verdict is read by scripts.
//
// Measuring means walking the state directory, which is cheap here for a reason worth writing
// down: the erofs snapshotter stores each layer as one layer.erofs file rather than as an
// unpacked tree, so the gigabytes live in a handful of inodes. The Windows measurement that
// prompted this was 1,768.8 MB across 59 files.
func stateCheck() Check {
	return Check{
		Name: "state directory",
		Run: func(_ context.Context, env Env) Result {
			if env.StateDir == "" {
				return Result{Status: StatusSkip, Detail: "no state directory resolved"}
			}
			// ScopeReclaim, because the figure quoted has to be the one a plain
			// `boks purge` delivers. Quoting --all's would overstate what the
			// command being named gives back.
			plan, err := purge.Inspect(env.StateDir, purge.ScopeReclaim)
			if err != nil {
				// A state directory this host will not let Boks reason about is
				// worth naming, but it is not a reason sandboxes cannot start:
				// containerd is reached through its socket either way.
				return Result{
					Status: StatusWarn, Detail: env.StateDir,
					Remedy: "Could not inspect the state directory: " + err.Error(),
				}
			}
			if !plan.Exists {
				return Result{Status: StatusOK, Detail: env.StateDir + " (not created yet)"}
			}
			return Result{
				Status: StatusOK,
				Detail: fmt.Sprintf("%s (%s, 'boks purge' reclaims %s)",
					env.StateDir, purge.Bytes(plan.Total()), purge.Bytes(plan.Freed())),
			}
		},
	}
}
