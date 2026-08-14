package enforce

// The control socket is not bound on Windows, and this file is the argument for that.
//
// It became a live question with the change that let `boks run` attempt a sandbox on Windows.
// Before it, no supervisor ever started here, so the fact that neither of the socket's two
// protections works on Windows was inert. It is not inert now: the supervisor binds a link
// socket, and it would bind this one beside it.
//
// **Both of the controls control.go names are absent here.**
//
//   - *The 0700 directory and the 0600 socket.* Windows ignores the permission bits Go passes
//     to MkdirAll and OpenFile; os.Chmod there sets the read-only attribute and nothing else.
//     The modes serveControl sets are not enforced by anything. What does keep other users out
//     of `%LocalAppData%\boks` is the ACL inherited from the user's profile — a real control,
//     but a different one, belonging to the path rather than to anything Boks does, and one
//     that moves the moment BOKS_STATE_DIR points somewhere else.
//   - *The peer credential check.* peerUID cannot be implemented for AF_UNIX on Windows: the
//     implementation carries no SO_PEERCRED, no SCM_CREDENTIALS and no ancillary data at all.
//     GetNamedPipeClientProcessId — the API that would answer this question — is a *named
//     pipe* call and does not apply to an AF_UNIX socket. So the second opinion is not merely
//     unimplemented here, it is unavailable in this shape.
//
// That leaves a socket whose whole protection would be "it happens to sit in a directory
// Windows protects", with nothing in Boks asserting it and nothing able to notice if it
// stopped being true. The socket exists to let a local process open a hole into a running VM
// (`boks ports --publish`), so that is not a good enough trade, and refusing costs less than
// it looks: the ports a sandbox is started with are bound by the supervisor itself and work
// without this, and only *changing* the set on a running sandbox needs it.
//
// # What would lift this
//
// A named pipe, which is Windows' own answer to both halves at once. `\\.\pipe\boks-<sandbox>`
// can be created with an explicit security descriptor naming the creating user's SID, and
// GetNamedPipeClientProcessId gives the server the client's process id, from which
// OpenProcessToken gives its user SID — the exact second opinion peerUID provides on Linux and
// Darwin. It needs a Windows named-pipe implementation on both ends (go-winio, or the syscalls
// directly) and it needs to be testable; it is not a line of code, which is why this refuses
// rather than half-doing it.
//
// **Nothing here has been executed on Windows.**

// controlSocketSecurable reports whether the supervisor's control socket can be given the
// protections control.go claims for it. On Windows it cannot, and this says so in the words
// the supervisor logs and `boks ports` prints.
func controlSocketSecurable() error { return controlSocketRefusal() }
