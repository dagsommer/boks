package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestRunValidatesPolicyFlagsAndWarns checks the two things `boks run` currently owes a
// user who passes -allow: reject a rule that will not work, and never let them believe the
// rule is protecting them.
func TestRunValidatesPolicyFlagsAndWarns(t *testing.T) {
	dir := t.TempDir()

	var out, errOut bytes.Buffer
	err := runCommand(context.Background(), Env{
		Args:   []string{dir, "-allow", "*.*.example.com"},
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errOut,
	})
	if err == nil {
		t.Fatal("an invalid -allow rule was accepted")
	}
	if !strings.Contains(err.Error(), "example.com") {
		t.Errorf("error %q should name the offending rule", err)
	}

	// A valid rule gets past validation; the run then fails for an unrelated reason
	// (no containerd here), but the warning must already have been printed.
	out.Reset()
	errOut.Reset()
	_ = runCommand(context.Background(), Env{
		Args:   []string{dir, "-allow", "example.com:443", "--", "true"},
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errOut,
	})
	if !strings.Contains(errOut.String(), "NOT enforced") {
		t.Errorf("no unenforced-policy warning was printed:\n%s", errOut.String())
	}
}

func TestPolicyFlagsSpecified(t *testing.T) {
	var f policyFlags
	if f.specified() {
		t.Error("empty flags reported as specified")
	}
	f.allow = stringList{"example.com"}
	if !f.specified() {
		t.Error("-allow was not detected")
	}
}
