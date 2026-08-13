package cli

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// TestPolicyLogFilters: the log is a global firehose across every sandbox ever run, and it
// had only --limit and --raw. A tester debugging one run was reading another sandbox's
// traffic from four hours earlier.
func TestPolicyLogFilters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	sink, err := policy.NewFileSink(path)
	if err != nil {
		t.Fatalf("NewFileSink: %v", err)
	}
	old := time.Now().Add(-4 * time.Hour)
	for _, d := range []policy.Decision{
		{Time: old, Type: policy.TypeNetwork, Host: "stale.test", Port: 443, Sandbox: "other", Reason: "denied"},
		{Time: time.Now(), Type: policy.TypeNetwork, Host: "wanted.test", Port: 443, Sandbox: "web", Reason: "denied"},
		{Time: time.Now(), Type: policy.TypeNetwork, Host: "noise.test", Port: 443, Sandbox: "other", Reason: "denied"},
	} {
		sink.Record(d)
	}
	sink.Close()

	out, _, err := runCLI(t, "", "policy", "log", "--file", path, "--sandbox", "web")
	if err != nil {
		t.Fatalf("policy log --sandbox: %v", err)
	}
	if !strings.Contains(out, "wanted.test") || strings.Contains(out, "noise.test") || strings.Contains(out, "stale.test") {
		t.Errorf("--sandbox did not narrow to one sandbox:\n%s", out)
	}

	out, _, err = runCLI(t, "", "policy", "log", "--file", path, "--since", "1h")
	if err != nil {
		t.Fatalf("policy log --since: %v", err)
	}
	if strings.Contains(out, "stale.test") {
		t.Errorf("--since 1h kept a decision from four hours ago:\n%s", out)
	}
	if !strings.Contains(out, "wanted.test") || !strings.Contains(out, "noise.test") {
		t.Errorf("--since 1h dropped decisions inside the window:\n%s", out)
	}

	// A filter that matches nothing has to say so as a filter result, or "no decisions"
	// reads as "nothing was recorded" and sends the user looking for the wrong bug.
	out, _, err = runCLI(t, "", "policy", "log", "--file", path, "--sandbox", "ghost")
	if err != nil {
		t.Fatalf("policy log --sandbox ghost: %v", err)
	}
	if !strings.Contains(out, "for sandbox ghost") || !strings.Contains(out, "the filter excluded") {
		t.Errorf("an empty filter result does not say it was filtered:\n%s", out)
	}

	// The limit applies to what survived the filter, not to what was read: tailing first
	// would throw away the decision the filter was looking for.
	out, _, err = runCLI(t, "", "policy", "log", "--file", path, "--sandbox", "web", "--limit", "1")
	if err != nil {
		t.Fatalf("policy log --sandbox --limit: %v", err)
	}
	if !strings.Contains(out, "wanted.test") {
		t.Errorf("--limit was applied before the filter:\n%s", out)
	}

	if _, _, code := mainExitCode(t, "policy", "log", "--file", path, "--since", "yesterday"); code != 2 {
		t.Errorf("an unreadable --since exited %d, want 2", code)
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

// A forgotten passphrase used to be a dead end reachable in one step: every subcommand has
// to decrypt, `rm` included, so the remedy for "wrong passphrase" was the command that had
// just failed, and `ls` failed the same way. There was no move left inside the CLI.
func TestAForgottenPassphraseHasAWayOut(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BOKS_STATE_DIR", dir)

	t.Setenv(secret.PassphraseEnv, "the-right-one")
	if _, _, err := runCLI(t, "value\n", "secret", "set", "github"); err != nil {
		t.Fatalf("secret set: %v", err)
	}

	t.Setenv(secret.PassphraseEnv, "not-the-right-one")
	for _, args := range [][]string{{"secret", "ls"}, {"secret", "rm", "github"}, {"secret", "set", "other"}} {
		_, _, err := runCLI(t, "value\n", args...)
		if err == nil {
			t.Fatalf("boks %v succeeded with the wrong passphrase", args)
		}
		// The remedy has to name the file, say what deleting it costs, and give the
		// command — none of which the user can be expected to know otherwise.
		for _, want := range []string{"secrets.json", "boks secret reset --force", "set again"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("boks %v: the error does not say %q:\n%v", args, want, err)
			}
		}
	}

	// The way out must not itself need the passphrase, or it is not a way out.
	t.Setenv(secret.PassphraseEnv, "")
	if _, _, err := runCLI(t, "", "secret", "reset"); err == nil {
		t.Error("secret reset destroyed the store without --force")
	}
	out, _, err := runCLI(t, "", "secret", "reset", "--force")
	if err != nil {
		t.Fatalf("secret reset --force: %v", err)
	}
	if !strings.Contains(out, "deleted") {
		t.Errorf("secret reset --force said %q", out)
	}
	if _, err := os.Stat(filepath.Join(dir, "secrets.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the store is still there after a reset: %v", err)
	}

	// And afterwards the store is usable again, with whatever passphrase you now have.
	t.Setenv(secret.PassphraseEnv, "a-new-one")
	if _, _, err := runCLI(t, "value\n", "secret", "set", "github"); err != nil {
		t.Fatalf("the store was not usable after a reset: %v", err)
	}
	// Resetting a store that is not there is a statement, not a failure.
	if _, _, err := runCLI(t, "", "secret", "reset", "--force"); err != nil {
		t.Fatal(err)
	}
	out, _, err = runCLI(t, "", "secret", "reset", "--force")
	if err != nil || !strings.Contains(out, "nothing to reset") {
		t.Errorf("resetting an absent store = %q, %v", out, err)
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

// `-t` means --template on run and --tty on exec, and that is not going to change: both
// spellings are sbx's. What has to change is what happens when someone types Docker's `-it`
// at `boks run`, which used to fail two different ways — `-it` with an unhelpful "unknown
// shorthand flag: 'i'", and `-ti` not failing at all, because it set --template to "i" and
// sent the user off to debug a missing image.
func TestDockerTerminalFlagsOnRunAreExplained(t *testing.T) {
	for _, arg := range []string{"-it", "-ti", "-i"} {
		out, errOut, code := mainExitCode(t, "run", arg, ".")
		if code != 2 {
			t.Errorf("boks run %s . exited %d, want 2", arg, code)
		}
		// The answer has to name both halves of the confusion: what -t is here, and
		// where a terminal actually comes from.
		for _, want := range []string{"--template", "boks exec -it", "no -i or -t terminal flags"} {
			if !strings.Contains(errOut, want) {
				t.Errorf("boks run %s: the message does not say %q:\n%s", arg, want, errOut)
			}
		}
		if strings.Contains(out, "\n") {
			t.Errorf("boks run %s wrote to stdout: %q", arg, out)
		}
	}

	// `create` takes the same flags as `run` and has the same trap.
	if _, errOut, code := mainExitCode(t, "create", "-it", "."); code != 2 || !strings.Contains(errOut, "--template") {
		t.Errorf("boks create -it exited %d:\n%s", code, errOut)
	}

	// The flags this guard is *not* about must be untouched: -t with a value is the
	// template flag doing its job, and `exec -it` is the documented way to get a terminal.
	if _, errOut, code := mainExitCode(t, "run", "-t", "example.com/img:1", "--help"); code != 0 {
		t.Errorf("boks run -t IMAGE --help exited %d:\n%s", code, errOut)
	}
	if _, errOut, _ := mainExitCode(t, "exec", "-it", "web", "sh"); strings.Contains(errOut, "no -i or -t terminal flags") {
		t.Errorf("the guard reached into 'exec', where -it is real:\n%s", errOut)
	}
	// And nothing after `--` is ours: an agent may have its own -i.
	if _, errOut, _ := mainExitCode(t, "run", "shell", ".", "--", "-it"); strings.Contains(errOut, "no -i or -t terminal flags") {
		t.Errorf("the guard reached past `--` into the agent's own arguments:\n%s", errOut)
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
