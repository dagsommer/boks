// Package ca is the local certificate authority Boks uses on the one occasion it has to
// terminate TLS: attaching a credential to an HTTPS request for a host you configured by
// hand. Nothing else in Boks mints a certificate, and nothing else should.
//
// # Why this exists at all
//
// Injecting a header into a request means reading the request. For HTTPS that means
// terminating the TLS session, which means presenting a certificate the client accepts,
// which means a certificate authority the client trusts. There is no version of credential
// injection over HTTPS that avoids this; a product that offers the feature has made this
// trade whether or not it says so. Boks says so.
//
// # What the trade actually is
//
//   - The CA private key is generated on the host, stored under the state directory with
//     owner-only permissions, and **never leaves the host**. No guest, no image, no
//     annotation and no mount carries it. There is no code path in Boks that reads it out.
//   - The CA *certificate* is public. A guest that holds it can verify certificates Boks
//     minted; it cannot mint any, because minting needs the key. Exfiltrating the
//     certificate gains an attacker nothing.
//   - A guest that trusts this CA is trusting **the host it is already running on**. That is
//     not a new trust relationship: the host already owns the guest's kernel, disk and
//     clock. What it does change is that traffic to intercepted hosts is readable by the
//     host process, so the guest can no longer assume end-to-end confidentiality with those
//     origins.
//   - Install the CA in a guest, never in your host trust store. In the guest its blast
//     radius is one sandbox. In your login keychain it is every TLS connection you make,
//     and anyone who reads the key file owns them.
//
// # Leaves
//
// Leaf certificates are minted per host, kept in memory only, and never written to disk.
// They are short-lived and are re-minted on every proxy start; there is nothing to revoke
// individually. Revoking the authority means regenerating it, after which previously issued
// leaves chain to a certificate no guest trusts.
package ca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// ErrNotFound reports that no authority exists yet at a location.
var ErrNotFound = errors.New("no boks certificate authority")

const (
	certFile = "ca-cert.pem"
	keyFile  = "ca-key.pem"

	// caLifetime is long enough that regeneration is not a weekly chore and short enough
	// that a forgotten key on an old laptop eventually stops being useful.
	caLifetime = 3 * 365 * 24 * time.Hour
	// leafLifetime only has to outlive a proxy process; leaves live in memory.
	leafLifetime = 30 * 24 * time.Hour
	// backdate absorbs clock skew between host and guest.
	backdate = time.Hour
)

// Authority signs the leaf certificates the proxy presents when it intercepts a flow.
// It is safe for concurrent use.
type Authority struct {
	dir  string
	cert *x509.Certificate
	key  *ecdsa.PrivateKey

	mu     sync.Mutex
	leaves map[string]*tls.Certificate

	now func() time.Time
}

// DefaultDir is where the authority lives, beside the rest of Boks' host-side state.
func DefaultDir(stateDir string) string { return filepath.Join(stateDir, "ca") }

// Open loads an existing authority. It returns ErrNotFound if there is none, so a caller
// can tell "not set up" apart from "broken".
func Open(dir string) (*Authority, error) {
	certPath, keyPath := filepath.Join(dir, certFile), filepath.Join(dir, keyFile)
	certPEM, err := os.ReadFile(certPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w in %s", ErrNotFound, dir)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", certPath, err)
	}
	if err := checkPrivatePerm(keyPath); err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%s exists but %s does not; the authority cannot sign. Regenerate it with 'boks ca regenerate'", certPath, keyPath)
	}
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", keyPath, err)
	}

	cert, err := parseCert(certPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", certPath, err)
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", keyPath, err)
	}
	if !key.PublicKey.Equal(cert.PublicKey) {
		return nil, fmt.Errorf("%s does not match %s; regenerate the authority with 'boks ca regenerate'", keyPath, certPath)
	}
	return &Authority{dir: dir, cert: cert, key: key, leaves: map[string]*tls.Certificate{}, now: time.Now}, nil
}

