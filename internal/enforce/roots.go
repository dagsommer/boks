package enforce

// The guest's trust bundle: what it is, and why getting it wrong is worse than it looks.
//
// When a sandbox intercepts anything, Boks terminates TLS on the host for the hosts you
// named, so the guest is shown a certificate minted by the Boks CA rather than the origin's.
// For that to work the guest has to trust the Boks CA. But the guest also still talks to
// everything Boks does *not* intercept — github.com, a package registry, an npm mirror — and
// those are still end-to-end TLS to the real origin, so the guest must **also** keep trusting
// the ordinary public roots.
//
// Node is handled separately and additively: NODE_EXTRA_CA_CERTS *adds* to Node's built-in
// root list, so it can safely name a file holding only the Boks CA. The other three variables
// are not additive. SSL_CERT_FILE, REQUESTS_CA_BUNDLE and CURL_CA_BUNDLE *replace* the trust
// store of OpenSSL, requests and curl respectively, so pointing them at a Boks-only file
// would leave the guest trusting exactly one CA and failing every other TLS connection it
// makes. They may only ever name a file that carries the public roots as well.
//
// That is why this file exists: to produce the public half of that bundle from whatever the
// host has, on every platform Boks builds for. The Boks CA is appended to it by writeGuestCA.
//
// # Why the platform split
//
// On Unix the answer is a file. Every distribution ships its curated root store as a PEM
// bundle at a well-known path, and Boks reads the first one it finds — the same list, in the
// same order, that Go's own crypto/x509 walks. Nothing is re-encoded: the bytes the host has
// are the bytes the guest gets.
//
// On Windows there is no such file. The roots live in the certificate store, a structured
// database reached through CryptoAPI, and there is no PEM rendering of it anywhere on disk.
// Worse, Go cannot be asked for one either: x509.SystemCertPool on Windows returns an opaque
// pool that defers to CertGetCertificateChain at verification time and **cannot enumerate its
// contents**, by design. So the store has to be read directly and encoded here. See
// roots_windows.go.
//
// Before this split, hostRoots() simply returned false on Windows and the three replacing
// variables were left unset. That is a quiet, nasty failure mode and it is the thing this
// code exists to remove: with interception on, Node would trust the Boks CA (it is configured
// separately) while curl, python/requests and anything else on OpenSSL would not, unless the
// image happened to install the CA itself. Half the tools in the sandbox work and half fail
// with certificate errors, for a reason nothing in the output names.
//
// # The failure mode that makes this security-relevant rather than cosmetic
//
// A guest that cannot verify an intercepted certificate is not *insecure* in the direct
// sense: the connection fails, which is fail-closed. The danger is second-order and entirely
// predictable. The thing inside the sandbox is an untrusted coding agent, and the universally
// documented cure for "certificate verify failed" is to stop verifying — `curl -k`,
// `NODE_TLS_REJECT_UNAUTHORIZED=0`, `verify=False`, `git config http.sslVerify false`,
// `PIP_TRUSTED_HOST`. An agent that hits an unexplained certificate error will find one of
// those within a few turns, and the result is a sandbox that has genuinely stopped
// authenticating its peers — including the ones Boks is not intercepting, where the origin is
// real and the guarantee was real. Shipping a broken trust store therefore does not merely
// break tools; it applies steady pressure toward disabling TLS verification wholesale.
//
// Which is why the Windows path refuses rather than shrugging. See hostRoots there.

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"sort"
)

// rootBundle is the result of turning a set of DER certificates into a PEM trust bundle,
// along with an account of everything that was left out.
//
// The counts are not decoration. The Windows caller has no logger — Prepare is a pure
// computation that returns a Guest — so when the bundle comes out empty these numbers are the
// only way the resulting error can say *why* the host's root store produced nothing, which is
// the difference between "the store was unreadable" and "every certificate in it was
// distrusted".
type rootBundle struct {
	// PEM is the encoded bundle, ready to have the Boks CA appended to it.
	PEM []byte
	// Encoded is how many certificates PEM holds.
	Encoded int
	// Duplicate, Distrusted and Unparseable count what was dropped, and for which reason.
	Duplicate   int
	Distrusted  int
	Unparseable int
}

