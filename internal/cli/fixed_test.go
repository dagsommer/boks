package cli

import (
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/dagsommer/boks/internal/runtimecfg"
	"github.com/dagsommer/boks/internal/sandbox"
)

// The annotations are spelled out rather than imported, because they are a contract with the
// runtime rather than an internal name: if internal/sandbox renames its constant the strings
// here must still be the ones nerdbox reads, and a test that follows the rename would hide
// exactly the mistake worth catching.
const (
	cpuAnnotation    = "io.containerd.nerdbox.resources.cpu"
	memoryAnnotation = "io.containerd.nerdbox.resources.memory"
)

// parseRunFlags registers the flags `boks run` registers, on one set, and parses a command
// line into them. It is the walk the user's typing actually takes: the flag as typed, through
// pflag, into the structs the refusal reads — not a hand-built struct that could disagree with
// what `run` accepts. TestRunRegistersEveryFlagThatIsFixedAtCreation keeps the two aligned.
func parseRunFlags(t *testing.T, args ...string) (*sandboxFlags, *policyFlags) {
	t.Helper()
	fs := pflag.NewFlagSet("run", pflag.ContinueOnError)
	dev := &devFlags{}
	dev.register(fs)
	flags := registerSandboxFlags(fs, dev)
	net := &policyFlags{}
	net.register(fs)
	net.registerPublish(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parsing %v: %v", args, err)
	}
	return flags, net
}

// existingSandbox is a sandbox that already exists: wired for NAT, built from the claude
// image with eight vCPUs and 8 GiB — the shape of the measurement this whole mechanism came
// from, where `boks run --cpus 2` against it silently ran with eight.
func existingSandbox() invocation {
	return invocation{
		name:   "claude-proj",
		exists: true,
		info: sandbox.Info{
			Name:        "claude-proj",
			Image:       "ghcr.io/dagsommer/boks-claude:latest",
			Runtime:     runtimecfg.Runtime,
			Snapshotter: runtimecfg.Snapshotter,
			Env:         []string{"PATH=/usr/bin", "FOO=one"},
			Annotations: map[string]string{
				cpuAnnotation:                         "8",
				memoryAnnotation:                      "8192",
				"io.containerd.nerdbox.network.0":     "socket=/x,mode=unixgram,mac=aa:bb:cc:dd:ee:ff",
				"io.containerd.nerdbox.ctr.network.0": "vmmac=aa:bb:cc:dd:ee:ff,addr=192.168.127.2/24",
			},
		},
	}
}

// TestReAttachRefusesEveryFlagFixedAtCreation is the measurement, turned into a test: on
// Windows 11 on 2026-08-15, `boks run --cpus 2` against a sandbox created with `--cpus 8` ran
// with eight vCPUs, silently. So did every other flag written into the container when it was
// built.
//
// One invocation must learn about all of them. Reporting the first would make a user fix them
// one at a time, and each round would have created nothing, so each round would be the same
// message about a different flag.
func TestReAttachRefusesEveryFlagFixedAtCreation(t *testing.T) {
	flags, net := parseRunFlags(t,
		"--cpus", "2",
		"--memory", "4g",
		"--template", "debian:stable",
		"--env", "FOO=two",
		"--annotation", "io.containerd.nerdbox.experiment=1",
		"--net", "none",
	)

	err := checkFixedAtCreation(flags, net, existingSandbox())
	if err == nil {
		t.Fatal("a re-attach that redefined the machine, the image, the environment and the " +
			"network was accepted; every one of those flags would have been silently ignored")
	}
	got := err.Error()

	for _, want := range []string{
		// The flag as the user typed it, so they can find it in their own command line.
		"--cpus 2",
		"--memory 4g",
		"--template debian:stable",
		"--env FOO=two",
		"--annotation io.containerd.nerdbox.experiment=1",
		"--net none",
		// What the sandbox actually is. A user who typed `--cpus 2` needs to read "8".
		"8 vCPU",
		"8192 MiB (8g)",
		"ghcr.io/dagsommer/boks-claude:latest",
		"FOO=one",
		"no io.containerd.nerdbox.experiment",
		"wired for --net nat",
		// And the remedy, spelled out.
		"boks rm claude-proj",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the refusal does not mention %q:\n%s", want, got)
		}
	}
}

