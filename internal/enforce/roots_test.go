package enforce

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests drive the trust-bundle assembly with synthetic DER, which is the only way it
// can be tested at all: the Windows certificate store it exists to serve is on a machine
// nobody working on this project has. The split is deliberate — systemStoreDER is the thin
// part that talks to CryptoAPI and cannot be exercised here, and rootsPEM holds every
// decision about what belongs in a guest's trust bundle and is exercised in full.

// syntheticRootDER mints a self-signed CA certificate and returns its DER. It stands in for
// something the host's root store would have handed over.
func syntheticRootDER(t *testing.T, commonName string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generating a key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("creating a certificate: %v", err)
	}
	return der
}

// decodeAll returns every DER body in a PEM bundle, and fails if the buffer holds anything
// that is not a CERTIFICATE block.
func decodeAll(t *testing.T, bundle []byte) [][]byte {
	t.Helper()
	var out [][]byte
	rest := bundle
	for len(bytes.TrimSpace(rest)) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			t.Fatalf("the bundle holds %d bytes that are not a PEM block: %q", len(rest), rest)
		}
		if block.Type != "CERTIFICATE" {
			t.Fatalf("the bundle holds a %q block; a trust bundle may only hold certificates", block.Type)
		}
		if len(block.Headers) != 0 {
			t.Errorf("the block for a certificate carries PEM headers %v", block.Headers)
		}
		out = append(out, block.Bytes)
	}
	return out
}

func TestRootsPEMEncodesEveryCertificateItIsGiven(t *testing.T) {
	ders := [][]byte{
		syntheticRootDER(t, "one"),
		syntheticRootDER(t, "two"),
		syntheticRootDER(t, "three"),
	}
	bundle := rootsPEM(ders, nil)
	if bundle.Encoded != 3 {
		t.Fatalf("encoded %d certificates, want 3 (%+v)", bundle.Encoded, bundle)
	}

	got := decodeAll(t, bundle.PEM)
	if len(got) != 3 {
		t.Fatalf("the bundle decodes to %d certificates, want 3", len(got))
	}
	// Byte-for-byte: a trust anchor that came back re-encoded would be a different
	// certificate, and a bundle is only as good as the exact bytes in it.
	for _, want := range ders {
		found := false
		for _, have := range got {
			if bytes.Equal(want, have) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("a certificate handed to rootsPEM is not in the bundle, or came back altered")
		}
	}
}

// A certificate the host has explicitly stopped trusting must not come back as a trust anchor
// inside the guest. PEM cannot express distrust, so leaving it out is the only remedy there
// is, and this is the test that says it happens.
func TestRootsPEMDropsDistrustedCertificates(t *testing.T) {
	keep := syntheticRootDER(t, "still trusted")
	drop := syntheticRootDER(t, "distrusted by the host")

	bundle := rootsPEM([][]byte{keep, drop}, [][]byte{drop})
	if bundle.Encoded != 1 || bundle.Distrusted != 1 {
		t.Fatalf("bundle = %+v, want one encoded and one distrusted", bundle)
	}
	got := decodeAll(t, bundle.PEM)
	if len(got) != 1 || !bytes.Equal(got[0], keep) {
		t.Fatalf("the bundle does not hold exactly the trusted certificate")
	}
	if bytes.Contains(bundle.PEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: drop})) {
		t.Error("the distrusted certificate is in the bundle")
	}
}

// A Windows system store is a collection over several physical stores, so one root can be
// enumerated more than once. Duplicated anchors are harmless to a verifier but they bloat a
// file that is written on every sandbox start, and the count is what tells the caller the
// store was read correctly rather than twice.
func TestRootsPEMDropsDuplicates(t *testing.T) {
	der := syntheticRootDER(t, "in two physical stores")
	bundle := rootsPEM([][]byte{der, der, der}, nil)
	if bundle.Encoded != 1 || bundle.Duplicate != 2 {
		t.Fatalf("bundle = %+v, want one encoded and two duplicates", bundle)
	}
	if n := len(decodeAll(t, bundle.PEM)); n != 1 {
		t.Fatalf("the bundle holds %d copies of one certificate", n)
	}
}

