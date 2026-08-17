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
	"slices"
	"strings"
	"text/tabwriter"
)

// Status is the outcome of a single check.
type Status int

const (
	// StatusUnknown is the zero value, and means no check reported this: a name in the
	// display order with nothing recorded against it.
	//
	// It is first deliberately. StatusOK used to be the zero value, which made "this
	// check produced no result" indistinguishable from "this check passed" — a
	// requirement that was never consulted read as a requirement that was satisfied, in
	// the one command whose entire job is not to do that. Nothing can now say a host is
	// ready by omission.
	StatusUnknown Status = iota
	// StatusOK means the requirement is satisfied.
	StatusOK
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
	// StateDir is where Boks keeps its host-side state. Empty means the caller did not
	// resolve one, and the check that reads it skips rather than resolving its own — this
	// package does not read global state.
	StateDir string
}

// Report is the outcome of a full run.
type Report struct {
	Results map[string]Result
	Order   []string
}

// Verdict is the single decision doctor makes about a host: whether sandboxes can start,
// which checks say they cannot, and the sentence that states it.
//
// It exists because that decision used to be made twice — once to choose the summary line
// and once again to choose the exit code — from two different traversals of the report:
// Ready() ranged over the results map, Failures() over the display order. Two answers to one
// question can only ever agree by luck, and the failure mode is the worst one this command
// has: a summary that says the host is not ready above an exit status that says it is, which
// is what a script reads. Now Write prints Summary and hands back the same value the caller
// exits on, so no traversal can disagree with the text the user was shown.
type Verdict struct {
	// Ready reports whether sandboxes can be expected to start.
	Ready bool
	// Failures names the checks blocking that, in display order.
	Failures []string
	// Summary is the closing line of the report, and states exactly the above.
	Summary string
}

// Verdict decides, once, whether this host can start sandboxes.
//
// Every name is consulted: those in the display order, and then any result that is not in it
// at all. A result nobody displays still counts, because the question is whether the host is
// ready, not whether the table happens to mention why it is not.
func (r Report) Verdict() Verdict {
	var failures []string
	for _, name := range r.names() {
		// A missing result is a failure and not an omission. Checks are added per
		// platform and the two collections are built together, so this should be
		// unreachable — but "should be unreachable" is not a basis for reporting a host
		// as ready.
		res, ok := r.Results[name]
		if !ok || res.Status == StatusFail || res.Status == StatusUnknown {
			failures = append(failures, name)
		}
	}
	if len(failures) == 0 {
		return Verdict{Ready: true, Summary: "Host looks ready to start sandboxes."}
	}
	return Verdict{
		Failures: failures,
		Summary: fmt.Sprintf("Not ready: %s must be fixed before sandboxes can start.",
			strings.Join(failures, ", ")),
	}
}

// names is every check the report knows about: the display order first, then anything
// recorded outside it, sorted so the output does not depend on map iteration order.
func (r Report) names() []string {
	names := make([]string, 0, len(r.Order)+len(r.Results))
	seen := make(map[string]bool, len(r.Order))
	for _, name := range r.Order {
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	var extra []string
	for name := range r.Results {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	slices.Sort(extra)
	return append(names, extra...)
}

// Ready reports whether sandboxes can be expected to start.
func (r Report) Ready() bool { return r.Verdict().Ready }

// Failures returns the names of checks that failed, in display order.
func (r Report) Failures() []string { return r.Verdict().Failures }

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
		runtimeSkewCheck(),
		hypervisorLibraryCheck(),
		guestImageCheck(),
		stateCheck(),
	}
	// Platforms contribute their own requirements rather than every check having to
	// declare itself irrelevant elsewhere.
	return append(checks, extraChecks()...)
}

// Write renders a report as an aligned table followed by remediation text, and returns the
// verdict it just printed.
//
// The caller is meant to exit on the returned value rather than ask the report again: the
// exit status and the closing line are then the same decision by construction, and cannot
// contradict each other however the report was assembled.
func (r Report) Write(w io.Writer) Verdict {
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, name := range r.names() {
		res := r.Results[name]
		line := fmt.Sprintf("%s\t%s", name, res.Status)
		if res.Detail != "" {
			line += "\t" + res.Detail
		}
		fmt.Fprintln(tw, line)
	}
	tw.Flush()

	var remedies []string
	for _, name := range r.names() {
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

	verdict := r.Verdict()
	fmt.Fprintln(w, verdict.Summary)
	return verdict
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
			case "windows":
				// This said `fail` — "That is ctr, not Boks … run Boks inside
				// WSL2 in the meantime" — for a day after it stopped being
				// true. On 2026-08-14 `boks run --net nat` ran a container in a
				// microVM on Windows 11 hardware, on Boks' own stack, with the
				// policy engine allowing one destination and refusing another;
				// on 2026-08-15 the same probe fetched HTTP 200 from
				// github.com, from an unelevated shell (docs/verification.md).
				// README.md and docs/get-started.md both promise "Nothing
				// should be `fail`", so a failing platform line on a working
				// host is not a stale comment — it is an instruction to give up.
				//
				// The architecture arm is a real limit and stays: the Windows
				// stack is x86-64 only, because neither the krun.dll recipe nor
				// the mkfs.erofs.exe one cross-compiles beyond it.
				if runtime.GOARCH != "amd64" {
					return Result{
						Status: StatusFail, Detail: detail,
						Remedy: "Boks on Windows is x86-64 only. There is no krun.dll and no mkfs.erofs.exe\n" +
							"for ARM64 Windows, and neither build recipe cross-compiles beyond x86-64.\n" +
							"Run Boks inside WSL2 with nested virtualisation instead. See docs/windows.md.",
					}
				}
				return Result{Status: StatusOK, Detail: detail}
			default:
				return Result{
					Status: StatusFail, Detail: detail,
					Remedy: "Boks runs on Linux, on Apple silicon macOS, and on x86-64 Windows.\n" +
						"There is no VM backend for this platform.",
				}
			}
		},
	}
}