// A run that asks for what the sandbox already has is a request that *is* met. Re-running the
// same command line is the ordinary case — it is how a sandbox is re-attached to at all — and
// it must not start failing because the flags are on it.
func TestReAttachAcceptsTheCommandLineThatBuiltTheSandbox(t *testing.T) {
	inv := existingSandbox()
	flags, net := parseRunFlags(t,
		"--cpus", "8",
		"--memory", "8g",
		"--template", inv.info.Image,
		"--env", "FOO=one",
		"--net", "nat",
	)

	if err := checkFixedAtCreation(flags, net, inv); err != nil {
		t.Fatalf("a re-attach asking for exactly what the sandbox has was refused:\n%v", err)
	}
}

// Nothing at all is said when no flag was named: the defaults are not requests, and a bare
// `boks run` must not be refused because its computed memory differs from a sandbox created
// on a different day.
func TestReAttachSaysNothingWhenNoFlagWasNamed(t *testing.T) {
	flags, net := parseRunFlags(t)
	if err := checkFixedAtCreation(flags, net, existingSandbox()); err != nil {
		t.Fatalf("a re-attach that named no flags was refused:\n%v", err)
	}
}

// The create path must keep working exactly as it did: these flags are how a sandbox is
// described in the first place, and a sandbox that does not exist has nothing to disagree
// with. This asserts both halves — no refusal, and the values still reaching the config the
// sandbox is built from.
func TestANewSandboxTakesEveryOneOfThoseFlags(t *testing.T) {
	flags, net := parseRunFlags(t,
		"--cpus", "2",
		"--memory", "4g",
		"--template", "debian:stable",
		"--env", "FOO=two",
		"--net", "none",
	)
	fresh := invocation{name: "claude-proj", exists: false}

	if err := checkFixedAtCreation(flags, net, fresh); err != nil {
		t.Fatalf("a sandbox that does not exist yet was refused its own flags:\n%v", err)
	}

	cfg, err := flags.config(fresh, nil)
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if cfg.CPUs != 2 {
		t.Errorf("Config.CPUs = %d, want 2", cfg.CPUs)
	}
	if cfg.MemoryMiB != 4096 {
		t.Errorf("Config.MemoryMiB = %d, want 4096", cfg.MemoryMiB)
	}
	if cfg.Image != "debian:stable" {
		t.Errorf("Config.Image = %q, want debian:stable", cfg.Image)
	}
	if !strings.Contains(strings.Join(cfg.Env, " "), "FOO=two") {
		t.Errorf("Config.Env = %v, want it to carry FOO=two", cfg.Env)
	}
}

// A sandbox that recorded nothing to compare against — one created before Boks wrote these
// annotations — must not be refused over a comparison Boks made up.
func TestReAttachIgnoresWhatTheSandboxNeverRecorded(t *testing.T) {
	inv := invocation{name: "old", exists: true, info: sandbox.Info{Name: "old"}}
	flags, net := parseRunFlags(t, "--cpus", "2", "--memory", "4g", "--template", "debian:stable",
		"--env", "FOO=two", "--annotation", "a=b")

	if err := checkFixedAtCreation(flags, net, inv); err != nil {
		t.Fatalf("a sandbox with nothing recorded was refused:\n%v", err)
	}
}

// `--cpus 0` is not "unset": it asks for every host CPU. It is compared as the number it
// resolves to, or a sandbox pinned to one vCPU would silently accept a request for the whole
// machine — the same bug in the one place a zero could hide it.
func TestCpusZeroIsARequestForEveryHostCPU(t *testing.T) {
	flags, net := parseRunFlags(t, "--cpus", "0")

	mismatch := existingSandbox()
	mismatch.info.Annotations[cpuAnnotation] = strconv.Itoa(autoCPUs() + 1)
	err := checkFixedAtCreation(flags, net, mismatch)
	if err == nil {
		t.Fatal("--cpus 0 against a sandbox with a different vCPU count was accepted")
	}
	if !strings.Contains(err.Error(), "--cpus 0") {
		t.Errorf("the refusal does not name the flag as typed:\n%v", err)
	}

	match := existingSandbox()
	match.info.Annotations[cpuAnnotation] = strconv.Itoa(autoCPUs())
	if err := checkFixedAtCreation(flags, net, match); err != nil {
		t.Fatalf("--cpus 0 against a sandbox that already has every host CPU was refused:\n%v", err)
	}
}