// rootsPEM encodes DER certificates as a PEM trust bundle, dropping duplicates, anything
// listed in distrusted, and anything that does not parse as a certificate.
//
// It is deliberately separate from the code that reads a certificate store, so that the part
// with all the judgement in it can be tested on a machine that has no such store — which is
// every machine this was written on. roots_test.go drives it with synthetic DER.
//
// Four decisions are worth stating rather than leaving to be inferred:
//
//   - **Unparseable entries are dropped, not passed through.** The tempting alternative is to
//     emit whatever bytes the store handed over and let the guest's parser decide, on the
//     grounds that OpenSSL accepts some encodings Go's x509 rejects and dropping such a
//     certificate silently narrows what the guest trusts. It is the wrong trade, because the
//     failure is not symmetric: OpenSSL's X509_STORE_load_file aborts on a malformed entry
//     rather than skipping it, so one bad block can cost the guest the *entire* file and
//     leave it trusting nothing at all. Losing one root breaks the sites under it; losing the
//     file breaks everything, including the Boks CA appended at the end. Drop the one.
//
//   - **Distrust is matched on exact DER equality.** Windows keeps explicitly untrusted
//     certificates in a separate store, and a root that Microsoft or an administrator has
//     distrusted generally stays in ROOT while being *added* to Disallowed — so a bundle
//     built from ROOT alone would re-trust, inside the guest, a CA the host has stopped
//     trusting. PEM has no way to express distrust, so the only available remedy is to leave
//     the certificate out. Exact byte equality is used because it cannot produce a false
//     positive: a certificate is dropped only when the very same certificate is present in
//     the untrusted store. The known limitation is the converse — Microsoft also distributes
//     distrust as a certificate trust list keyed by hash, with no certificate body to match
//     against, and those entries are invisible here. This is strictly better than not
//     filtering and strictly worse than CryptoAPI's own answer, which no PEM file can reach.
//
//   - **No filtering on IsCA, key usage or expiry.** A curated Unix bundle does not filter on
//     any of them either, and the guest's own verifier applies all three at verification
//     time, where the certificate's role in an actual chain is known. Filtering here would
//     mean a root that a legitimate but ancient CA relies on — pre-BasicConstraints, say —
//     silently disappearing from a guest and not from the host.
//
//   - **The output is sorted and holds nothing but PEM blocks.** Sorted, because a store's
//     enumeration order is not specified and the bundle is written to disk on every sandbox
//     start: an unstable byte order would churn the file for no reason and make "did the
//     host's roots change?" impossible to answer by comparison. Nothing but PEM blocks,
//     because at least three independent parsers read this file inside the guest (OpenSSL,
//     Python, curl's TLS backend of the day) and the human-readable subject headers that
//     would help debugging are only *usually* tolerated. Debuggability is not worth a file
//     one of them might reject.
func rootsPEM(ders, distrusted [][]byte) rootBundle {
	untrusted := make(map[string]bool, len(distrusted))
	for _, der := range distrusted {
		untrusted[string(der)] = true
	}

	var out rootBundle
	seen := make(map[string]bool, len(ders))
	keep := make([][]byte, 0, len(ders))
	for _, der := range ders {
		switch {
		case len(der) == 0:
			out.Unparseable++
		case untrusted[string(der)]:
			out.Distrusted++
		case seen[string(der)]:
			// A Windows system store is a collection over several physical stores
			// (per-user, machine-wide, group policy, enterprise), and a root present
			// in more than one is returned more than once.
			out.Duplicate++
		default:
			if _, err := x509.ParseCertificate(der); err != nil {
				out.Unparseable++
				continue
			}
			seen[string(der)] = true
			keep = append(keep, der)
		}
	}

	sort.Slice(keep, func(i, j int) bool { return bytes.Compare(keep[i], keep[j]) < 0 })

	var buf bytes.Buffer
	for _, der := range keep {
		// pem.Encode only fails if the writer does, and a bytes.Buffer does not.
		_ = pem.Encode(&buf, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	out.Encoded = len(keep)
	if out.Encoded > 0 {
		out.PEM = buf.Bytes()
	}
	return out
}

// windowsRootBundle turns the certificates enumerated from the Windows stores into the PEM
// the guest gets, or into a refusal.
//
// It lives in this untagged file rather than in roots_windows.go for one reason: it is where
// the fail-closed decision is taken, and a decision that can only be executed on a machine
// nobody here has is a decision nothing can test. internal/workspace does the same thing with
// its Windows path mapping — the platform-specific *rules* are ordinary Go that runs
// anywhere, and only the syscall that feeds them is behind a build tag. roots_test.go drives
// this with synthetic DER, including the empty case that produces the refusal.
//
// The reasoning behind refusing, rather than returning nothing and letting the sandbox start
// with whatever trust its image happens to carry, is on hostRoots in roots_windows.go. The
// short version: "no bundle" is not a safe fallback, it is a *partial-trust* fallback — Node
// trusts the Boks CA because it is configured additively and everything on OpenSSL does not —
// and the standard cure for the certificate errors that follow is to disable TLS verification,
// which an untrusted coding agent will reach for within a few turns.
func windowsRootBundle(roots, distrusted [][]byte) ([]byte, error) {
	bundle := rootsPEM(roots, distrusted)
	if bundle.Encoded == 0 {
		return nil, fmt.Errorf("enforce: the Windows %q certificate store yielded no usable "+
			"trust anchors (%d certificates enumerated: %d unparseable, %d distrusted, "+
			"%d duplicates)\n\n%s",
			rootStoreName, len(roots), bundle.Unparseable, bundle.Distrusted,
			bundle.Duplicate, whyTheBundleMatters)
	}
	return bundle.PEM, nil
}

// The two Windows system stores Boks reads, by their CryptoAPI names. They are named here
// rather than in roots_windows.go so that the error text above, and the tests that assert on
// it, do not depend on a build tag.
//
// ROOT holds the trust anchors. Opened through CertOpenSystemStore it is not a single
// physical store but a *collection*: the current user's roots, the machine-wide roots, the
// roots pushed by group policy and the enterprise store are all enumerated through it. That
// is the set Boks wants, and specifically it is the set that includes a corporate root pushed
// by an organisation that inspects TLS — a machine on such a network would otherwise hand its
// guests a bundle that works nowhere.
//
// Disallowed is the untrusted store: certificates Microsoft or an administrator has
// explicitly stopped trusting. It is read so that its contents can be *subtracted* from the
// bundle; see rootsPEM for why that is necessary and what it cannot catch.
const (
	rootStoreName      = "ROOT"
	untrustedStoreName = "Disallowed"
)

// The "CA" store is deliberately absent from that list, and its absence is a decision rather
// than an oversight.
//
// Windows keeps intermediate certificates in a store named "CA", and Boks does not read it.
// The reason is that the two systems disagree about what a certificate in a file means.
// CryptoAPI treats the CA store as a pool of *intermediates* — material to help build a chain
// that must still terminate at a ROOT anchor. A PEM file named by SSL_CERT_FILE has no such
// notion: OpenSSL, Python and curl load every certificate in it as a **trust anchor**. So
// copying the CA store into the bundle would silently promote every cached intermediate on
// the host to a root inside the guest, and the guest would then trust far more than the host
// does: an intermediate whose issuer had been distrusted would still verify, because path
// building would stop at the intermediate and never reach the revoked root above it.
//
// It is also unnecessary. TLS servers are required to send their intermediate chain, so a
// correctly configured origin needs nothing from this store; and the Windows CA store is a
// *cache*, populated by whatever chains this particular machine happened to build, so
// including it would make a guest's trust depend on which sites the host has visited. "Works
// on my machine" is a bad property for a trust store.

// whyTheBundleMatters is appended to every refusal from the Windows path. An error that only
// says what failed leaves the reader to guess whether it matters; this says what the bundle is
// for, what starting anyway would have cost, and what to do instead.
const whyTheBundleMatters = `This sandbox intercepts TLS for at least one host, so the guest is shown certificates
signed by the Boks CA and has to trust it — while still trusting the ordinary public roots
for every destination Boks does not intercept. Boks builds one bundle carrying both, from
this host's certificate store, and points SSL_CERT_FILE, REQUESTS_CA_BUNDLE and
CURL_CA_BUNDLE at it.

Boks refuses to start the sandbox rather than start it half-trusting: Node would trust the
Boks CA and curl, python and git would not, which surfaces as unexplained certificate
errors and pushes whatever is running in the sandbox toward disabling TLS verification
altogether.

Start the sandbox without interception to run with no TLS termination at all — credential
injection over HTTPS will not fire — or repair the certificate store on this host.`

// readFirstBundle returns the contents of the first readable, non-empty file in paths.
//
// This is the whole of the Unix root-store lookup, kept here rather than in roots_unix.go so
// that its behaviour is exercised by tests on every platform — including the ones where a
// change to it would otherwise only be noticed by a Windows or macOS user.
//
// It returns the file's bytes **verbatim**. Nothing is parsed, filtered, re-encoded or
// reordered, and that is a deliberate guarantee rather than an implementation detail: the
// distribution's curated store is the host's own answer to which roots to trust, including
// whatever it has already removed, and re-deriving it here could only ever produce a guest
// that trusts a different set from its host. roots_unix_test.go asserts the passthrough.
func readFirstBundle(paths []string) []byte {
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err == nil && len(data) > 0 {
			return data
		}
	}
	return nil
}
