package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/runtimecfg"
)

// Some of what `boks run` accepts is decided when a sandbox is created and cannot be revisited
// when one is re-attached to. The image is a snapshot taken once; the vCPU count, the memory
// size and every other annotation are read when the VM is built; the environment is written
// into the container's OCI process spec; the network wiring is annotations the runtime reads
// at boot. containerd writes all of that at creation and never looks at it again, so a second
// `boks run` that names one of those flags is asking for something that will not happen.
//
// Until this existed, what happened was nothing at all. `boks run --cpus 2` against a sandbox
// created with `--cpus 8` ran with 8 — measured on Windows 11 on 2026-08-15, from `dmesg`
// ("smp: Brought up 1 node, 8 CPUs") and from eight per-CPU columns in /proc/interrupts. No
// warning, no error: the flag was accepted and ignored. That is the third instance of one
// bug, after `-net` and after the agent name, so this is written as the general answer rather
// than as a third special case.
//
// # Which flags refuse, and which only say something
//
// The test is containment, and it is asymmetric on purpose: a flag whose being ignored leaves
// the user with *less* than they asked for is a refusal, and a flag whose being ignored leaves
// them with *more* is a note.
//
//   - Refused, because the sandbox would be materially different from the one described on the
//     command line: --template, --cpus, --memory, --env, --annotation, --net, and the hidden
//     --runtime/--snapshotter. `--net none` is the sharpest of them — a user who asks for no
//     network and silently gets one is worse off than they believe they are — but `--cpus 2`
//     on an eight-vCPU sandbox is the same shape: the machine that runs is not the machine
//     that was asked for, and nothing said so.
//
//   - A note, because ignoring them errs towards containment: --publish (see publishFor —
//     fewer ports are published than asked for, and the ports of a running sandbox are live
//     state rather than a property of whichever terminal reached it), --clone (see
//     applyCloneMode) and the workspace positionals (see sandbox.warnWorkspaceMismatch — a
//     directory that is not shared is a directory the guest cannot touch).
//
//   - Applied, and therefore not here at all: --policy, --profile, --allow, --deny, --inject,
//     --guest-credential, --oauth and --no-secrets. Those resolve into the stack this run
//     builds rather than into the container, and the merge rules in policyFlags.resolution
//     only ever narrow what a sandbox may reach.
//
// A mismatch is only a mismatch against a value the sandbox actually recorded. Re-running the
// same command line — the ordinary case, and the one that must not start failing — asks for
// what the sandbox already has and passes in silence.

// fixedFlag is one flag this invocation named that the sandbox cannot honour.
type fixedFlag struct {
	// asked is the flag as the user typed it, so that they can find it in their own
	// command line rather than translate from ours.
	asked string
	// has is what the sandbox is, in the same units the flag takes where there are any.
	has string
}

// fixedConflicts collects every such flag, so that one run reports all of them.
//
// Reporting them one at a time would make a user who passed three fix them in three
// invocations, learning about the next only after removing the previous — and each of those
// invocations would have created nothing, so they would be three rounds of the same message.
type fixedConflicts struct {
	sandbox string
	entries []fixedFlag
}

func (c *fixedConflicts) add(asked, has string) {
	c.entries = append(c.entries, fixedFlag{asked: asked, has: has})
}