// OpenOrCreate loads the authority, generating one on first use.
func OpenOrCreate(dir string) (*Authority, error) {
	a, err := Open(dir)
	if err == nil {
		return a, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	return Create(dir)
}

// Create generates a new authority, replacing any that is already there.
//
// Replacing is what "revoke" means here. There is no revocation list a guest would check;
// the guest trusts exactly the certificate it was given, so retiring an authority is
// retiring that file.
func Create(dir string) (*Authority, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating the CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			Organization: []string{"Boks"},
			CommonName:   "Boks local CA (" + hostLabel() + ")",
		},
		NotBefore:             now.Add(-backdate),
		NotAfter:              now.Add(caLifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		// A CA that can only sign leaves cannot be used to mint another CA if the key
		// ever escapes.
		MaxPathLen:     0,
		MaxPathLenZero: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("creating the CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parsing the CA certificate just created: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %w", dir, err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("encoding the CA key: %w", err)
	}
	// The key is written first and the certificate second, so a half-written authority
	// fails to load rather than loading with a key that signs nothing.
	if err := writePrivate(filepath.Join(dir, keyFile), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})); err != nil {
		return nil, err
	}
	if err := writePrivate(filepath.Join(dir, certFile), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})); err != nil {
		return nil, err
	}
	return &Authority{dir: dir, cert: cert, key: key, leaves: map[string]*tls.Certificate{}, now: time.Now}, nil
}

// Dir reports where the authority is stored.
func (a *Authority) Dir() string { return a.dir }

// CertPath is the file a guest would be given.
func (a *Authority) CertPath() string { return filepath.Join(a.dir, certFile) }

// KeyPath is the file that must never be given to anything.
func (a *Authority) KeyPath() string { return filepath.Join(a.dir, keyFile) }

// Certificate returns the authority's certificate.
func (a *Authority) Certificate() *x509.Certificate { return a.cert }

// CertPEM returns the authority's certificate in PEM form. This is the only material this
// package will hand out, and it is public by nature.
func (a *Authority) CertPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: a.cert.Raw})
}

// CertEnvVar is the environment variable a guest can read the CA from, base64-encoded.
//
// A file in the guest's system trust store is not enough on its own: Node ships its own
// root store, Python's certifi ships another, and both ignore the system one unless told
// otherwise. Handing the certificate over in an environment variable as well lets a guest's
// setup write it wherever each runtime actually looks. Both routes carry the same public
// certificate; neither carries the key.
const CertEnvVar = "BOKS_CA_CERT_B64"

// CertBase64 returns the certificate encoded for CertEnvVar.
func (a *Authority) CertBase64() string {
	return base64.StdEncoding.EncodeToString(a.CertPEM())
}

// Pool returns a certificate pool trusting only this authority, for a client that should
// accept intercepted flows and nothing else.
func (a *Authority) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// Fingerprint is the SHA-256 of the certificate's DER, the value to compare when checking
// that a guest trusts the authority you think it does.
func (a *Authority) Fingerprint() string {
	sum := sha256.Sum256(a.cert.Raw)
	hexed := strings.ToUpper(hex.EncodeToString(sum[:]))
	var b strings.Builder
	for i := 0; i < len(hexed); i += 2 {
		if i > 0 {
			b.WriteByte(':')
		}
		b.WriteString(hexed[i : i+2])
	}
	return b.String()
}

