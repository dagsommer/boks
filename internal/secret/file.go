package secret

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// FileStore is a passphrase-encrypted secret file: AES-256-GCM over a JSON map, with the
// key derived from a passphrase by PBKDF2-HMAC-SHA256.
//
// This is the portable fallback, and it is honest about what it is worth. The security of
// the file is the security of the passphrase, and the passphrase has to come from
// somewhere — an environment variable in the shell that runs Boks, most likely. That is
// better than a plaintext file and clearly worse than an OS keychain, which is why the
// keychains are the intended destination and Provider is an interface.
//
// What is deliberately *not* offered is a key file sitting next to the encrypted file.
// That arrangement encrypts nothing against anyone who can read the directory, while
// looking like it does.
type FileStore struct {
	mu         sync.Mutex
	path       string
	passphrase []byte
	iter       int
}

// envelope is the on-disk format. Everything outside ct is public.
type envelope struct {
	Version int    `json:"version"`
	KDF     string `json:"kdf"`
	Iter    int    `json:"iter"`
	Salt    []byte `json:"salt"`
	Nonce   []byte `json:"nonce"`
	Data    []byte `json:"data"`
}

const (
	envelopeVersion = 1
	kdfName         = "pbkdf2-hmac-sha256"
	// defaultIterations is a cost that keeps `boks run` responsive while making an
	// offline guess of a weak passphrase expensive. It is recorded in the file, so it
	// can be raised later without breaking existing stores.
	defaultIterations = 600_000
	saltLen           = 16
	keyLen            = 32
)

// ErrNoPassphrase reports that the store cannot be opened because no passphrase was given.
var ErrNoPassphrase = errors.New("no passphrase for the secret store")

// PassphraseEnv is the environment variable the CLI reads the passphrase from.
const PassphraseEnv = "BOKS_SECRETS_PASSPHRASE"

// OpenFile binds a store to a path and passphrase. The file need not exist yet; it is
// created by the first Set.
func OpenFile(path string, passphrase []byte) (*FileStore, error) {
	if len(passphrase) == 0 {
		return nil, fmt.Errorf("%w: set %s", ErrNoPassphrase, PassphraseEnv)
	}
	if path == "" {
		return nil, errors.New("no secret store path")
	}
	return &FileStore{path: path, passphrase: passphrase, iter: defaultIterations}, nil
}

// Lookup implements Provider.
func (s *FileStore) Lookup(_ context.Context, name string) (Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return Value{}, err
	}
	v, ok := m[name]
	if !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	if IsOAuth(v) {
		// Refusing here rather than returning the record is the difference between a
		// clear error and a JSON blob sent to an origin as an Authorization header.
		return Value{}, fmt.Errorf("secret %q is an oauth credential; use --oauth rather than --inject for it", name)
	}
	return NewValue(v), nil
}

// LookupOAuth implements OAuthProvider.
func (s *FileStore) LookupOAuth(ctx context.Context, name string) (OAuthTokens, error) {
	r, err := s.LookupOAuthRecord(ctx, name)
	if err != nil {
		return OAuthTokens{}, err
	}
	return r.Tokens(), nil
}

// LookupOAuthRecord returns the whole stored OAuth credential — tokens and shape.
//
// The CLI needs this to hand a sandbox's supervisor a self-contained credential; the
// injection path needs only the tokens, and asks for those.
func (s *FileStore) LookupOAuthRecord(_ context.Context, name string) (OAuthRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return OAuthRecord{}, err
	}
	v, ok := m[name]
	if !ok {
		return OAuthRecord{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return decodeOAuth(name, v)
}

// SetOAuth stores an OAuth credential, replacing whatever was under that name.
func (s *FileStore) SetOAuth(name string, r OAuthRecord) error {
	if name == "" {
		return errors.New("a secret needs a name")
	}
	if err := r.Validate(); err != nil {
		return err
	}
	encoded, err := encodeOAuth(r)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	m[name] = encoded
	return s.save(m)
}

// SaveOAuth implements OAuthSaver: it replaces the token pair of an existing credential and
// leaves its configuration alone.
//
// A refresh rotates values, never shape. Reading the record back rather than holding one in
// memory is what keeps a rotation from resurrecting configuration the user has since edited.
func (s *FileStore) SaveOAuth(_ context.Context, name string, tokens OAuthTokens) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	v, ok := m[name]
	if !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	r, err := decodeOAuth(name, v)
	if err != nil {
		return err
	}
	encoded, err := encodeOAuth(r.WithTokens(tokens))
	if err != nil {
		return err
	}
	m[name] = encoded
	return s.save(m)
}

