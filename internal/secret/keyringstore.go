package secret

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
)

// KeyringStore keeps every secret in the operating system's own store and keeps the list of
// NAMES in a file beside it.
//
// # Why the names are not in the keyring
//
// Because enumeration is the one thing the three platforms cannot agree on, and a name is not
// a secret. macOS can only list a keychain by dumping it, which prompts the user for each
// item; libsecret searches by attribute; Windows enumerates by filter. Building `boks secret
// ls` on those would mean three code paths, three failure modes, and — on macOS — a permission
// prompt storm for a command that prints a list of names.
//
// Names are therefore an index file: plain JSON, no encryption, because it holds no secret. It
// is a cache of what the keyring should contain rather than the truth: Lookup asks the keyring
// and believes its answer, so an index that has drifted costs a listing that is wrong until
// the next write, not a secret that cannot be read.
//
// # What this means for the threat model
//
// A FileStore holds every secret in one encrypted blob whose key is an environment variable —
// so anything that can read the file and the environment has everything. Here the values are
// held by the OS, which decides who may read them: on macOS that is per-application ACLs, on
// Linux the login keyring, on Windows the user's credential store. Boks never holds a
// passphrase, and there is nothing to leak from the environment.
//
// What is NOT claimed: that this stops a process running as the user from asking the OS for
// the same secrets. It runs as you and can ask as you. The gain is that the secrets are at
// rest under the OS's protection rather than under a passphrase people paste into shell
// profiles, and that stealing the state directory no longer steals the credentials.
type KeyringStore struct {
	ring  Keyring
	index string

	mu sync.Mutex
}

// NewKeyringStore returns a store backed by ring, with its name index at indexPath.
func NewKeyringStore(ring Keyring, indexPath string) *KeyringStore {
	return &KeyringStore{ring: ring, index: indexPath}
}

// DefaultIndexPath is where the name index lives, beside the encrypted file a FileStore would
// have used so that everything Boks keeps is under one directory.
func DefaultIndexPath(stateDir string) string {
	return filepath.Join(stateDir, "secrets-index.json")
}

// Describe names this store and where its index lives.
func (s *KeyringStore) Describe() string {
	return fmt.Sprintf("the %s (index: %s)", s.ring.Describe(), s.index)
}

// Path reports the index, which is the only file this store has. Named to match FileStore so
// that a caller printing "where are my secrets" needs no type switch.
func (s *KeyringStore) Path() string { return s.index }

// Lookup implements Provider.
func (s *KeyringStore) Lookup(ctx context.Context, name string) (Value, error) {
	stored, err := s.ring.Get(ctx, name)
	if err != nil {
		return Value{}, err
	}
	if IsOAuth(stored) {
		// The same refusal FileStore makes, for the same reason: returning the record
		// here is the difference between a clear error and a JSON blob sent to an origin
		// as an Authorization header.
		return Value{}, fmt.Errorf("secret %q is an oauth credential; use --oauth rather than --inject for it", name)
	}
	return NewValue(stored), nil
}

// LookupOAuth implements OAuthProvider.
func (s *KeyringStore) LookupOAuth(ctx context.Context, name string) (OAuthTokens, error) {
	r, err := s.LookupOAuthRecord(ctx, name)
	if err != nil {
		return OAuthTokens{}, err
	}
	return r.Tokens(), nil
}

// LookupOAuthRecord returns the whole stored OAuth credential — tokens and shape.
func (s *KeyringStore) LookupOAuthRecord(ctx context.Context, name string) (OAuthRecord, error) {
	stored, err := s.ring.Get(ctx, name)
	if err != nil {
		return OAuthRecord{}, err
	}
	return decodeOAuth(name, stored)
}

// Set stores a plain credential.
func (s *KeyringStore) Set(name string, v Value) error {
	// Reveal, not String. Value.String() redacts — that is its whole purpose — so the
	// obvious spelling here stores the literal text "[redacted]" as every secret. Caught
	// by TestKeyringStoreRoundTrip, which is why that test compares the value that
	// reached the keyring rather than only what came back out of the store.
	return s.put(name, v.Reveal())
}

// SetOAuth stores an OAuth credential, replacing whatever was under that name.
func (s *KeyringStore) SetOAuth(name string, r OAuthRecord) error {
	encoded, err := encodeOAuth(r)
	if err != nil {
		return err
	}
	return s.put(name, encoded)
}

// SaveOAuth implements the refresher's write-back: it replaces the tokens and keeps the rest
// of the record as it stands.
func (s *KeyringStore) SaveOAuth(ctx context.Context, name string, tokens OAuthTokens) error {
	r, err := s.LookupOAuthRecord(ctx, name)
	if err != nil {
		return err
	}
	return s.SetOAuth(name, r.WithTokens(tokens))
}

