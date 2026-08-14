//go:build windows

package enforce

import (
	"bytes"
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// hostRoots exports the host's public root store as PEM.
//
// Windows has no PEM root bundle on disk and Go cannot produce one either: x509.SystemCertPool
// on Windows returns an opaque pool that defers to CertGetCertificateChain at verification
// time and **cannot be enumerated**, by design. The store therefore has to be read through
// CryptoAPI directly, which is what this does. Which stores are read, and which deliberately
// is not, is documented on the store-name constants in roots.go.
//
// # It refuses rather than returning nothing
//
// Unlike the Unix implementation, this returns an error when it cannot produce a bundle, and
// that error aborts Prepare, so no sandbox is created. The alternative — and the behaviour
// this replaces, where Windows simply had no root store to find — is to return nothing and
// let the guest start anyway. That is worse, for a reason worth spelling out, because "no
// bundle" sounds like the conservative option:
//
//   - A guest with no bundle does not fall back to a safe state. It falls back to whatever
//     its image ships, which may or may not carry the Boks CA. Boks does not control the
//     image — the user names it — so this is not a state Boks can reason about at all.
//   - The likely outcome is *partial* trust, which is the worst of the three. Node is
//     configured additively through NODE_EXTRA_CA_CERTS and trusts the Boks CA regardless;
//     curl, python and git go through OpenSSL and do not. Half the tools in the sandbox work,
//     half fail with certificate errors, and nothing in the output explains why.
//   - The pressure that failure creates points the wrong way. The occupant of the sandbox is
//     an untrusted coding agent, and every guide on the internet answers "certificate verify
//     failed" with an instruction to stop verifying. `curl -k`,
//     `NODE_TLS_REJECT_UNAUTHORIZED=0`, `verify=False` and `http.sslVerify false` are one turn
//     away, and none of them disable verification only for the intercepted hosts — they
//     disable it for the real origins too, where the end-to-end guarantee was genuine and
//     Boks was not in the path at all.
//
// So the choice is between a sandbox that does not start and says exactly why, and a sandbox
// that starts in a state which quietly rewards turning TLS verification off. Refusing is
// louder, is discovered immediately rather than twenty minutes into a run, and costs a retry.
// Note also how narrow the refusal is: writeGuestCA is only reached when the sandbox actually
// intercepts something, so a Windows host running an ordinary sandbox with no credential
// injection never enters this path.
//
// **Nothing in this file has ever been executed.** No machine on this project runs Windows.
// It compiles for windows/amd64 and windows/arm64, and it follows the enumeration contract as
// Microsoft documents it and as Go's own crypto/x509 implemented it before that package moved
// to chain-based verification — but that is the whole of the claim. The decision it feeds,
// windowsRootBundle, is in roots.go precisely so that it can be tested somewhere.
func hostRoots() ([]byte, error) {
	roots, err := systemStoreDER(rootStoreName)
	if err != nil {
		return nil, fmt.Errorf("%w\n\n%s", err, whyTheBundleMatters)
	}
	// A failure to read the untrusted store is fatal too, and not out of tidiness: it is
	// the only mechanism by which a root the host has stopped trusting is kept out of the
	// guest's anchors. Continuing without it would produce a bundle quietly *wider* than
	// the host's own trust, which is the sort of difference nobody ever discovers.
	distrusted, err := systemStoreDER(untrustedStoreName)
	if err != nil {
		return nil, fmt.Errorf("%w\n\n%s", err, whyTheBundleMatters)
	}
	return windowsRootBundle(roots, distrusted)
}

// systemStoreDER enumerates one Windows system certificate store and returns each
// certificate's DER.
//
// Nothing is interpreted here on purpose. Every judgement about what belongs in a trust
// bundle is in rootsPEM and windowsRootBundle, which are ordinary Go over byte slices and are
// therefore testable on the Linux and macOS machines this was written on. This function is
// the part that cannot be tested anywhere, and it is kept as small as the API allows.
func systemStoreDER(name string) ([][]byte, error) {
	name16, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("enforce: %q is not a usable certificate store name: %w", name, err)
	}
	// A zero provider handle asks for the default provider; the name is what selects the
	// store, and CertOpenSystemStore resolves it to the current user's collection view.
	store, err := windows.CertOpenSystemStore(0, name16)
	if err != nil {
		return nil, fmt.Errorf("enforce: opening the Windows %q certificate store: %w", name, err)
	}
	defer windows.CertCloseStore(store, 0)

	var (
		ders [][]byte
		ctx  *windows.CertContext
	)
	for {
		// CertEnumCertificatesInStore frees the context it is handed, whatever it goes
		// on to return, so no exit from this loop leaves a context to release. It
		// signals the end of the enumeration by returning nil with the thread's last
		// error set to CRYPT_E_NOT_FOUND — an ordinary end-of-list rather than a
		// failure. Every other error is real, and is reported rather than treated as an
		// early end, because a truncated enumeration would silently narrow the guest's
		// trust to whatever was read before the error.
		next, err := windows.CertEnumCertificatesInStore(store, ctx)
		if err != nil {
			if errors.Is(err, syscall.Errno(windows.CRYPT_E_NOT_FOUND)) {
				break
			}
			return nil, fmt.Errorf("enforce: enumerating the Windows %q certificate store: %w", name, err)
		}
		if next == nil {
			break
		}
		ctx = next

		// A store may hold PKCS#7-encoded entries beside plain X.509 ones. Only the
		// latter is a single DER certificate, which is what a PEM CERTIFICATE block has
		// to contain.
		if ctx.EncodingType&windows.X509_ASN_ENCODING == 0 {
			continue
		}
		if ctx.EncodedCert == nil || ctx.Length == 0 {
			continue
		}
		// The bytes belong to the context and are freed with it on the next iteration,
		// so they are copied out rather than aliased. Nothing retains a pointer into
		// CryptoAPI's memory past this line.
		ders = append(ders, bytes.Clone(unsafe.Slice(ctx.EncodedCert, ctx.Length)))
	}
	return ders, nil
}
