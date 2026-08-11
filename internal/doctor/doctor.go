// Package doctor inspects host prerequisites for running Boks sandboxes and reports
// what is missing in terms the user can act on.
//
// Checks are values rather than a script so that each platform contributes its own
// implementations: a check that does not apply to the running OS reports StatusSkip with a
// reason instead of being compiled away into a Linux-shaped assumption.
package doctor

import (
	"context"
	"fmt"
	"io"
	"runtime"
	"strings"
	"text/tabwriter"
)

// Status is the outcome of a single check.
type Status int

const (
	// StatusOK means the requirement is satisfied.
	StatusOK Status = iota
	// StatusWarn means Boks can run but something is degraded or unverified.
	StatusWarn
	// StatusFail means sandboxes cannot start until this is fixed.
	StatusFail
	// StatusSkip means the check does not apply to this platform.
	StatusSkip
)

func (s Status) String() string {
	switch s {
	case StatusOK:
		return "ok"
	case StatusWarn:
		return "warn"
	case StatusFail:
		return "fail"
	case StatusSkip:
		return "skip"
	}
	return "unknown"
}

// Result is what a check found.
type Result struct {
	Status Status
	// Detail is a short factual statement, e.g. a version or a path.
	Detail string
	// Remedy explains what Boks expected and how to fix it. Required for
	// StatusFail and StatusWarn; ignored otherwise.
	Remedy string
}

// Check is a single host requirement.
type Check struct {
	// Name is the label shown to the user.
	Name string
	// Run performs the check. It should not panic and should be fast.
	Run func(ctx context.Context, env Env) Result
}

// Env carries settings the checks need, so that doctor does not read global state.
type Env struct {
	// ContainerdAddress is the containerd gRPC socket to probe.
	ContainerdAddress string
	// Runtime is the containerd runtime handler Boks expects, e.g.
	// "io.containerd.nerdbox.v1".
	Runtime string
	// Snapshotter is the snapshotter the runtime requires, e.g. "erofs".
	Snapshotter string
}

// Report is the outcome of a full run.
type Report struct {
	Results map[string]Result
	Order   []string
}

// Ready reports whether sandboxes can be expected to start.
func (r Report) Ready() bool {
	for _, res := range r.Results {
		if res.Status == StatusFail {
			return false
		}
	}
	return true
}

// Failures returns the names of checks that failed, in display order.
func (r Report) Failures() []string {
	var out []string
	for _, name := range r.Order {
		if r.Results[name].Status == StatusFail {
			out = append(out, name)
		}
	}
	return out
}

// Run executes every check for the current platform.
func Run(ctx context.Context, env Env) Report {
	checks := Checks()
	report := Report{Results: make(map[string]Result, len(checks))}
	for _, c := range checks {
		report.Order = append(report.Order, c.Name)
		report.Results[c.Name] = c.Run(ctx, env)
	}
	return report
}

// Checks returns the checks applicable to the current platform, in display order.
func Checks() []Check {
	checks := []Check{
		platformCheck(),
		virtualizationCheck(),
		containerdCheck(),
		snapshotterCheck(),
		snapshotterToolsCheck(),
		runtimeShimCheck(),
		hypervisorLibraryCheck(),
	}
	// Platforms contribute their own requirements rather than every check having to
	// declare itself irrelevant elsewhere.
	return append(checks, extraChecks()...)
}

// Write renders a report as an aligned table followed by remediation text.
func (r Report) Write(w io.Writer) {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, name := range r.Order {
		res := r.Results[name]
		line := fmt.Sprintf("%s\t%s", name, res.Status)
		if res.Detail != "" {
			line += "\t" + res.Detail
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	var remedies []string
	for _, name := range r.Order {
		res := r.Results[name]
		if res.Remedy != "" && (res.Status == StatusFail || res.Status == StatusWarn) {
			remedies = append(remedies, fmt.Sprintf("%s (%s):\n  %s", name, res.Status,
				strings.ReplaceAll(res.Remedy, "\n", "\n  ")))
		}
	}
	if len(remedies) > 0 {
		fmt.Fprintln(w)
		for _, rem := range remedies {
			fmt.Fprintln(w, rem)
			fmt.Fprintln(w)
		}
	}

	if r.Ready() {
		fmt.Fprintln(w, "Host looks ready to start sandboxes.")
	} else {
		fmt.Fprintf(w, "Not ready: %s must be fixed before sandboxes can start.\n",
			strings.Join(r.Failures(), ", "))
	}
}

func platformCheck() Check {
	return Check{
		Name: "platform",
		Run: func(ctx context.Context, env Env) Result {
			detail := runtime.GOOS + "/" + runtime.GOARCH
			switch runtime.GOOS {
			case "linux":
				return Result{Status: StatusOK, Detail: detail}
			case "darwin":
				if runtime.GOARCH != "arm64" {
					return Result{
						Status: StatusFail, Detail: detail,
						Remedy: "Boks on macOS targets Apple silicon. Intel Macs have no supported VM backend.",
					}
				}
				return Result{Status: StatusOK, Detail: detail}
			default:
				return Result{
					Status: StatusFail, Detail: detail,
					Remedy: "Boks supports Linux today and macOS next. Windows is blocked on runtime support.",
				}
			}
		},
	}
}