// put writes a value to the keyring and then records the name.
//
// The ORDER is deliberate and is the answer to "what if this is interrupted". Keyring first
// means a crash between the two leaves a secret the index does not list: `boks secret ls`
// misses it, and the next Set repairs the index. The other order would leave a name listed
// with no value behind it, so `boks run` would find a credential the store says exists and
// fail at injection — a listing that is briefly incomplete is a far better failure than one
// that is confidently wrong.
func (s *KeyringStore) put(name, value string) error {
	if err := validKeyringName(name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ring.Set(context.Background(), name, value); err != nil {
		return err
	}
	names, err := s.readIndex()
	if err != nil {
		return err
	}
	if !slices.Contains(names, name) {
		names = append(names, name)
		slices.Sort(names)
		return s.writeIndex(names)
	}
	return nil
}

// Delete removes a credential. The index is updated even when the keyring says the value was
// already gone, so that a listing cannot keep naming something no store holds.
func (s *KeyringStore) Delete(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ring.Delete(context.Background(), name); err != nil && !errors.Is(err, ErrNotFound) {
		return err
	}
	names, err := s.readIndex()
	if err != nil {
		return err
	}
	if i := slices.Index(names, name); i >= 0 {
		return s.writeIndex(slices.Delete(names, i, i+1))
	}
	return nil
}

// Names lists the stored credentials.
func (s *KeyringStore) Names() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readIndex()
}

// Entries lists the stored credentials with their kind.
//
// The kind is not in the index, because it would be a second thing to keep in step with the
// keyring; it is read per name instead. A name the index lists and the keyring has lost is
// SKIPPED rather than reported as an error: the index is a cache, and one stale entry must not
// make `boks secret ls` fail entirely.
func (s *KeyringStore) Entries() ([]Entry, error) {
	names, err := s.Names()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	entries := make([]Entry, 0, len(names))
	for _, name := range names {
		stored, err := s.ring.Get(ctx, name)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		entries = append(entries, Entry{Name: name, OAuth: IsOAuth(stored)})
	}
	return entries, nil
}

// readIndex returns the recorded names. A missing index is an empty store, not an error: that
// is what a machine that has never stored a secret looks like.
func (s *KeyringStore) readIndex() ([]string, error) {
	data, err := os.ReadFile(s.index)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("reading the secret index %s: %w", s.index, err)
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("the secret index %s is unreadable: %w", s.index, err)
	}
	return names, nil
}

// writeIndex replaces the index through a temporary file, so an interrupted write cannot leave
// a half-written list where a whole one was.
func (s *KeyringStore) writeIndex(names []string) error {
	if err := os.MkdirAll(filepath.Dir(s.index), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(names, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.index + ".tmp"
	// 0o600 even though this holds no secret: the list of services a user has credentials
	// for is worth keeping to themselves, and a wider mode here would be noticed by nobody.
	if err := os.WriteFile(tmp, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.index)
}

// Store is what a secret store does, so that a caller can hold either the OS keyring or the
// encrypted file without caring which.
//
// Introduced when the keyring arrived: before it there was one implementation and every caller
// named it concretely, which is fine until there are two. It is written from what the CLI
// actually calls rather than from what FileStore happens to expose, so that adding a store
// means implementing what is used and not everything that exists.
type Store interface {
	// Provider is the read path the proxy uses. Everything else here is the CLI's.
	Provider

	LookupOAuth(ctx context.Context, name string) (OAuthTokens, error)
	LookupOAuthRecord(ctx context.Context, name string) (OAuthRecord, error)
	SetOAuth(name string, r OAuthRecord) error
	SaveOAuth(ctx context.Context, name string, tokens OAuthTokens) error
	Set(name string, v Value) error
	Delete(name string) error
	Names() ([]string, error)
	Entries() ([]Entry, error)
	// Path is where the store keeps what it keeps on disk — the encrypted file for a
	// FileStore, the name index for a KeyringStore. Callers print it when telling a user
	// where their credentials live, so it must never be empty.
	Path() string
	// Describe names the store in a form a user can act on.
	//
	// It exists because there are now two stores and which one a command uses depends on
	// the environment: a terminal with BOKS_SECRETS_PASSPHRASE set reads the encrypted
	// file, and one without it reads the OS keyring. Two terminals can therefore disagree
	// completely about what is stored, and with only a path in the output the disagreement
	// looks like a lost secret rather than two different stores. Every command that prints
	// where a credential went prints this.
	Describe() string
}

// Both stores are the same shape to their callers, and the compiler is what keeps them that
// way: a method added to the interface without an implementation in either fails here rather
// than at the call site.
var (
	_ Store = (*FileStore)(nil)
	_ Store = (*KeyringStore)(nil)
)