// The -net refusal moved here from networkFor, so that it is reported alongside the rest
// rather than instead of them. The property it was written for has to survive the move: an
// explicit -net that disagrees with the wiring stops the run, in either direction, and names
// the mode the sandbox actually has.
func TestReAttachRefusesANetworkModeItCannotApply(t *testing.T) {
	inv := existingSandbox()

	for _, mode := range []string{"none", "nat"} {
		flags, net := parseRunFlags(t, "--net", mode)
		err := checkFixedAtCreation(flags, net, inv)
		if mode == "nat" {
			if err != nil {
				t.Errorf("-net nat on a nat sandbox was refused:\n%v", err)
			}
			continue
		}
		if err == nil {
			t.Fatal("-net none against a sandbox wired for NAT was accepted; the flag would " +
				"have appeared to be obeyed while the container stayed connected")
		}
		for _, want := range []string{"--net none", "wired for --net nat", "boks rm claude-proj"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal does not mention %q:\n%v", want, err)
			}
		}
	}

	// A sandbox Boks never wired is not "no network": it is on the runtime's own
	// transport, where the guest reaches the host's loopback. An explicit -net must not
	// look obeyed there either.
	legacy := invocation{name: "old", exists: true, info: sandbox.Info{Name: "old"}}
	flags, net := parseRunFlags(t, "--net", "none")
	err := checkFixedAtCreation(flags, net, legacy)
	if err == nil {
		t.Fatal("-net none against an unwired sandbox was accepted")
	}
	if !strings.Contains(err.Error(), "runtime's default transport") {
		t.Errorf("the refusal does not say what such a sandbox is actually on:\n%v", err)
	}
}

// The hidden development flags are recorded on the container too. A developer who passes
// --runtime or --snapshotter to a sandbox that exists is testing something other than what
// they think they are.
func TestReAttachRefusesTheDevelopmentFlagsItCannotApply(t *testing.T) {
	flags, net := parseRunFlags(t, "--runtime", "io.containerd.runc.v2", "--i-know-this-is-not-isolated")
	err := checkFixedAtCreation(flags, net, existingSandbox())
	if err == nil {
		t.Fatal("--runtime against an existing sandbox was accepted")
	}
	if !strings.Contains(err.Error(), runtimecfg.Runtime) {
		t.Errorf("the refusal does not name the runtime the sandbox actually runs on:\n%v", err)
	}
}

// The worst form of this bug is not a resource that differs but the boundary the tool exists
// for, missing while everything says it is there: a sandbox created with a container-only
// runtime came back up under that runtime for a later `boks run` that named no flags, and
// passed the isolation check on the strength of a default it was not using.
func TestReAttachRefusesASandboxWithNoVMBoundary(t *testing.T) {
	inv := existingSandbox()
	inv.info.Runtime = "io.containerd.runc.v2"

	flags, net := parseRunFlags(t)
	err := checkFixedAtCreation(flags, net, inv)
	if err == nil {
		t.Fatal("a re-attach to a sandbox with no VM boundary was accepted as if it were isolated")
	}
	for _, want := range []string{"io.containerd.runc.v2", runtimecfg.Runtime,
		"--i-know-this-is-not-isolated", "boks rm claude-proj"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q:\n%v", want, err)
		}
	}

	// The opt-out is the same one the create-time refusal names, and it works here too:
	// the two are one decision asked at two moments.
	flags, net = parseRunFlags(t, "--i-know-this-is-not-isolated")
	if err := checkFixedAtCreation(flags, net, inv); err != nil {
		t.Fatalf("the opt-out did not permit a re-attach it permits at creation:\n%v", err)
	}
}

// The helper above registers the flags rather than driving the real command, because the real
// command reaches containerd before it reaches the refusal. This is what keeps the two the
// same: every flag the refusal knows about has to exist on `run`, spelled the same way.
func TestRunRegistersEveryFlagThatIsFixedAtCreation(t *testing.T) {
	cmd := newRunCommand(Env{Stdout: io.Discard, Stderr: io.Discard}, &devFlags{})
	// Persistent flags of the root are merged into a command's set by cobra; the
	// development flags live there, so they are added the way cobra would.
	root := &cobra.Command{}
	dev := &devFlags{}
	dev.register(root.PersistentFlags())
	cmd.Flags().AddFlagSet(root.PersistentFlags())

	for _, name := range []string{
		"template", "cpus", "memory", "env", "annotation", "net", "runtime", "snapshotter",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("boks run has no --%s, but the re-attach refusal names it", name)
		}
	}
}
