package enforce

// The supervisor's bounded wait for a VM to turn up on the link socket.
//
// # Why a wait exists at all, and only on one platform
//
// The supervisor binds the link socket, reports ready, and then does nothing about the link
// for the rest of its life: the VM connects during boot, whenever boot happens to be, and the
// accept loop takes it. That is right where a guest has been watched crossing this link. If
// the VM does not connect there, it is because it did not boot, and containerd says so to the
// command that started it.
//
// Windows is not in that position. Nothing has ever been observed putting an Ethernet frame on
// libkrun's virtio-net device there (network.Unexercised), so "the socket is bound and nobody
// came" is the *likely* outcome of the first attempt rather than an impossible one. Two things
// make silence the wrong response to it:
//
//   - **A shim that ignores the network annotations does not leave the guest disconnected.**
//     It leaves it on libkrun's TSI, where the guest's AF_INET calls are performed on the host
//     and the guest's 127.0.0.1 is the host's. The sandbox looks like it is working. Nothing
//     is enforced. The only signal Boks has that this happened is that nothing ever connected
//     to the link socket, so that signal has to be acted on rather than logged.
//   - **The supervisor would otherwise wait for the sandbox's task to exit** — up to
//     TaskAppearTimeout for it to appear and then indefinitely — holding a stack that
//     terminates nothing, for a VM that will never dial.
//
// # Why the clock starts when the task starts
//
// The obvious timer — from the moment the socket is bound — is wrong here, and the reason is
// the order the CLI does things in. The network is started *before* the container is created,
// because the VM connects to the socket while it boots and a socket that appears late is a
// boot failure rather than a retry; the image pull happens after that. So a wait measured from
// readiness is a wait racing an image pull on an unknown link, which is why TaskAppearTimeout
// is fifteen minutes rather than thirty seconds.
//
// The supervisor already learns when the task appears, because watching the task is what
// bounds its own life. That signal is passed to the watchdog, and the grace period runs from
// there: once containerd says the task is running, a VMM that is going to dial has dialled
// within a couple of seconds, and one that has not is not going to.

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

// linkPeerGrace is how long after the sandbox's task starts the supervisor waits for something
// to connect to the link socket before concluding that nothing will.
//
// It bounds the gap between "containerd reports the task running" and "the VMM has connected
// its virtio-net backend", which on the runs recorded in docs/verification.md is part of a boot
// that completes in about two seconds in total. Thirty is generous by more than an order of
// magnitude on purpose: the cost of being too slow is a diagnostic that arrives late, and the
// cost of being too fast is a false accusation on the one platform where nobody can yet tell
// the difference.
const linkPeerGrace = 30 * time.Second

// linkWatchdog watches for a peer on the link socket, and fails the supervisor if none
// arrives after the sandbox's task is running.
//
// A watchdog that is not armed has nil channels, which is the whole of its Unix behaviour: a
// receive on a nil channel blocks forever, so its arm of the supervisor's select can never be
// chosen, and taskStarted does nothing. Nothing is started, nothing is timed, and no goroutine
// exists.
type linkWatchdog struct {
	started chan struct{}
	failed  chan error
	once    sync.Once
}

// taskStarted is called by the task watch the first time it sees the sandbox running. It is
// safe to call more than once and from any goroutine, because the watch's contract is only
// that it calls this at least once when the task appears.
func (w *linkWatchdog) taskStarted() {
	if w.started == nil {
		return
	}
	w.once.Do(func() { close(w.started) })
}

// failure returns the channel a bounded wait reports on. It is nil unless the watchdog is
// armed, so a select on it is a no-op on every platform but Windows.
func (w *linkWatchdog) failure() <-chan error { return w.failed }

// watchLink arms a watchdog where nothing has been seen carrying frames on this link, and
// returns an inert one everywhere else.
//
// unexercised is network.Unexercised()'s answer, passed in rather than read here so that the
// armed behaviour can be constructed and tested on a machine where it is nil.
func watchLink(ctx context.Context, unexercised error, sandbox, socket string,
	connected <-chan struct{}, grace time.Duration, log io.Writer) *linkWatchdog {
	if unexercised == nil {
		return &linkWatchdog{}
	}
	w := &linkWatchdog{started: make(chan struct{}), failed: make(chan error, 1)}
	go func() {
		// Nothing is timed until the task is running: before that, the wait is for an
		// image pull of unknown length, not for a VM.
		select {
		case <-connected:
			logLine(log, "network: the guest attached to the link socket %s", socket)
			return
		case <-ctx.Done():
			return
		case <-w.started:
		}

		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-connected:
			logLine(log, "network: the guest attached to the link socket %s", socket)
		case <-ctx.Done():
		case <-timer.C:
			w.failed <- noPeerError(sandbox, socket, grace, unexercised)
		}
	}()
	return w
}

// noPeerError says what did not happen, in the order a person would check it.
//
// It is a function in a file with no build constraint so that the text can be read by a test
// on the machine the tests run on. The Windows build is compiled in CI and executed nowhere;
// an error message that only exists behind a platform tag is a message nothing checks.
func noPeerError(sandbox, socket string, waited time.Duration, unexercised error) error {
	return fmt.Errorf(""+
		"sandbox %q is running, but nothing connected to its link socket %s in the %s after it "+
		"started, so this stack is terminating nothing and the sandbox's network policy is not "+
		"being enforced.\n\n"+
		"%v\n\n"+
		"Check, in this order:\n"+
		"  1. that the containerd-shim-nerdbox-v1 on containerd's PATH is a build that carries "+
		"the external network provider. A shim without it ignores the two "+
		"io.containerd.nerdbox.network annotations and falls back to libkrun's TSI, which is not "+
		"a network boks can see or filter.\n"+
		"  2. that krun.dll was built with `--features blk,net`, so that it has a virtio-net "+
		"device to attach at all, and that it exports krun_add_net_unixstream. `boks doctor` "+
		"reports where the shim will find krun.dll; what that DLL was built with is not "+
		"something boks can see, so check the build you installed.\n"+
		"  3. the shim's own log, for a failure to connect to %s.\n\n"+
		"Until a frame is seen arriving here, do not treat this sandbox as contained: stop it "+
		"rather than trusting it.\n"+
		"  boks stop %s",
		sandbox, socket, waited, unexercised, socket, sandbox)
}

func logLine(w io.Writer, format string, args ...any) {
	if w == nil {
		return
	}
	fmt.Fprintf(w, format+"\n", args...)
}
