//go:build windows

package secret

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The Windows keyring: the Credential Manager, reached through advapi32's Cred* API.
//
// **What has and has not run here.** Nothing in this file has ever been executed. Not one of
// these functions has been called on a Windows machine, by a test or by hand; the whole of the
// claim is that it compiles for windows/amd64 and windows/arm64 and follows the API as
// Microsoft documents it. The struct below in particular is transcribed from documentation —
// wincred.h's CREDENTIALW — and a struct transcribed wrongly does not fail to compile, it reads
// whatever bytes happen to line up. Treat every statement in this file as a claim about the
// documentation and not as an observation.
//
// What would prove it: a Set/Get/Delete round trip on a real Windows session, run for an ASCII
// value, a non-ASCII one and an empty one, with the entry visible under
// `cmdkey /list:boks:<name>` between the Set and the Delete. internal/secret's own tests are
// the natural place, guarded so they only run on Windows, and the native job in
// .github/workflows/windows.yml is what would run them. Until that exists this backend is
// untested code that happens to build.
//
// The pure parts — the target name and the UTF-16LE encode/decode — deliberately live in
// keyring_credman.go, which has no build constraint, so that they can be tested on the
// machines this project is actually developed on. What is left here is the syscall surface,
// which cannot be.

// The Cred* API is not in golang.org/x/sys/windows: as of v0.46.0 that package exports no
// CredWriteW, CredReadW, CredDeleteW or CredFree (checked against the module cache, not
// assumed), so they are declared here the way x/sys declares the calls it does have.
//
// NewLazySystemDLL rather than NewLazyDLL: it forces the load out of System32 instead of the
// process's search path, which is what stops a DLL named advapi32.dll dropped beside boks.exe
// from being the thing that handles secrets.
var (
	credmanDLL      = windows.NewLazySystemDLL("advapi32.dll")
	procCredWriteW  = credmanDLL.NewProc("CredWriteW")
	procCredReadW   = credmanDLL.NewProc("CredReadW")
	procCredDeleteW = credmanDLL.NewProc("CredDeleteW")
	procCredFree    = credmanDLL.NewProc("CredFree")
)

// The constants, each with where its value comes from. They are written out rather than
// imported because x/sys/windows does not define them either.
const (
	// CRED_TYPE_GENERIC, wincred.h. Generic is the only type the OS itself does not
	// interpret: the domain-password types are consumed by the authentication packages,
	// which impose their own rules on the blob and on UserName. Boks stores opaque
	// strings, so generic is both the correct type and the only one that would accept
	// them.
	credTypeGeneric = 1

	// CRED_PERSIST_LOCAL_MACHINE, wincred.h (SESSION is 1, LOCAL_MACHINE 2, ENTERPRISE 3).
	//
	// ENTERPRISE was considered and rejected. It roams the credential with the domain
	// profile, so a token imported once on a laptop would be copied to the domain
	// controller and to every machine that user logs into — a blast radius Boks would be
	// widening on the user's behalf, silently, for no benefit it can name. The Unix
	// backends do not roam either (a login keychain and a login keyring stay on the
	// machine), so LOCAL_MACHINE is also the choice that keeps the three platforms
	// describable in one sentence.
	//
	// SESSION would drop every secret at logoff, which is not persistence at all.
	//
	// "Local machine" is about lifetime, not about who can read it: the credential still
	// lives in this user's credential set and is still protected by this user's DPAPI key,
	// so another user on the same machine does not get it by being on the same machine.
	credPersistLocalMachine = 2

	// CRED_MAX_CREDENTIAL_BLOB_SIZE, wincred.h: 5*512 = 2560 bytes, the Vista-and-later
	// value (XP's was 512). Over it, CredWriteW fails with ERROR_INVALID_PARAMETER, which
	// says nothing about which parameter — so the limit is checked here in order to say
	// the true thing instead.
	credMaxCredentialBlobSize = 5 * 512
)

// credmanComment is what the Credential Manager UI shows beside the entry. It exists so that a
// user who finds an unexplained credential can tell where it came from and that deleting it is
// a Boks matter, not a Windows one.
const credmanComment = "Managed by boks. Delete with `boks secret rm`."

// credentialW is wincred.h's CREDENTIALW.
//
// Transcribed from Microsoft's documentation of the structure; never verified against a real
// one. Field order and the documented C type of each field:
//
//	Flags              DWORD                  — bit flags; none apply to generic credentials
//	Type               DWORD                  — CRED_TYPE_*
//	TargetName         LPWSTR                 — the key the store is indexed by
//	Comment            LPWSTR                 — free text shown in the UI
//	LastWritten        FILETIME               — set by the OS on write, ignored on input
//	CredentialBlobSize DWORD                  — the size of the blob in BYTES
//	CredentialBlob     LPBYTE                 — the secret itself, an uninterpreted blob
//	Persist            DWORD                  — CRED_PERSIST_*
//	AttributeCount     DWORD                  — length of Attributes
//	Attributes         PCREDENTIAL_ATTRIBUTEW — application-defined extras; unused here
//	TargetAlias        LPWSTR                 — an alternate name; unused here
//	UserName           LPWSTR                 — the account the credential is for
//
// The padding is left to the compiler on purpose. Go lays out a struct with each field at its
// natural alignment, which is what MSVC does for this header, so the four bytes the 64-bit ABI
// needs between CredentialBlobSize and CredentialBlob appear without being written — and
// writing an explicit pad field would be wrong on 32-bit, where there is none. What is not
// automatic is the field *order*, which is why it is spelled out above: get one pair the wrong
// way round and every call still compiles and returns garbage.
type credentialW struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        windows.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

