package sandbox_test

import (
	"context"
	"os"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/sandbox"
)

// TestIntegrationPolicySurvivesStopAndStart is the regression test for the bug the policy
// store exists to fix.
//
// Policy used to live in the invocation. `boks run -policy locked -allow example.test:443`
// configured the process serving that sandbox's network and nothing else, so `boks stop`
// followed by `boks start` — a command with no policy flags at all — served the sandbox the
// *default* preset instead. A sandbox's containment silently widened when you restarted it.
//
// The fix is that the sandbox carries its own policy selection, in a container label, and
// every command that brings its network up reads it back. This test drives a real containerd
// through create → start → stop → start and asserts two things at the end: that the record is
// still there, and — the part that matters — that resolving it produces the same containment
// it was created with rather than the default.
//
// Needs BOKS_INTEGRATION=1. Like the rest of the suite it defaults to the isolating runtime;
// run against another one and it proves the containerd plumbing only, not isolation.
func TestIntegrationPolicySurvivesStopAndStart(t *testing.T) {
	ws := tempWorkspace(t)
	cfg := newSandbox(t, ws)
	ctx := context.Background()

	// The policy this sandbox is created with: locked, plus one destination. Nothing else
	// in the test tells any later command about it.
	cfg.Policy = &policy.SandboxPolicy{
		V:      policy.SandboxPolicyVersion,
		Preset: policy.PresetLocked,
		Allow:  []string{"allowed.test:443"},
		Deny:   []string{"blocked.test"},
	}

	if _, err := sandbox.Create(ctx, cfg); err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertPolicyPreserved(t, cfg, "after Create")

	if err := sandbox.Start(ctx, cfg.Address, cfg.Name, os.Stderr); err != nil {
		t.Fatalf("Start: %v", err)
	}
	assertPolicyPreserved(t, cfg, "after Start")

	if err := sandbox.Stop(ctx, cfg.Address, cfg.Name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	assertPolicyPreserved(t, cfg, "after Stop")

	if err := sandbox.Start(ctx, cfg.Address, cfg.Name, os.Stderr); err != nil {
		t.Fatalf("Start after Stop: %v", err)
	}
	assertPolicyPreserved(t, cfg, "after a stop and a start")
}

// assertPolicyPreserved reads the sandbox back the way `boks start` does and checks that the
// policy it would be served is the one it was created with.
func assertPolicyPreserved(t *testing.T, cfg sandbox.Config, when string) {
	t.Helper()

	info, ok := find(t, cfg.Address, cfg.Name)
	if !ok {
		t.Fatalf("%s: sandbox %q is not listed", when, cfg.Name)
	}
	record := info.Policy
	if record == nil {
		t.Fatalf("%s: the sandbox forgot its policy; a later start would serve it the default preset", when)
	}
	if record.Preset != cfg.Policy.Preset || len(record.Allow) != 1 || record.Allow[0] != cfg.Policy.Allow[0] {
		t.Fatalf("%s: recorded policy is %+v, want %+v", when, record, cfg.Policy)
	}

	// The record is only worth having if it resolves back to the same containment, so
	// assert the decisions rather than the fields. An empty store stands in for a machine
	// with no stored rules, which is the case that used to fall back to the default.
	res, err := record.Request(policy.NewStore(""), cfg.Name).Resolve()
	if err != nil {
		t.Fatalf("%s: resolving the recorded policy: %v", when, err)
	}
	pol, err := res.Policy()
	if err != nil {
		t.Fatalf("%s: %v", when, err)
	}
	for _, tc := range []struct {
		destination string
		allowed     bool
	}{
		{"allowed.test:443", true},
		{"blocked.test:443", false},
		// github.com is in the standard preset and not in this sandbox's policy, so it
		// is exactly what a fallback to the default would let through.
		{"github.com:443", false},
	} {
		target, err := policy.ParseTarget(tc.destination, 443)
		if err != nil {
			t.Fatal(err)
		}
		if v := pol.Evaluate(target); v.Allowed != tc.allowed {
			t.Errorf("%s: %s allowed=%v, want %v (%s)", when, tc.destination, v.Allowed, tc.allowed, v.Reason)
		}
	}
}
