package cli

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/ca"
)

func TestCaShowWithoutAnAuthority(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	out, _, err := runCLI(t, "", "ca", "show", "-dir", dir)
	if err != nil {
		t.Fatalf("ca show: %v", err)
	}
	if !strings.Contains(out, "no certificate authority") {
		t.Errorf("output = %q", out)
	}
}

func TestCaShowCreatesAndDescribes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	out, _, err := runCLI(t, "", "ca", "show", "-dir", dir, "-create")
	if err != nil {
		t.Fatalf("ca show -create: %v", err)
	}
	for _, want := range []string{"sha256:", "Boks local CA", "never leaves this machine"} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not contain %q:\n%s", want, out)
		}
	}
	// Nothing that prints the authority may print its key.
	if strings.Contains(out, "PRIVATE KEY") {
		t.Errorf("ca show printed key material:\n%s", out)
	}
}

func TestCaExportWritesOnlyTheCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	out, _, err := runCLI(t, "", "ca", "export", "-dir", dir)
	if err != nil {
		t.Fatalf("ca export: %v", err)
	}
	if !strings.Contains(out, "BEGIN CERTIFICATE") {
		t.Errorf("export did not print a certificate:\n%s", out)
	}
	if strings.Contains(out, "PRIVATE KEY") {
		t.Errorf("export printed a private key:\n%s", out)
	}

	file := filepath.Join(t.TempDir(), "boks-ca.pem")
	if _, _, err := runCLI(t, "", "ca", "export", "-dir", dir, "-o", file); err != nil {
		t.Fatalf("ca export -o: %v", err)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != out {
		t.Error("the exported file differs from what was printed")
	}
}

// TestCaEnvCarriesTheSameCertificate: the environment variable exists because Node and
// Python ignore the system trust store, so it has to carry exactly what the file does.
func TestCaEnvCarriesTheSameCertificate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	pem, _, err := runCLI(t, "", "ca", "export", "-dir", dir)
	if err != nil {
		t.Fatalf("ca export: %v", err)
	}
	out, _, err := runCLI(t, "", "ca", "env", "-dir", dir)
	if err != nil {
		t.Fatalf("ca env: %v", err)
	}
	name, encoded, ok := strings.Cut(strings.TrimSpace(out), "=")
	if !ok || name != ca.CertEnvVar {
		t.Fatalf("ca env printed %q, want %s=<base64>", out, ca.CertEnvVar)
	}
	if strings.HasPrefix(name, "PROXY_CA") || strings.Contains(strings.ToLower(name), "docker") {
		t.Errorf("the environment variable name is not ours: %q", name)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decoding the value: %v", err)
	}
	if string(decoded) != pem {
		t.Error("the environment variable does not carry the same certificate as the file")
	}
	if strings.Contains(string(decoded), "PRIVATE KEY") {
		t.Error("ca env carried key material")
	}
}

func TestCaRegenerateNeedsConfirmationAndChangesTheFingerprint(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "ca")
	before, _, err := runCLI(t, "", "ca", "export", "-dir", dir)
	if err != nil {
		t.Fatalf("ca export: %v", err)
	}

	if _, _, err := runCLI(t, "n\n", "ca", "regenerate", "-dir", dir); err == nil {
		t.Error("regenerate proceeded without confirmation")
	}
	unchanged, _, err := runCLI(t, "", "ca", "export", "-dir", dir)
	if err != nil {
		t.Fatalf("ca export: %v", err)
	}
	if unchanged != before {
		t.Error("a refused regeneration still replaced the authority")
	}

	if _, _, err := runCLI(t, "y\n", "ca", "regenerate", "-dir", dir); err != nil {
		t.Fatalf("ca regenerate: %v", err)
	}
	after, _, err := runCLI(t, "", "ca", "export", "-dir", dir)
	if err != nil {
		t.Fatalf("ca export: %v", err)
	}
	if after == before {
		t.Error("regenerate did not replace the authority")
	}
}

// TestPolicyLsDisclosesInterception: configuring a credential rule buys TLS interception
// for that host, and the user has to be told so without asking.
func TestPolicyLsDisclosesInterception(t *testing.T) {
	out, _, err := runCLI(t, "", "policy", "ls", "-policy", "locked",
		"-allow", "api.example.com:443",
		"-inject", "tok@api.example.com=x-api-key",
		"-guest-credential", "tok=EXAMPLE_TOKEN=sk-example-placeholder0000000000")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	for _, want := range []string{
		"TLS INTERCEPTION",
		"DECRYPT",
		"api.example.com",
		"forward-bypass",
		"boks ca export",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output does not disclose %q:\n%s", want, out)
		}
	}
}

// TestPolicyLsSaysNothingAboutInterceptionWithoutRules: the notice must not become
// background noise that appears on every run.
func TestPolicyLsSaysNothingAboutInterceptionWithoutRules(t *testing.T) {
	out, _, err := runCLI(t, "", "policy", "ls", "-policy", "locked", "-allow", "api.example.com:443")
	if err != nil {
		t.Fatalf("policy ls: %v", err)
	}
	if strings.Contains(out, "TLS INTERCEPTION") {
		t.Errorf("interception was announced with no credential rule configured:\n%s", out)
	}
}
