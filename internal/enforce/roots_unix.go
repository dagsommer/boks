//go:build !windows

package enforce

// hostRootBundles are the usual locations of a PEM root store. The list is the same one
// Go's own x509 package walks, which is why it covers the distributions it does.
var hostRootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
	"/etc/ssl/ca-bundle.pem",             // openSUSE
	"/etc/ssl/cert.pem",                  // macOS, Alpine
}

// hostRoots returns the host's public root store in PEM form.
//
// A nil result with a nil error means "this host has no root store where Boks knows to look",
// and the caller then leaves SSL_CERT_FILE, REQUESTS_CA_BUNDLE and CURL_CA_BUNDLE unset
// rather than pointing them at a Boks-only file. This function never returns an error; the
// signature carries one because the Windows implementation does, and refusing there is the
// whole point of that file.
//
// Unix does not refuse, and the asymmetry is intentional. The paths below are exactly the
// paths Go's crypto/x509 searches, so a Unix host with none of them is a host on which Go
// itself has no roots — an exotic, hand-built system whose owner has already opted out of the
// distribution's trust store and is unlikely to be surprised. On Windows the equivalent
// condition is not exotic: every Windows machine has a populated ROOT store, so producing
// nothing there means something is actually broken, and it is worth stopping for. Making Unix
// refuse too would be a defensible change, but it is a behaviour change to a working path and
// belongs in its own commit with its own reasoning.
func hostRoots() ([]byte, error) { return readFirstBundle(hostRootBundles), nil }