// Entry is one stored credential, named and classified. It never carries a value.
type Entry struct {
	Name  string
	OAuth bool
}

// Entries lists the stored credentials with their kind, sorted. Names and kinds only: there
// is no subcommand that prints a value, and there should not be.
func (s *FileStore) Entries() ([]Entry, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	out := make([]Entry, 0, len(m))
	for name, v := range m {
		out = append(out, Entry{Name: name, OAuth: IsOAuth(v)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out, nil
}

// Set stores a secret, creating the file if needed.
func (s *FileStore) Set(name string, v Value) error {
	if name == "" {
		return errors.New("a secret needs a name")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	m[name] = v.Reveal()
	return s.save(m)
}

// Delete removes a secret. Removing one that is not there is not an error.
func (s *FileStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return err
	}
	delete(m, name)
	return s.save(m)
}

// Names lists the stored secret names, sorted.
//
// This is a host-side convenience for the CLI. It is not reachable from a guest, and no
// code path exposes it over a socket — see the package documentation.
func (s *FileStore) Names() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.load()
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// Path reports where the store lives.
func (s *FileStore) Path() string { return s.path }

func (s *FileStore) load() (map[string]string, error) {
	raw, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading secret store %s: %w", s.path, err)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("secret store %s is not a boks secret file", s.path)
	}
	if env.Version != envelopeVersion || env.KDF != kdfName {
		return nil, fmt.Errorf("secret store %s uses an unsupported format (version %d, kdf %q)", s.path, env.Version, env.KDF)
	}
	aead, err := s.aead(env.Salt, env.Iter)
	if err != nil {
		return nil, err
	}
	plain, err := aead.Open(nil, env.Nonce, env.Data, nil)
	if err != nil {
		// GCM cannot tell a wrong key from a damaged file, and neither can we.
		return nil, fmt.Errorf("cannot decrypt %s: wrong passphrase, or the file has been modified", s.path)
	}
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		return nil, fmt.Errorf("secret store %s decrypted to something unexpected", s.path)
	}
	return m, nil
}

func (s *FileStore) save(m map[string]string) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encoding secrets: %w", err)
	}
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generating salt: %w", err)
	}
	aead, err := s.aead(salt, s.iter)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generating nonce: %w", err)
	}
	env := envelope{
		Version: envelopeVersion,
		KDF:     kdfName,
		Iter:    s.iter,
		Salt:    salt,
		Nonce:   nonce,
		Data:    aead.Seal(nil, nonce, plain, nil),
	}
	out, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("encoding secret store: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return fmt.Errorf("creating secret store directory: %w", err)
	}
	// Write and rename, so an interrupted save cannot leave a store that decrypts to
	// half a file. The temporary file is created in the destination directory so the
	// rename stays on one filesystem.
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".boks-secrets-*")
	if err != nil {
		return fmt.Errorf("creating temporary secret file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("setting permissions on the secret store: %w", err)
	}
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("writing secret store: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing secret store: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("replacing secret store: %w", err)
	}
	return nil
}

func (s *FileStore) aead(salt []byte, iter int) (cipher.AEAD, error) {
	if iter <= 0 {
		iter = defaultIterations
	}
	key, err := pbkdf2.Key(sha256.New, string(s.passphrase), salt, iter, keyLen)
	if err != nil {
		return nil, fmt.Errorf("deriving the secret store key: %w", err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("initialising the secret store cipher: %w", err)
	}
	return cipher.NewGCM(block)
}

// DefaultPath is where the encrypted store lives when no path is given. It sits beside the
// rest of Boks' host-side state and is never shared into a guest.
func DefaultPath(stateDir string) string { return filepath.Join(stateDir, "secrets.json") }
