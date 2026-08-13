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

// ErrWrongPassphrase reports that the store did not decrypt.
//
// GCM cannot tell a wrong key from a damaged file, so neither can this — the two are one
// error on purpose. It is a sentinel so that the command layer can recognise the one failure
// every subcommand shares and offer the way out, rather than matching on the message text.
var ErrWrongPassphrase = errors.New("the secret store did not decrypt")

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
	return NewValue(v), nil
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
