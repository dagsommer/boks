package ca

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCreateWritesOwnerOnlyFiles(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	a, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("permissions are not expressed this way on Windows")
	}
	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if got := di.Mode().Perm(); got != 0o700 {
		t.Errorf("directory mode = %04o, want 0700", got)
	}
	for _, p := range []string{a.CertPath(), a.KeyPath()} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("stat %s: %v", p, err)
		}
		if got := fi.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode = %04o, want 0600", p, got)
		}
	}
}

func TestOpenRoundTripsAndKeepsTheFingerprint(t *testing.T) {
	dir := t.TempDir()
	created, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if created.Fingerprint() != reopened.Fingerprint() {
		t.Errorf("fingerprint changed across reload: %s vs %s", created.Fingerprint(), reopened.Fingerprint())
	}
	if !strings.Contains(reopened.Certificate().Subject.CommonName, "Boks local CA") {
		t.Errorf("subject = %q, want it to name Boks", reopened.Certificate().Subject.CommonName)
	}
	if !reopened.Certificate().IsCA || reopened.Certificate().MaxPathLen != 0 {
		t.Error("the authority should be a CA that cannot sign another CA")
	}
	if reopened.Certificate().NotAfter.Before(time.Now().Add(365 * 24 * time.Hour)) {
		t.Error("the authority expires within a year, which will surprise someone")
	}
}

func TestOpenReportsAMissingAuthority(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "absent"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestOpenOrCreateIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	first, err := OpenOrCreate(dir)
	if err != nil {
		t.Fatalf("OpenOrCreate: %v", err)
	}
	second, err := OpenOrCreate(dir)
	if err != nil {
		t.Fatalf("OpenOrCreate again: %v", err)
	}
	if first.Fingerprint() != second.Fingerprint() {
		t.Error("OpenOrCreate generated a second authority instead of loading the first")
	}
}

// TestRefusesAWorldReadableKey: the whole arrangement rests on one person holding the key.
func TestRefusesAWorldReadableKey(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("permissions are not expressed this way on Windows")
	}
	dir := t.TempDir()
	a, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := os.Chmod(a.KeyPath(), 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	_, err = Open(dir)
	if err == nil {
		t.Fatal("a group- and world-readable CA key was accepted")
	}
	if !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("error %q should say how to fix it", err)
	}
}

func TestLeafChainsToTheAuthority(t *testing.T) {
	a, err := Create(t.TempDir())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	leaf, err := a.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	if leaf.Leaf.IsCA {
		t.Error("a leaf certificate must not be a CA")
	}
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:     a.Pool(),
		DNSName:   "api.example.com",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("leaf does not verify against the authority: %v", err)
	}
	// A name the leaf was not minted for must not verify.
	if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
		Roots:   a.Pool(),
		DNSName: "elsewhere.example.com",
	}); err == nil {
		t.Error("a leaf for one host verified for another")
	}
}

func TestLeafIsCachedPerHost(t *testing.T) {
	a, err := Create(t.TempDir())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	first, err := a.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	second, err := a.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor again: %v", err)
	}
	if first != second {
		t.Error("a second call minted a new certificate instead of reusing the cached one")
	}
	other, err := a.LeafFor("other.example.com")
	if err != nil {
		t.Fatalf("LeafFor other: %v", err)
	}
	if other.Leaf.SerialNumber.Cmp(first.Leaf.SerialNumber) == 0 {
		t.Error("two hosts share a certificate serial")
	}
}

func TestLeafForAnAddressUsesAnIPSAN(t *testing.T) {
	a, err := Create(t.TempDir())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	leaf, err := a.LeafFor("203.0.113.7")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	if len(leaf.Leaf.IPAddresses) != 1 || leaf.Leaf.IPAddresses[0].String() != "203.0.113.7" {
		t.Errorf("IP SANs = %v, want the address", leaf.Leaf.IPAddresses)
	}
	if len(leaf.Leaf.DNSNames) != 0 {
		t.Errorf("an address literal was given DNS names: %v", leaf.Leaf.DNSNames)
	}
}

func TestLeafIsUsableAsATLSCertificate(t *testing.T) {
	a, err := Create(t.TempDir())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	leaf, err := a.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	// The chain the proxy presents must include the authority, or a client that trusts
	// only the root cannot build a path.
	if len(leaf.Certificate) != 2 {
		t.Fatalf("chain has %d certificates, want leaf + CA", len(leaf.Certificate))
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{*leaf}}
	if _, err := cfg.Certificates[0].Leaf.Verify(x509.VerifyOptions{Roots: a.Pool(), DNSName: "api.example.com"}); err != nil {
		t.Errorf("verify: %v", err)
	}
}

func TestRegeneratingReplacesTheAuthority(t *testing.T) {
	dir := t.TempDir()
	first, err := Create(dir)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	second, err := Create(dir)
	if err != nil {
		t.Fatalf("Create again: %v", err)
	}
	if first.Fingerprint() == second.Fingerprint() {
		t.Fatal("regenerating produced the same authority")
	}
	// Revocation, such as it is: certificates from the old authority no longer chain.
	oldLeaf, err := first.LeafFor("api.example.com")
	if err != nil {
		t.Fatalf("LeafFor: %v", err)
	}
	if _, err := oldLeaf.Leaf.Verify(x509.VerifyOptions{Roots: second.Pool(), DNSName: "api.example.com"}); err == nil {
		t.Error("a certificate from the retired authority still verifies against the new one")
	}
}

// TestExportedMaterialHasNoPrivateKey guards the one mistake that would matter most.
func TestExportedMaterialHasNoPrivateKey(t *testing.T) {
	a, err := Create(t.TempDir())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	printed := string(a.CertPEM()) + "\n" + a.Info().String() + "\n" + a.Fingerprint()
	for _, forbidden := range []string{"PRIVATE KEY", "BEGIN EC PRIVATE"} {
		if strings.Contains(printed, forbidden) {
			t.Errorf("exported material contains %q:\n%s", forbidden, printed)
		}
	}
	if !strings.Contains(string(a.CertPEM()), "BEGIN CERTIFICATE") {
		t.Error("CertPEM did not return a certificate")
	}
	keyBytes, err := os.ReadFile(a.KeyPath())
	if err != nil {
		t.Fatalf("read key: %v", err)
	}
	if strings.Contains(printed, strings.TrimSpace(string(keyBytes))) {
		t.Error("the private key appears in exported material")
	}
}