// err renders the collected conflicts, or nil when there are none.
//
// The text owes the user three things, which is what the -net refusal already gave them: the
// flag they typed, the value the sandbox actually has — someone who typed `--cpus 2` needs to
// read "8", not "a different number" — and the one command that changes it.
func (c *fixedConflicts) err() error {
	if len(c.entries) == 0 {
		return nil
	}
	width := 0
	for _, e := range c.entries {
		if len(e.asked) > width {
			width = len(e.asked)
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "sandbox %q already exists, and these are fixed when a sandbox is created:\n\n", c.sandbox)
	for _, e := range c.entries {
		fmt.Fprintf(&b, "  %-*s   %s\n", width, e.asked, e.has)
	}
	fmt.Fprintf(&b, "\n"+
		"They live in the container's OCI spec and the runtime annotations read when the VM\n"+
		"is built, both written once and never revisited, so a re-attach cannot apply them.\n"+
		"Boks refuses rather than running a sandbox that is not the one you described.\n\n"+
		"Remove it and run again to get them:\n"+
		"  boks rm %s\n"+
		"Or leave it alone and build a second sandbox beside it: boks run --name NEW ...\n"+
		"'boks inspect %s' shows what this one was created with.", c.sandbox, c.sandbox)
	return fmt.Errorf("%s", b.String())
}

// checkFixedAtCreation refuses an invocation whose flags a sandbox that already exists cannot
// honour. It must run before anything is pulled, created or started, and it is a no-op for a
// sandbox that does not exist yet — on the create path every one of these flags is applied
// exactly as it always was.
func checkFixedAtCreation(f *sandboxFlags, net *policyFlags, inv invocation) error {
	if !inv.exists {
		return nil
	}
	if err := checkRuntimeIsolation(f.dev, inv); err != nil {
		return err
	}
	c := fixedConflicts{sandbox: inv.name}
	checkImage(f, inv, &c)
	checkResources(f, inv, &c)
	checkEnv(f, inv, &c)
	checkAnnotations(f, inv, &c)
	checkNetworkMode(net, inv, &c)
	checkRuntimeFlags(f, inv, &c)
	checkCloneMode(f, inv, &c)
	return c.err()
}

// checkImage compares the requested image with the one the sandbox's filesystem came from.
//
// The rootfs is a snapshot of that image, made once. `-t debian:stable` on a sandbox built
// from the agent's own image does not produce Debian; it produces the sandbox that is already
// there, running a command line written for a different filesystem.
func checkImage(f *sandboxFlags, inv invocation, c *fixedConflicts) {
	if !f.changed("template") || inv.info.Image == "" || f.image == inv.info.Image {
		return
	}
	c.add("--template "+f.image, "the sandbox runs "+inv.info.Image)
}

// checkResources compares the requested vCPUs and memory with what the VM is sized from.
//
// Both are only compared against a value the sandbox actually recorded: a sandbox created
// before Boks wrote these annotations has nothing to disagree with, and inventing a number for
// it would refuse a run over a comparison Boks made up.
func checkResources(f *sandboxFlags, inv invocation, c *fixedConflicts) {
	if f.changed("cpus") {
		if existing, ok := inv.info.CPUs(); ok && existing != resolvedCPUs(f.cpus) {
			c.add(fmt.Sprintf("--cpus %d", f.cpus), fmt.Sprintf("the sandbox has %d vCPU(s)", existing))
		}
	}
	if !f.changed("memory") {
		return
	}
	// A memory size that does not parse is not a mismatch to report: the parse error is
	// the better message, and flags.config produces it a moment later.
	requested, err := parseMemory(f.memory)
	if err != nil {
		return
	}
	if existing, ok := inv.info.MemoryMiB(); ok && existing != requested {
		c.add("--memory "+f.memory, "the sandbox has "+formatMiB(existing))
	}
}

// resolvedCPUs is what a --cpus value becomes, so that `--cpus 0` on a four-CPU host is
// compared as the four vCPUs it asks for rather than as a zero the sandbox can never hold.
func resolvedCPUs(cpus int) int {
	if cpus == 0 {
		return autoCPUs()
	}
	return cpus
}

// formatMiB renders a size the way the flag would take it back, since a user who typed "8g"
// should not have to divide 8192 by 1024 to see whether it matched.
func formatMiB(mib int) string {
	if mib%1024 == 0 {
		return fmt.Sprintf("%d MiB (%dg)", mib, mib/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}

// checkEnv compares each requested variable with the container's process environment.
//
// The environment is written into the OCI spec at creation, and `boks run` execs into the
// container without adding to it, so a --env on a re-attach reaches nothing. An agent that
// silently lacks the token or the proxy setting it was told to have is the failure this
// prevents; `boks exec -e` is the way to give one command a variable of its own.
func checkEnv(f *sandboxFlags, inv invocation, c *fixedConflicts) {
	if !f.changed("env") || len(inv.info.Env) == 0 {
		return
	}
	for _, entry := range f.env {
		key, _, found := strings.Cut(entry, "=")
		if !found || key == "" {
			// Malformed: parseKeyValues rejects it with a better message.
			continue
		}
		existing, present := lookupEnv(inv.info.Env, key)
		if present && existing == entry {
			continue
		}
		if present {
			c.add("--env "+entry, "the sandbox has "+existing)
			continue
		}
		c.add("--env "+entry, "the sandbox has no "+key)
	}
}

func lookupEnv(env []string, key string) (string, bool) {
	for _, entry := range env {
		if name, _, found := strings.Cut(entry, "="); found && name == key {
			return entry, true
		}
	}
	return "", false
}

// checkAnnotations compares each requested annotation with the ones on the container.
//
// --annotation is the escape hatch for a runtime capability Boks has no flag for, and it wins
// over the computed ones — including the resource annotations. A user reaching for it on a
// re-attach is trying to change the VM, which is exactly what cannot be done.
func checkAnnotations(f *sandboxFlags, inv invocation, c *fixedConflicts) {
	if !f.changed("annotation") || inv.info.Annotations == nil {
		return
	}
	requested, err := parseKeyValues(f.annotations)
	if err != nil {
		// Malformed: flags.config reports it, with the offending entry.
		return
	}
	keys := make([]string, 0, len(requested))
	for key := range requested {
		keys = append(keys, key)
	}
	// Deterministic order: a map's is not, and an error message that reorders itself
	// between runs is one a user cannot diff or search for.
	sort.Strings(keys)
	for _, key := range keys {
		existing, present := inv.info.Annotations[key]
		if present && existing == requested[key] {
			continue
		}
		if present {
			c.add("--annotation "+key+"="+requested[key], "the sandbox has "+key+"="+existing)
			continue
		}
		c.add("--annotation "+key+"="+requested[key], "the sandbox has no "+key)
	}
}

// checkNetworkMode compares the requested mode with the wiring the container carries.
//
// This is the case the whole mechanism was generalised from, and the refusal it used to make
// on its own — in networkFor — is now one entry among the rest. The disagreement is refused in
// either direction: `-net none` against a NAT sandbox is a containment failure, and `-net nat`
// against a `none` sandbox is not, but it is still a request that will not be met, and finding
// that out here beats finding it out from a connection that mysteriously fails in the guest.
//
// A sandbox with no Boks wiring at all is not "no network": it is on the runtime's own
// transport, where the guest's 127.0.0.1 is the host's. An explicit `-net` against one of those
// is refused too, rather than appearing to be obeyed by the least contained thing Boks can
// produce. A run that passes no `-net` still gets the full warning about it, from networkFor.
func checkNetworkMode(net *policyFlags, inv invocation, c *fixedConflicts) {
	if net == nil || net.mode == "" {
		return
	}
	requested, err := network.ParseMode(net.mode)
	if err != nil {
		// Malformed: `run` has already rejected it by the time this is reached, and
		// `create` never gets here.
		return
	}
	existing, wired := network.ModeFromAnnotations(inv.info.Annotations)
	if !wired {
		c.add("--net "+string(requested),
			"the sandbox is on the runtime's default transport, with no Boks network at all")
		return
	}
	if existing != requested {
		c.add("--net "+string(requested), "the sandbox is wired for --net "+string(existing))
	}
}

// checkRuntimeFlags covers the two hidden development flags that are recorded on the
// container: the runtime handler and the snapshotter. Both are chosen once, and a developer
// who passes either to a sandbox that exists is testing something other than what they think.
func checkRuntimeFlags(f *sandboxFlags, inv invocation, c *fixedConflicts) {
	if f.dev == nil {
		return
	}
	if f.changed("runtime") && inv.info.Runtime != "" && f.dev.runtimeID != inv.info.Runtime {
		c.add("--runtime "+f.dev.runtimeID, "the sandbox runs on "+inv.info.Runtime)
	}
	if f.changed("snapshotter") && inv.info.Snapshotter != "" && f.dev.snapshotter != inv.info.Snapshotter {
		c.add("--snapshotter "+f.dev.snapshotter, "the sandbox uses "+inv.info.Snapshotter)
	}
}

// checkRuntimeIsolation refuses to re-attach to a sandbox that has no VM boundary without the
// same acknowledgement it took to create one.
//
// devFlags.requireIsolation judges the *flag*, which is the only thing there is to judge when
// a sandbox is being created. On a re-attach the flag decides nothing — the container already
// has a runtime — so a sandbox created with `--runtime io.containerd.runc.v2` came back up
// under runc for a later `boks run` that named no flags at all, and passed the isolation check
// on the strength of a default it was not using. That is this same bug in its worst form: not
// a resource that differs, but the boundary the tool exists for, missing while everything
// says it is there.
//
// It is a refusal rather than a warning, and it names the same opt-out the create-time refusal
// names, because the two are the same decision asked at two moments.
func checkRuntimeIsolation(dev *devFlags, inv invocation) error {
	if dev == nil || dev.insecure || inv.info.Runtime == "" || runtimecfg.IsolatedRuntime(inv.info.Runtime) {
		return nil
	}
	return fmt.Errorf(
		"sandbox %q was created with runtime %q, which does not provide a virtual machine\n"+
			"boundary, and a sandbox's runtime is fixed when it is created. Boks refuses to\n"+
			"re-attach to it as if it were isolated. The isolating runtime is %q.\n"+
			"Remove it and run again to get a real sandbox: boks rm %s\n"+
			"If you are developing Boks itself, pass --i-know-this-is-not-isolated.",
		inv.name, inv.info.Runtime, runtimecfg.Runtime, inv.name)
}

// checkCloneMode compares a --clone request with how the sandbox's workspace actually
// reaches its guest.
//
// This one was a note until 2026-08-15, and the note was the wrong call by this file's own
// rule. Every other flag here is judged by asking which way the silence errs: ignoring
// --publish shares fewer ports, ignoring a workspace shares fewer directories, and a user
// who ends up more contained than they asked for is not harmed by the difference.
// --clone is the opposite shape. Its whole purpose is that guest writes never touch the
// user's disk, so a --clone that is quietly ignored hands the guest a read-write share of
// the very repository the flag was reached for to protect — the same failure --net none
// has, and that one has been an error for exactly this reason.
//
// The mode lives in the container's OCI mounts, written once when the sandbox is created,
// so it cannot be changed on a re-attach; recreating is the only way to get it.
func checkCloneMode(f *sandboxFlags, inv invocation, c *fixedConflicts) {
	if !f.clone || inv.info.Filesystem.IsClone() {
		return
	}
	c.add("--clone", "the sandbox's workspace is shared read-write ("+inv.info.Filesystem.Mode+")")
}