// The property this protects is the one that decided the design: OpenSSL's file loader aborts
// on a malformed entry rather than skipping it, so a single bad block can cost the guest the
// whole trust store — including the Boks CA appended to the end of it. One unusable
// certificate must therefore cost exactly one certificate.
func TestRootsPEMDropsUnparseableEntriesAndKeepsTheRest(t *testing.T) {
	good := syntheticRootDER(t, "good")
	other := syntheticRootDER(t, "other")
	junk := []byte{0x30, 0x82, 0xff, 0xff, 'n', 'o', 't', ' ', 'a', ' ', 'c', 'e', 'r', 't'}

	bundle := rootsPEM([][]byte{good, junk, other, nil, {}}, nil)
	if bundle.Encoded != 2 {
		t.Fatalf("bundle = %+v, want the two real certificates encoded", bundle)
	}
	if bundle.Unparseable != 3 {
		t.Errorf("bundle.Unparseable = %d, want 3 (the junk and the two empty entries)", bundle.Unparseable)
	}
	got := decodeAll(t, bundle.PEM)
	if len(got) != 2 {
		t.Fatalf("the bundle decodes to %d certificates, want 2", len(got))
	}
	for _, der := range got {
		if _, err := x509.ParseCertificate(der); err != nil {
			t.Errorf("the bundle holds something that is not a certificate: %v", err)
		}
	}
}

// The bundle is rewritten every time a sandbox starts. If its byte order depended on the
// order a store happened to enumerate in, the file would churn for no reason and "have the
// host's roots changed?" would stop being answerable by comparing two bundles.
func TestRootsPEMIsDeterministicWhateverTheStoreOrder(t *testing.T) {
	a := syntheticRootDER(t, "a")
	b := syntheticRootDER(t, "b")
	c := syntheticRootDER(t, "c")

	first := rootsPEM([][]byte{a, b, c}, nil)
	for _, order := range [][][]byte{
		{c, b, a},
		{b, a, c},
		{c, a, b},
	} {
		got := rootsPEM(order, nil)
		if !bytes.Equal(got.PEM, first.PEM) {
			t.Fatalf("a different enumeration order produced a different bundle")
		}
	}
}

// An empty result must be empty rather than a zero-length file: the caller distinguishes the
// two, and on Windows the difference is between starting the sandbox and refusing to.
func TestRootsPEMWithNothingUsableProducesNoBundle(t *testing.T) {
	for name, in := range map[string][][]byte{
		"no certificates at all": nil,
		"only junk":              {{1, 2, 3}},
		"only empty entries":     {nil, {}},
	} {
		bundle := rootsPEM(in, nil)
		if bundle.Encoded != 0 || bundle.PEM != nil {
			t.Errorf("%s: bundle = %+v, want nothing encoded and a nil PEM", name, bundle)
		}
	}
}

// Everything a root store hands over may be distrusted, and the result must be an empty
// bundle rather than a bundle of the distrusted certificates.
func TestRootsPEMWithEverythingDistrustedProducesNoBundle(t *testing.T) {
	ders := [][]byte{syntheticRootDER(t, "one"), syntheticRootDER(t, "two")}
	bundle := rootsPEM(ders, ders)
	if bundle.Encoded != 0 || bundle.PEM != nil || bundle.Distrusted != 2 {
		t.Fatalf("bundle = %+v, want nothing encoded and two distrusted", bundle)
	}
}

// The bundle is parsed inside the guest by OpenSSL, by Python and by whatever TLS backend
// curl was built against that week. Only PEM blocks are universally accepted, so the helpful
// subject headers that would make the file readable are deliberately absent — and this is the
// assertion that keeps them absent.
func TestRootsPEMOutputHoldsNothingButCertificateBlocks(t *testing.T) {
	bundle := rootsPEM([][]byte{syntheticRootDER(t, "one"), syntheticRootDER(t, "two")}, nil)
	decodeAll(t, bundle.PEM) // fails on any byte outside a CERTIFICATE block

	// Also usable as a trust store by Go's own verifier, which is the closest thing to an
	// independent parser available here.
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(bundle.PEM) {
		t.Fatal("x509 could not load the bundle as a certificate pool")
	}
}

