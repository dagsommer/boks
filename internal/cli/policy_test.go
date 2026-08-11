package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	env := Env{
		Args:   args[1:],
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &errOut,
	}
	switch args[0] {
	case "policy":
		err = policyCommand(context.Background(), env)
	case "secret":
		err = secretCommand(context.Background(), env)
	case "ca":
		err = caCommand(context.Background(), env)
	default:
		t.Fatalf("unknown command %q", args[0])
	}
	return out.String(), errOut.String(), err
}

func TestPolicyLsShowsResolvedRules(t *testing.T) {
	out, _, err := runCLI(t, "", "policy", "ls", "-policy", "locked", "-allow", "api.example.com:443")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	for _, want := range []string{"api.example.com:443", "default: deny", "deny (always wins)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// The user must not be able to read this output and conclude they are protected.
	if !strings.Contains(out, "NOT enforced") {
		t.Errorf("output does not say the policy is unenforced:\n%s", out)
	}
}

func TestPolicyLsDefaultsToStandard(t *testing.T) {
	out, _, err := runCLI(t, "", "policy", "ls")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	if !strings.Contains(out, "policy "+policy.PresetStandard) {
		t.Errorf("default preset is not %s:\n%s", policy.PresetStandard, out)
	}
}

func TestPolicyLsRejectsBadRules(t *testing.T) {
	if _, _, err := runCLI(t, "", "policy", "ls", "-allow", "*.*.example.com"); err == nil {
		t.Error("an invalid -allow rule was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "ls", "-policy", "balanced"); err == nil {
		t.Error("an unknown preset was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "ls", "-inject", "tok@*=bearer"); err == nil {
		t.Error("a catch-all injection rule was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "ls", "-guest-credential", "unknown=placeholder"); err == nil {
		t.Error("a placeholder for a service with no injection rule was accepted")
	}
}

func TestPolicyLogWithoutAFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.jsonl")
	out, _, err := runCLI(t, "", "policy", "log", "-file", missing)
	if err != nil {
		t.Fatalf("policy log: %v", err)
	}
	if !strings.Contains(out, "no decisions recorded yet") {
		t.Errorf("output = %q", out)
	}
}

func TestPolicyLogReadsDecisions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	sink, err := policy.NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	log := policy.NewLog(4)
	log.AddSink(sink)
	engine := policy.NewEngine(mustPreset(t, policy.PresetLocked), log)
	target, err := policy.ParseTarget("blocked.test:443", 443)
	if err != nil {
		t.Fatalf("ParseTarget: %v", err)
	}
	engine.CheckMode(policy.StageConnect, target, policy.ModeForwardBypass)
	engine.CheckMode(policy.StageConnect, target, policy.ModeForwardBypass)
	sink.Close()

	out, _, err := runCLI(t, "", "policy", "log", "-file", path)
	if err != nil {
		t.Fatalf("policy log: %v", err)
	}
	// Aggregated: one row for the destination, a count of two, and the mode that says
	// whether boks could read it.
	for _, want := range []string{"Blocked requests:", "blocked.test:443", "forward-bypass", "COUNT"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	if !strings.Contains(out, "  2\n") {
		t.Errorf("two identical decisions were not collapsed into a count of 2:\n%s", out)
	}

	raw, _, err := runCLI(t, "", "policy", "log", "-file", path, "-raw")
	if err != nil {
		t.Fatalf("policy log -raw: %v", err)
	}
	if !strings.Contains(raw, "DENY") || strings.Count(raw, "blocked.test") != 2 {
		t.Errorf("-raw should print every decision:\n%s", raw)
	}
}

func TestSecretRoundTripThroughCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(secret.PassphraseEnv, "test-passphrase")
	t.Setenv("BOKS_STATE_DIR", dir)

	const value = "ghp_cli_canary_value"
	out, _, err := runCLI(t, value+"\n", "secret", "set", "github")
	if err != nil {
		t.Fatalf("secret set: %v", err)
	}
	if !strings.Contains(out, "github") {
		t.Errorf("output = %q", out)
	}

	out, _, err = runCLI(t, "", "secret", "ls")
	if err != nil {
		t.Fatalf("secret ls: %v", err)
	}
	if strings.TrimSpace(out) != "github" {
		t.Errorf("secret ls = %q, want just the name", out)
	}
	// There is no command that prints a value, so the only assertion available is that
	// listing does not print one.
	if strings.Contains(out, value) {
		t.Errorf("secret ls printed the value: %q", out)
	}

	if _, _, err := runCLI(t, "", "secret", "rm", "github"); err != nil {
		t.Fatalf("secret rm: %v", err)
	}
	out, _, err = runCLI(t, "", "secret", "ls")
	if err != nil {
		t.Fatalf("secret ls: %v", err)
	}
	if !strings.Contains(out, "no secrets stored") {
		t.Errorf("output = %q", out)
	}
}

func TestSecretRequiresAPassphrase(t *testing.T) {
	t.Setenv(secret.PassphraseEnv, "")
	t.Setenv("BOKS_STATE_DIR", t.TempDir())
	_, _, err := runCLI(t, "value\n", "secret", "set", "github")
	if err == nil {
		t.Fatal("a secret was stored with no passphrase")
	}
	if !strings.Contains(err.Error(), secret.PassphraseEnv) {
		t.Errorf("error %q should name the environment variable", err)
	}
}

func mustPreset(t *testing.T, name string) policy.Policy {
	t.Helper()
	p, err := policy.Preset(name)
	if err != nil {
		t.Fatalf("Preset(%q): %v", name, err)
	}
	return p
}