// credentialManager is the Keyring backed by the Windows Credential Manager.
//
// It holds nothing. The Cred* API has no handle to open and no session to keep: every call
// names its target and does its own RPC to the credential store. So there is nothing to close,
// and nothing that goes stale between calls.
type credentialManager struct{}

// openKeyring probes the credential store rather than reporting that Windows is Windows.
//
// Two things can be missing and neither is visible from the GOOS. The exports may not be there
// — Nano Server and other trimmed images ship an advapi32 without the Cred* family — and a
// LazyProc that cannot be found panics the first time it is called, which is not how a keyring
// should report that it is absent. And the credential store needs an interactive logon session
// behind it: a process running as a service account or over a network logon gets
// ERROR_NO_SUCH_LOGON_SESSION from every call, on a machine where the API exists and the user
// is real. Both are found here, at open time, where the caller can still fall back to the
// encrypted file.
func openKeyring(ctx context.Context) (Keyring, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, p := range []*windows.LazyProc{procCredWriteW, procCredReadW, procCredDeleteW, procCredFree} {
		if err := p.Find(); err != nil {
			return nil, keyringUnavailable("advapi32.dll does not export "+p.Name, err)
		}
	}
	// A read, not a write: the probe must not leave anything behind on a machine where
	// Boks is only being asked whether a keyring exists. Any answer at all — the entry is
	// missing, or by coincidence some entry is there — proves the store is reachable, so
	// the probe target does not need to be unique and is not treated as if it were.
	_, err := credmanRead(credmanTarget("probe: does this session have a credential store?"))
	switch {
	case err == nil, errors.Is(err, ErrNotFound):
		return credentialManager{}, nil
	case errors.Is(err, ErrNoKeyring):
		return nil, err
	default:
		return nil, keyringUnavailable("the Credential Manager could not be read", err)
	}
}

// Get returns the value stored under name.
//
// The context is checked and then not honoured further, which is the honest description: a
// Cred* call is a synchronous RPC to the local credential store with no cancellation of any
// kind, so a deadline that expires mid-call cannot be acted on until it returns. Checking it up
// front at least means a caller that has already given up does not start new work.
func (credentialManager) Get(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if err := validKeyringName(name); err != nil {
		return "", err
	}
	value, err := credmanRead(credmanTarget(name))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", fmt.Errorf("%w: %q", ErrNotFound, name)
		}
		// The name, never the value: this error is printed, and a read that fails
		// halfway is exactly the moment a blob might otherwise end up in a log.
		return "", fmt.Errorf("reading secret %q from the Credential Manager: %w", name, err)
	}
	return value, nil
}

// Set stores value under name, replacing any existing one.
//
// CredWriteW replaces by default — without CRED_PRESERVE_CREDENTIAL_BLOB it overwrites the
// whole record — so there is no delete-then-write here, and therefore no window in which the
// old value is gone and the new one is not yet there.
func (credentialManager) Set(ctx context.Context, name, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validKeyringName(name); err != nil {
		return err
	}
	blob, err := credmanEncodeUTF16LE(value)
	if err != nil {
		return fmt.Errorf("storing secret %q: %w", name, err)
	}
	if len(blob) > credMaxCredentialBlobSize {
		// The length of a secret is not the secret, and a caller given
		// ERROR_INVALID_PARAMETER instead would have no way to guess which of the
		// twelve fields Windows meant.
		return fmt.Errorf("secret %q is %d bytes once encoded as UTF-16LE, over the Credential Manager's limit of %d",
			name, len(blob), credMaxCredentialBlobSize)
	}
	target, err := windows.UTF16PtrFromString(credmanTarget(name))
	if err != nil {
		return fmt.Errorf("secret name %q: %w", name, err)
	}
	// UserName is filled in with the secret's name — never its value. It is optional for
	// a generic credential, but the Credential Manager UI shows it as the entry's account,
	// and an entry whose account column is blank is one a user cannot identify. It also
	// means no call here passes NULL for it, so nothing depends on NULL being accepted.
	user, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return fmt.Errorf("secret name %q: %w", name, err)
	}
	comment, err := windows.UTF16PtrFromString(credmanComment)
	if err != nil {
		return fmt.Errorf("secret %q: %w", name, err)
	}

	// An empty value still gets a non-NULL pointer with a zero size. Whether CredWriteW
	// accepts a NULL blob for a generic credential is not something the documentation
	// says, and an empty secret is a real case (a caller clearing a value in place), so
	// the ambiguity is avoided rather than tested in production: Windows reads
	// CredentialBlobSize bytes, which is none of them.
	empty := [1]byte{}
	blobPtr := &empty[0]
	if len(blob) > 0 {
		blobPtr = &blob[0]
	}
	cred := credentialW{
		Type:       credTypeGeneric,
		TargetName: target,
		Comment:    comment,
		// BYTES, not code units. This is the mistake this API is known for, and it
		// is silent in both directions: halve it and the value comes back truncated,
		// double it and Windows reads memory that is not the secret's.
		CredentialBlobSize: uint32(len(blob)),
		CredentialBlob:     blobPtr,
		Persist:            credPersistLocalMachine,
		UserName:           user,
	}
	// SyscallN rather than LazyProc.Call: the unsafe.Pointer conversion has to happen in
	// the argument list of a syscall.Syscall-family call for the pointer to be kept alive
	// across it, and Call's variadic ...uintptr does not carry that guarantee. Everything
	// the struct points at — the blob, the three wide strings — is reachable from cred, so
	// keeping cred alive keeps all of it alive.
	r1, _, e := syscall.SyscallN(procCredWriteW.Addr(), uintptr(unsafe.Pointer(&cred)), 0)
	if r1 == 0 {
		return fmt.Errorf("storing secret %q in the Credential Manager: %w", name, credmanError(e))
	}
	return nil
}

