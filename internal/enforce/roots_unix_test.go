//go:build !windows

package enforce

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"testing"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/network"
)

// This file exists to hold the Unix trust-bundle behaviour still while the Windows one was
// added beside it. Adding a platform to a security-relevant path is exactly the change that
// tends to "tidy" the working platform on the way past — routing Linux through the new DER
// re-encoder for symmetry, say, which would quietly reorder the bundle, drop any root Go's
// parser dislikes, and replace the distribution's curated answer with Boks' own. Each test
// below fails if that happens.

// hostRootStoreOnThisMachine finds the root store the way hostRoots is supposed to, without
// calling it. Re-walking the list rather than reusing the helper is the point: it is what
// makes the comparisons below a cross-check instead of a tautology.
func hostRootStoreOnThisMachine() (path string, contents []byte) {
	for _, p := range hostRootBundles {
		data, err := os.ReadFile(p)
		if err == nil && len(data) > 0 {
			return p, data
		}
	}
	return "", nil
}

// TestHostRootBundlesAreTheDocumentedPaths pins the search list and its order.
//
// The order is not cosmetic: Alpine has both /etc/ssl/certs/ca-certificates.crt and
// /etc/ssl/cert.pem, and the list is the same one Go's crypto/x509 walks, which is the reason
// it covers the distributions it does. A path added, removed or reordered here changes which
// roots a guest gets on some hosts and no other test would notice.
func TestHostRootBundlesAreTheDocumentedPaths(t *testing.T) {
	want := []string{
		"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
		"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
		"/etc/ssl/ca-bundle.pem",             // openSUSE
		"/etc/ssl/cert.pem",                  // macOS, Alpine
	}
	if len(hostRootBundles) != len(want) {
		t.Fatalf("hostRootBundles = %v, want %v", hostRootBundles, want)
	}
	for i := range want {
		if hostRootBundles[i] != want[i] {
			t.Errorf("hostRootBundles[%d] = %q, want %q", i, hostRootBundles[i], want[i])
		}
	}
}

// TestHostRootsIsAVerbatimPassthrough is the assertion that Unix hosts still hand the guest
// the distribution's own bundle rather than one Boks reconstructed.
//
// The comparison is byte-for-byte against the file this test finds for itself, by walking the
// list the same way rather than by calling the helper under test. A Unix path that started
// parsing, deduplicating, sorting or re-encoding — all of which the Windows path does, and
// must — would fail here, and so would one that started appending or filtering.
func TestHostRootsIsAVerbatimPassthrough(t *testing.T) {
	wantPath, want := hostRootStoreOnThisMachine()

	got, err := hostRoots()
	if err != nil {
		t.Fatalf("hostRoots returned an error on a Unix host: %v", err)
	}
	if wantPath == "" {
		// A host with none of the four paths. hostRoots must report nothing rather than
		// improvise, because the caller reads "nothing" as "leave the three replacing
		// variables unset" — the behaviour that keeps SSL_CERT_FILE from naming a
		// Boks-only file.
		if got != nil {
			t.Fatalf("no root bundle exists on this host, but hostRoots returned %d bytes", len(got))
		}
		t.Skip("this host has no PEM root store, so there is no passthrough to compare")
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("hostRoots returned %d bytes; %s holds %d. The Unix path must hand over "+
			"the distribution's bundle unaltered", len(got), wantPath, len(want))
	}
}

// hostRoots on Unix has no failure to report, and the caller depends on that: an error there
// aborts Prepare, so a Unix host that started returning one would turn a working sandbox into
// a refusal.
func TestHostRootsNeverFailsOnUnix(t *testing.T) {
	if _, err := hostRoots(); err != nil {
		t.Fatalf("hostRoots = %v; the Unix implementation must not fail", err)
	}
}

// TestTheGuestBundleIsTheHostRootsFollowedByTheBoksCA covers the shape of the file that ends
// up in the sandbox, which is the thing the Windows implementation had to reproduce.
//
// Both halves matter and for different reasons. The host's roots have to be there or
// SSL_CERT_FILE would point at a file that breaks every destination Boks does not intercept;
// the Boks CA has to be there or the destinations Boks *does* intercept fail instead. The
// assertion is on the exact bytes, in that order, with nothing else in the file.
func TestTheGuestBundleIsTheHostRootsFollowedByTheBoksCA(t *testing.T) {
	// The distribution's file, read here rather than through hostRoots, so that this test
	// keeps its meaning even if the Unix lookup is one day rewritten.
	storePath, roots := hostRootStoreOnThisMachine()
	if storePath == "" {
		t.Skip("this host has no PEM root store, so no bundle is written")
	}

	spec := testSpec(t, network.ModeNAT)
	spec.Inject = []string{"anthropic@api.anthropic.com=x-api-key"}
	spec.Intercept = true

	guest, err := spec.Prepare()
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	authority, err := ca.Open(spec.CADir)
	if err != nil {
		t.Fatalf("opening the authority Prepare created: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(spec.certDir(), bundleFile))
	if err != nil {
		t.Fatalf("reading the bundle shared into the guest: %v", err)
	}
	want := append(append([]byte{}, roots...), authority.CertPEM()...)
	if !bytes.Equal(got, want) {
		t.Errorf("the guest bundle is %d bytes; %s followed by the Boks CA is %d",
			len(got), storePath, len(want))
	}
	if !bytes.HasPrefix(got, roots) {
		t.Errorf("the guest bundle does not start with %s verbatim", storePath)
	}
	if !bytes.HasSuffix(got, authority.CertPEM()) {
		t.Error("the guest bundle does not end with the Boks CA")
	}

	// All three replacing variables, or a guest trusts the bundle in one runtime and not
	// in the next — the partial-trust state this whole path exists to avoid.
	env := envMap(guest.Env)
	guestBundle := path.Join(GuestCADir, bundleFile)
	for _, key := range []string{"SSL_CERT_FILE", "REQUESTS_CA_BUNDLE", "CURL_CA_BUNDLE"} {
		if env[key] != guestBundle {
			t.Errorf("%s = %q, want %q", key, env[key], guestBundle)
		}
	}
	// And the additive one still names the Boks-only file: pointing Node at the bundle
	// would work, but pointing it at the CA is what keeps the two files' purposes distinct.
	if env["NODE_EXTRA_CA_CERTS"] != path.Join(GuestCADir, certFile) {
		t.Errorf("NODE_EXTRA_CA_CERTS = %q", env["NODE_EXTRA_CA_CERTS"])
	}
}