// LeafFor mints — or returns from cache — a certificate for one host.
//
// The host comes from the proxy's already-normalised policy target, never from the
// client's ClientHello. A guest must not be able to choose what gets signed.
func (a *Authority) LeafFor(host string) (*tls.Certificate, error) {
	if host == "" {
		return nil, errors.New("cannot mint a certificate for an empty host")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	if leaf, ok := a.leaves[host]; ok {
		if x509cert := leaf.Leaf; x509cert != nil && now.Before(x509cert.NotAfter.Add(-time.Minute)) {
			return leaf, nil
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generating a key for %s: %w", host, err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	notAfter := now.Add(leafLifetime)
	if notAfter.After(a.cert.NotAfter) {
		notAfter = a.cert.NotAfter
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Boks intercepted"}, CommonName: host},
		NotBefore:    now.Add(-backdate),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		// Not a CA: a leaf that could sign would turn one intercepted host into all of
		// them for anything that trusts the chain.
		BasicConstraintsValid: true,
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		tmpl.IPAddresses = []net.IP{net.IP(addr.AsSlice())}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, a.cert, &key.PublicKey, a.key)
	if err != nil {
		return nil, fmt.Errorf("signing a certificate for %s: %w", host, err)
	}
	leafCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, fmt.Errorf("parsing the certificate just signed for %s: %w", host, err)
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, a.cert.Raw},
		PrivateKey:  key,
		Leaf:        leafCert,
	}
	a.leaves[host] = leaf
	return leaf, nil
}

// Info is the human-readable summary `boks ca show` prints. It carries no key material.
type Info struct {
	Dir         string
	CertPath    string
	KeyPath     string
	Subject     string
	Serial      string
	Fingerprint string
	NotBefore   time.Time
	NotAfter    time.Time
}

// Info describes the authority.
func (a *Authority) Info() Info {
	return Info{
		Dir:         a.dir,
		CertPath:    a.CertPath(),
		KeyPath:     a.KeyPath(),
		Subject:     a.cert.Subject.String(),
		Serial:      a.cert.SerialNumber.String(),
		Fingerprint: a.Fingerprint(),
		NotBefore:   a.cert.NotBefore,
		NotAfter:    a.cert.NotAfter,
	}
}

func (i Info) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "subject:     %s\n", i.Subject)
	fmt.Fprintf(&b, "serial:      %s\n", i.Serial)
	fmt.Fprintf(&b, "sha256:      %s\n", i.Fingerprint)
	fmt.Fprintf(&b, "valid:       %s .. %s\n", i.NotBefore.Format(time.RFC3339), i.NotAfter.Format(time.RFC3339))
	fmt.Fprintf(&b, "certificate: %s  (public; this is what a guest is given)\n", i.CertPath)
	fmt.Fprintf(&b, "private key: %s  (never leaves this machine)\n", i.KeyPath)
	return b.String()
}

func newSerial() (*big.Int, error) {
	// 128 random bits: unpredictable serials are the cheap defence against a chosen-prefix
	// collision on the signature.
	max := new(big.Int).Lsh(big.NewInt(1), 128)
	n, err := rand.Int(rand.Reader, max)
	if err != nil {
		return nil, fmt.Errorf("generating a certificate serial: %w", err)
	}
	return n.Add(n, big.NewInt(1)), nil
}

func parseCert(pemBytes []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("is not a PEM certificate")
	}
	return x509.ParseCertificate(block.Bytes)
}

func parseKey(pemBytes []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("is not a PEM private key")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("is not a usable CA key: %w", err)
	}
	return key, nil
}

// writePrivate writes a file only its owner can read, replacing any existing one.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".boks-ca-*")
	if err != nil {
		return fmt.Errorf("creating a temporary file in %s: %w", filepath.Dir(path), err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting permissions on %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("replacing %s: %w", path, err)
	}
	return nil
}

// checkPrivatePerm refuses to use a signing key other users can read.
//
// The whole value of the arrangement is that the key stays with one person on one machine.
// A group-readable key quietly turns the authority into everyone's authority, so this fails
// loudly with the command that fixes it. Windows does not express permissions this way and
// is skipped rather than checked badly.
func checkPrivatePerm(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil // absence is reported by the caller, with better context
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf("the CA private key %s is readable by other users (mode %04o).\n"+
			"Boks will not sign with it. Fix it with:\n\n  chmod 600 %s\n", path, mode, path)
	}
	return nil
}

func hostLabel() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "local"
	}
	return name
}
