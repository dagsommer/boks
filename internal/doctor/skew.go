package doctor

import (
	"context"
	"fmt"
	"os"

	"github.com/dagsommer/boks/internal/daemon"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

// runtimeSkewCheck asks whether the pieces this host has are compatible with each other,
// rather than merely present.
//
// Every other check here answers "is it there". That is necessary and it is not sufficient, and
// the gap between the two is where the worst failures live: on 2026-08-15, on WSL2, a host with
// `containerd ok` and `vm runtime ok` failed at task start with
//
//	unsupported protocol: Yunix
//
// because the shim was built against containerd 2.3.3 and the daemon was 2.2.2. Both lines were
// true. The set was wrong, and nothing in doctor was asking about the set.
//
// The comparison is cheap and entirely local: a Go binary records the module graph it was built
// from, which is what `go version -m` prints, and debug/buildinfo reads it out of the file with
// no subprocess. See internal/daemon/compat.go for the rule and for the three related skews
// this does not yet cover.
func runtimeSkewCheck() Check {
	return Check{
		Name: "runtime skew",
		Run: func(ctx context.Context, env Env) Result {
			shim := daemon.FindShim(env.Runtime, os.Getenv("PATH"))
			if shim == "" {
				// The `vm runtime` line already reports a missing shim, and saying
				// so twice would make the report longer without making it truer.
				return Result{Status: StatusSkip, Detail: "no shim to compare (see vm runtime)"}
			}
			shimVersion := daemon.ShimContainerd(shim)
			if shimVersion == "" {
				return Result{
					Status: StatusWarn,
					Detail: "the shim does not record which containerd it was built against",
					Remedy: fmt.Sprintf("%s carries no Go build information naming %s, so Boks cannot tell\n"+
						"whether it matches this daemon. A shim stripped of its build info, or one not\n"+
						"built from Go, reads this way. The skew this would catch fails at task start\n"+
						"with 'unsupported protocol: Yunix'.", shim, "github.com/containerd/containerd/v2"),
				}
			}

			ctx, cancel := context.WithTimeout(ctx, probeTimeout)
			defer cancel()
			client, err := runtimecfg.Connect(ctx, env.ContainerdAddress)
			if err != nil {
				return Result{Status: StatusSkip, Detail: "not checked (containerd unreachable)"}
			}
			defer client.Close()
			version, err := client.Version(ctx)
			if err != nil {
				return Result{Status: StatusSkip, Detail: "not checked (containerd unreachable)"}
			}

			// The daemon's *reported* version is what is compared, not its binary's
			// name or a --version line: it is the only one that describes the process
			// that will actually receive the shim's bootstrap parameters.
			if skew := daemon.CheckSkew(version.Version, shimVersion); skew != nil {
				return Result{Status: StatusFail, Detail: skew.Detail, Remedy: skew.Remedy}
			}
			return Result{
				Status: StatusOK,
				Detail: fmt.Sprintf("containerd %s, shim built against %s", version.Version, shimVersion),
			}
		},
	}
}
