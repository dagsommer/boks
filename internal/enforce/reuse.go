package enforce

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// credentialSummary is the set of credential rules a spec carries, by name, in a form two
// specs can be compared by. Names only: they are already visible in `boks secret ls` and in
// this sandbox's own state file, and nothing about a value belongs in either.
//
// The `-inject` entries travel as the strings the user typed, which begin with the service
// name and continue with where it may go ("github:api.github.com:Authorization"). The service
// is the part that decides whether a host is intercepted, so that is the part compared.
func credentialSummary(spec Spec) []string {
	seen := map[string]bool{}
	for _, rule := range spec.Inject {
		name := rule
		if i := strings.IndexByte(rule, ':'); i > 0 {
			name = rule[:i]
		}
		if name != "" {
			seen[name] = true
		}
	}
	for service := range spec.OAuth {
		seen[service] = true
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// reusedStackNote explains a running sandbox whose network was built with different
// credentials from the ones this run asks for, or returns "" when they agree.
//
// # Why this is needed
//
// A stack is not rebuilt for a running sandbox, and that is deliberate: the VM connects to
// the link socket once, while it boots, and a socket bound afterwards is a different socket
// that nothing in the guest will ever speak to (measured 2026-08-12). So Ensure reuses the
// supervisor that is already there — and that supervisor is serving the credentials it was
// STARTED with. A credential added since is not injected, and its host is not intercepted.
//
// Nothing said so. Reported on 2026-08-30: a credential rule added for GitHub against a
// running sandbox on macOS, and requests to GitHub carried on unintercepted, with every
// command reporting success and the decision log showing ordinary forwards.
//
// # Why the answer is 'stop', and when it is 'rm'
//
// Stopping the sandbox ends its supervisor, and the next run builds a stack from the current
// spec — no recreation, and nothing in the sandbox's filesystem is lost. That is the whole
// fix, PROVIDED the sandbox already has Boks' CA in its trust store. A sandbox created with
// no credential at all never had one mounted, because the authority is opened only when a
// credential rule justifies it; interception would then fail inside the guest at certificate
// verification. Mounts are fixed when a container is created, so that case does need `boks
// rm`. The note says both, in that order, because the first is the common one and costs
// nothing.
func reusedStackNote(st State, spec Spec) string {
	want := credentialSummary(spec)
	wantIntercept := spec.intercepts()
	if st.Intercept == wantIntercept && equalStrings(st.Services, want) {
		return ""
	}

	var b strings.Builder
	fmt.Fprintf(&b, "note: sandbox %q already has a network running", spec.Sandbox)
	if !st.Started.IsZero() {
		fmt.Fprintf(&b, ", started %s ago", roughly(time.Since(st.Started)))
	}
	b.WriteString(", and a stack is\n      not rebuilt for a running sandbox — the VM attaches to its link socket once, at\n" +
		"      boot. It is serving the credentials it was started with:\n")
	fmt.Fprintf(&b, "        running: %s\n        this run: %s\n", describeCredentials(st.Services), describeCredentials(want))
	b.WriteString("      So a credential added since it started is not being injected, and its host is\n" +
		"      not being intercepted. To apply it:\n")
	fmt.Fprintf(&b, "        boks stop %s\n", spec.Sandbox)
	if wantIntercept && !st.Intercept {
		fmt.Fprintf(&b, "      If it still is not intercepted after that, this sandbox was created with no\n"+
			"      credential at all and so has no Boks CA in its trust store, which only a\n"+
			"      recreate fixes: boks rm %s\n", spec.Sandbox)
	}
	return b.String()
}

func describeCredentials(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return strings.Join(names, ", ")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// roughly renders a duration the way a person reads one. Exact enough to answer "is this the
// stack I started a minute ago, or one from this morning".
func roughly(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
}
