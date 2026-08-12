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

// runCLI drives the whole command tree the way the binary does, so a test exercises the
// real dispatch, the real flags and the real argument validation rather than one function
// out of context.
func runCLI(t *testing.T, stdin string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	var out, errOut bytes.Buffer
	root := newRootCommand(Env{
		Args:   args,
		Stdin:  strings.NewReader(stdin),
		Stdout: &out,
		Stderr: &errOut,
	})
	root.SetArgs(args)
	root.SetContext(context.Background())
	_, err = root.ExecuteC()
	return out.String(), errOut.String(), err
}

// mainExitCode runs the CLI exactly as cmd/boks does, for the assertions that are about the
// process's exit status rather than its output.
func mainExitCode(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Main(context.Background(), Env{
		Args:   args,
		Stdin:  strings.NewReader(""),
		Stdout: &out,
		Stderr: &errOut,
	})
	return out.String(), errOut.String(), code
}

// The exit codes a script can rely on: 0 for help, 2 for anything the user got wrong about
// how they invoked boks, and 1 for a command that ran and failed.
func TestExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"bare boks", nil, 2},
		{"help", []string{"--help"}, 0},
		{"version", []string{"--version"}, 0},
		{"unknown command", []string{"nosuchthing"}, 2},
		{"unknown flag", []string{"ls", "--nope"}, 2},
		{"missing argument", []string{"stop"}, 2},
		{"contradictory flags", []string{"ls", "-q", "--json"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, code := mainExitCode(t, tt.args...); code != tt.want {
				t.Errorf("boks %v exited %d, want %d", tt.args, code, tt.want)
			}
		})
	}
}

// Help goes to stdout and reads like sbx's, because a habit formed there should work here.
func TestHelpFormatMatchesTheReference(t *testing.T) {
	out, _, code := mainExitCode(t, "--help")
	if code != 0 {
		t.Fatalf("boks --help exited %d", code)
	}
	for _, want := range []string{"Usage:", "Available Commands:", "Flags:", "completion", "run", "-h, --help"} {
		if !strings.Contains(out, want) {
			t.Errorf("boks --help does not contain %q:\n%s", want, out)
		}
	}

	runHelp, _, code := mainExitCode(t, "run", "--help")
	if code != 0 {
		t.Fatalf("boks run --help exited %d", code)
	}
	// The symptom that started this: the template flag has to render with both spellings.
	for _, want := range []string{"-t, --template", "-m, --memory", "-d, --detached"} {
		if !strings.Contains(runHelp, want) {
			t.Errorf("boks run --help does not render %q:\n%s", want, runHelp)
		}
	}
	// The developer flags are hidden from the command that would otherwise list them
	// beside the ones a user is meant to choose between.
	for _, hidden := range []string{"--runtime", "--snapshotter", "--i-know-this-is-not-isolated"} {
		if strings.Contains(runHelp, hidden) {
			t.Errorf("boks run --help lists the developer flag %q:\n%s", hidden, runHelp)
		}
	}
	// Hidden is not gone: they still parse.
	if _, _, code := mainExitCode(t, "ls", "--containerd-address", "/nonexistent.sock", "--help"); code != 0 {
		t.Errorf("a hidden developer flag was rejected, exit %d", code)
	}
}

// `boks list` is `boks ls`, because sbx accepts both.
func TestLsHasTheListAlias(t *testing.T) {
	if _, _, code := mainExitCode(t, "list", "--help"); code != 0 {
		t.Errorf("boks list --help exited %d, want the ls command", code)
	}
}

// Cobra generates the completion scripts; the point of this test is that the command is
// wired in at all, since sbx has one and Boks did not.
func TestCompletionCommandExists(t *testing.T) {
	out, _, code := mainExitCode(t, "completion", "bash")
	if code != 0 {
		t.Fatalf("boks completion bash exited %d", code)
	}
	if !strings.Contains(out, "boks") {
		t.Errorf("the completion script does not mention boks:\n%s", out)
	}
}

func TestPolicyLsShowsResolvedRules(t *testing.T) {
	out, _, err := runCLI(t, "", "policy", "ls", "--policy", "locked", "--allow", "api.example.com:443")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	for _, want := range []string{"api.example.com:443", "default: deny", "deny (always wins)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// The user must be able to read this output and know exactly how far the protection
	// goes: what enforces it, what has been measured, and where that measurement stops.
	for _, want := range []string{"terminated on the host", "Measured against a real guest", "Linux is not covered"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not say %q:\n%s", want, out)
		}
	}
	// A policy written only in hostnames cannot match a raw connection, which surprised a
	// tester badly enough to be worth saying where the rules are shown.
	if !strings.Contains(out, "Every allow rule here names a host") {
		t.Errorf("a hostname-only policy did not warn that raw flows are denied:\n%s", out)
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
	if _, _, err := runCLI(t, "", "policy", "ls", "--allow", "*.*.example.com"); err == nil {
		t.Error("an invalid -allow rule was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "ls", "--policy", "balanced"); err == nil {
		t.Error("an unknown preset was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "ls", "--inject", "tok@*=bearer"); err == nil {
		t.Error("a catch-all injection rule was accepted")
	}
	if _, _, err := runCLI(t, "", "policy", "ls", "--guest-credential", "unknown=placeholder"); err == nil {
		t.Error("a placeholder for a service with no injection rule was accepted")
	}
}

func TestPolicyLogWithoutAFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent.jsonl")
	out, _, err := runCLI(t, "", "policy", "log", "--file", missing)
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

	out, _, err := runCLI(t, "", "policy", "log", "--file", path)
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

	raw, _, err := runCLI(t, "", "policy", "log", "--file", path, "--raw")
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

// A long flag written with one dash used to work, because the stdlib flag package accepted
// it. pflag reads it as a cluster of shorthands, and for `-template` every letter happens to
// be a real shorthand — so it silently set --template to "emplate", left the value as a
// positional, and told the user their *workspace* did not exist. The mistake was the dash,
// and nothing in that message said so.
func TestSingleDashLongFlagIsRejectedByName(t *testing.T) {
	for _, arg := range []string{"-template", "-policy", "-name", "-memory"} {
		_, errOut, code := mainExitCode(t, "run", arg, "value")
		if code != 2 {
			t.Errorf("boks run %s value exited %d, want 2", arg, code)
		}
		if !strings.Contains(errOut, "two dashes") || !strings.Contains(errOut, "--"+strings.TrimPrefix(arg, "-")) {
			t.Errorf("%s: error does not name the fix:\n%s", arg, errOut)
		}
	}

	// A genuine shorthand cluster must survive: -it is two real short flags, not a long
	// name, and rejecting it would break the documented way to get a terminal.
	_, errOut, code := mainExitCode(t, "exec", "-it")
	if code == 2 && strings.Contains(errOut, "two dashes") {
		t.Errorf("the guard rejected the shorthand cluster -it:\n%s", errOut)
	}

	// Nothing after `--` is ours to judge; those flags belong to the agent.
	if _, errOut, _ := mainExitCode(t, "run", "shell", ".", "--", "-template"); strings.Contains(errOut, "two dashes") {
		t.Errorf("the guard reached past `--` into the agent's own arguments:\n%s", errOut)
	}
}