// Delete removes name, and treats a name that is not there as already done.
func (credentialManager) Delete(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validKeyringName(name); err != nil {
		return err
	}
	target, err := windows.UTF16PtrFromString(credmanTarget(name))
	if err != nil {
		return fmt.Errorf("secret name %q: %w", name, err)
	}
	r1, _, e := syscall.SyscallN(procCredDeleteW.Addr(), uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
	if r1 == 0 {
		err := credmanError(e)
		if errors.Is(err, ErrNotFound) {
			// Deliberately not an error: the Keyring contract asks for a delete
			// after a partial write to leave nothing behind, and a store that has
			// nothing already satisfies that.
			return nil
		}
		return fmt.Errorf("deleting secret %q from the Credential Manager: %w", name, err)
	}
	return nil
}

// credmanRead reads one target and returns its blob decoded.
//
// It is separate from Get because openKeyring's probe needs the same call without Get's
// name-shaped error wrapping, and because the CredFree it owes is easier to see when the
// function does one thing.
func credmanRead(target string) (string, error) {
	wide, err := windows.UTF16PtrFromString(target)
	if err != nil {
		return "", err
	}
	var pcred *credentialW
	r1, _, e := syscall.SyscallN(procCredReadW.Addr(),
		uintptr(unsafe.Pointer(wide)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&pcred)))
	if r1 == 0 {
		return "", credmanError(e)
	}
	// CredReadW allocates the credential and every string in it; the caller owns all of
	// it and CredFree is the only way to give it back. Deferred rather than called at the
	// end, so that the decode below can return an error without leaking.
	defer credmanFree(unsafe.Pointer(pcred))
	if pcred == nil {
		// Documented never to happen when the call succeeds. Checked anyway, because
		// the alternative to a bad assumption here is a nil dereference in the
		// process that holds the user's secrets.
		return "", errors.New("the Credential Manager reported success but returned no credential")
	}
	if pcred.CredentialBlob == nil || pcred.CredentialBlobSize == 0 {
		return "", nil
	}
	// Copied out before CredFree runs. unsafe.Slice over memory this process does not own
	// would otherwise be a slice whose backing array is freed while it is still referenced.
	blob := make([]byte, pcred.CredentialBlobSize)
	copy(blob, unsafe.Slice(pcred.CredentialBlob, pcred.CredentialBlobSize))
	return credmanDecodeUTF16LE(blob)
}

// credmanFree hands a CredReadW allocation back. The pointer is Windows' memory, not Go's, so
// there is nothing for the garbage collector to be told about it.
func credmanFree(p unsafe.Pointer) {
	_, _, _ = syscall.SyscallN(procCredFree.Addr(), uintptr(p))
}

// credmanError turns the errno a failed Cred* call left behind into the error the rest of the
// package understands.
//
// The errno is only meaningful once the BOOL return has already said the call failed:
// SyscallN reports the thread's last error unconditionally, so it is routinely non-zero and
// stale after a call that succeeded. Every caller here checks r1 first.
func credmanError(e syscall.Errno) error {
	switch e {
	case windows.ERROR_NOT_FOUND: // 1168, winerror.h
		// The caller adds the name; this one is the bare sentinel so that
		// Delete can recognise it without matching on text.
		return ErrNotFound
	case windows.ERROR_NO_SUCH_LOGON_SESSION: // 1312, winerror.h
		// Not a missing secret and not a broken machine: a process with no
		// interactive logon session — a service, a network logon — has no credential
		// set to read at all. That is precisely the case ErrNoKeyring exists for, and
		// the caller's answer is the encrypted file rather than a failure.
		return keyringUnavailable("this logon session has no credential store", e)
	case 0:
		return errors.New("the Credential Manager reported a failure without an error code")
	}
	return e
}

// Describe names this platform's store, so that a user told where a credential went can
// open the right application and look at it.
func (k credentialManager) Describe() string { return "Windows Credential Manager" }
