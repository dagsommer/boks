package enforce

import (
	"strings"
	"testing"
	"time"

	"github.com/dagsommer/boks/internal/secret"
)

// The reported case: a credential rule added while the sandbox was running, and nothing
// intercepted. The supervisor that is already there serves what it was started with, and a
// run that says nothing leaves the user watching plaintext GitHub traffic while every command
// reports success.
func TestAddingACredentialToARunningSandboxIsReported(t *testing.T) {
	running := State{Sandbox: "s", Intercept: false, Started: time.Now().Add(-90 * time.Minute)}
	spec := Spec{Sandbox: "s", Intercept: true, Inject: []string{"github:api.github.com:Authorization"}}

	note := reusedStackNote(running, spec)
	if note == "" {
		t.Fatal("a stack serving no credentials was reused for a run that asked for one, silently")
	}
	for _, want := range []string{"github", "boks stop s", "not being intercepted"} {
		if !strings.Contains(note, want) {
			t.Errorf("the note does not mention %q:\n%s", want, note)
		}
	}
	// Going from no credentials to some means the sandbox may also lack the CA, which
	// stopping does not fix. That has to be said, and only in this direction.
	if !strings.Contains(note, "boks rm s") {
		t.Errorf("the note does not mention the recreate that a missing CA needs:\n%s", note)
	}
}

// The opposite direction is a mismatch too — a rule REMOVED is still being applied by the
// running stack — but it needs no recreate, so it must not suggest one.
func TestRemovingACredentialDoesNotSuggestARecreate(t *testing.T) {
	running := State{Sandbox: "s", Intercept: true, Services: []string{"github"}}
	spec := Spec{Sandbox: "s"}

	note := reusedStackNote(running, spec)
	if note == "" {
		t.Fatal("a stack still injecting a withdrawn credential was reused silently")
	}
	if strings.Contains(note, "boks rm") {
		t.Errorf("a withdrawn credential does not need a recreate:\n%s", note)
	}
}

// Silence when they agree. A note on every ordinary run would be noise, and noise is how a
// real one gets ignored.
func TestAMatchingStackSaysNothing(t *testing.T) {
	spec := Spec{
		Sandbox:   "s",
		Intercept: true,
		Inject:    []string{"github:api.github.com:Authorization"},
		OAuth:     map[string]secret.OAuthRecord{"claude-code": {Service: "claude-code"}},
	}
	running := State{Sandbox: "s", Intercept: true, Services: credentialSummary(spec)}

	if note := reusedStackNote(running, spec); note != "" {
		t.Errorf("an unchanged credential set produced a note:\n%s", note)
	}
}

// The summary is what the comparison rests on: the SERVICE, not the whole rule, because that
// is what decides whether a host is intercepted. Order must not matter either — a user who
// reorders two -inject flags has not changed anything.
func TestCredentialSummaryIsTheServiceAndIsStable(t *testing.T) {
	a := credentialSummary(Spec{Inject: []string{"github:api.github.com:Authorization", "npm:registry.npmjs.org:Authorization"}})
	b := credentialSummary(Spec{Inject: []string{"npm:registry.npmjs.org:Authorization", "github:api.github.com:Authorization"}})
	if !equalStrings(a, b) {
		t.Errorf("the same two rules in a different order compared unequal: %v vs %v", a, b)
	}
	if len(a) != 2 || a[0] != "github" || a[1] != "npm" {
		t.Errorf("summary = %v, want the two service names sorted", a)
	}
	// A value must never reach the state file this ends up in.
	for _, s := range a {
		if strings.Contains(s, ":") {
			t.Errorf("the summary carries more than a service name: %q", s)
		}
	}
}
