package enforce

import "errors"

// errLockHeld means the supervisor lock is held by another live process — that is, the
// sandbox really does have a network supervisor.
//
// It exists because "I could not take the lock" and "somebody else has it" are not the same
// statement, and reporting them with one sentence produced an error that was simply false.
// `boks net serve` used to answer every failure of acquire with `sandbox %q already has a
// network supervisor`, so a Windows host — where acquire refuses outright, because there is
// no host-terminated link for a supervisor to own — was told a fresh sandbox already had one
// running. The true cause was wrapped inside as a detail, and the first line of the log sent
// the reader hunting for a process that had never existed.
//
// Only the platform lock primitives may attribute a failure to this sentinel, and only for a
// lock another holder is provably holding. Everything else — a directory that cannot be
// written, a platform that has no supervisor to run, a failure that is yet to exist — keeps
// its own error, and is reported as itself. That distinction gets more valuable, not less,
// as Windows moves from refusing to attempting: a real attempt has real ways to fail, and
// every one of them would otherwise be announced as a supervisor that is already running.
var errLockHeld = errors.New("another process holds the supervisor lock")