// The Windows fail-closed decision, driven on Linux.
//
// This is the half of the Windows path that matters and the half that is testable: given what
// a certificate store handed over, does Boks produce a bundle or refuse? The syscalls that
// produce the input are in roots_windows.go and cannot be run here; the judgement is here.

func TestWindowsRootBundleProducesAPEMBundleFromAStore(t *testing.T) {
	roots := [][]byte{syntheticRootDER(t, "one"), syntheticRootDER(t, "two")}
	got, err := windowsRootBundle(roots, nil)
	if err != nil {
		t.Fatalf("windowsRootBundle: %v", err)
	}
	if n := len(decodeAll(t, got)); n != 2 {
		t.Fatalf("the bundle holds %d certificates, want 2", n)
	}
}

// The decision this repository had to take: a Windows host whose certificate store yields
// nothing usable does not get a sandbox. It must not get one with no bundle either — that is
// the partial-trust state (Node trusts the Boks CA, OpenSSL does not) which the whole change
// exists to remove, and whose usual remedy is to turn TLS verification off.
func TestWindowsRootBundleRefusesRatherThanShippingNoBundle(t *testing.T) {
	allDistrusted := [][]byte{syntheticRootDER(t, "distrusted")}
	type store struct{ roots, distrusted [][]byte }
	for name, in := range map[string]store{
		"an empty store":            {nil, nil},
		"a store of junk":           {[][]byte{{1, 2, 3}}, nil},
		"a wholly distrusted store": {allDistrusted, allDistrusted},
	} {
		got, err := windowsRootBundle(in.roots, in.distrusted)
		if err == nil {
			t.Errorf("%s: windowsRootBundle returned %d bytes and no error; it must refuse",
				name, len(got))
			continue
		}
		if got != nil {
			t.Errorf("%s: windowsRootBundle returned both a bundle and an error", name)
		}
		// The refusal has to be actionable. A caller who sees only "no trust anchors"
		// cannot tell whether it matters, and the operator's first instinct — retry
		// without interception — is the one thing that is guaranteed to work.
		for _, want := range []string{
			rootStoreName,
			"SSL_CERT_FILE",
			"interception",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not mention %q:\n%v", name, want, err)
			}
		}
	}
}

// A refusal that could not say what it saw would leave "the store was unreadable" and "every
// certificate in it was distrusted" looking identical. Prepare has no logger to write those
// numbers to, so the error text is the only place they can appear.
func TestWindowsRootBundleRefusalAccountsForWhatItDropped(t *testing.T) {
	distrusted := [][]byte{syntheticRootDER(t, "distrusted")}
	roots := append([][]byte{{9, 9, 9}}, distrusted...)
	roots = append(roots, distrusted...)

	_, err := windowsRootBundle(roots, distrusted)
	if err == nil {
		t.Fatal("windowsRootBundle accepted a store with nothing usable in it")
	}
	for _, want := range []string{
		"3 certificates enumerated",
		"1 unparseable",
		"2 distrusted",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not report %q:\n%v", want, err)
		}
	}
}

func TestReadFirstBundleTakesTheFirstNonEmptyFile(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "missing")
	empty := filepath.Join(dir, "empty")
	first := filepath.Join(dir, "first")
	second := filepath.Join(dir, "second")

	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(first, []byte("first contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second contents\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := readFirstBundle([]string{missing, empty, first, second})
	if string(got) != "first contents\n" {
		t.Errorf("readFirstBundle = %q, want the first non-empty file verbatim", got)
	}
	if readFirstBundle([]string{missing, empty}) != nil {
		t.Error("readFirstBundle found something in a list with nothing readable in it")
	}
	if readFirstBundle(nil) != nil {
		t.Error("readFirstBundle found something in an empty list")
	}
}
