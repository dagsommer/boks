package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeKeyring is an in-memory Keyring, so the store's own logic — the index, the ordering of
// writes, the OAuth encoding — is tested on every platform. The three real backends shell out
// to an OS service that none of this project's machines have.
type fakeKeyring struct {
	values map[string]string
	// failSet makes Set fail, to exercise the case where the OS store refuses a write.
	failSet error
	// sets counts writes, so a test can prove the keyring was written BEFORE the index.
	sets int
}

func newFakeKeyring() *fakeKeyring { return &fakeKeyring{values: map[string]string{}} }

func (f *fakeKeyring) Describe() string { return "fake keyring" }

func (f *fakeKeyring) Get(_ context.Context, name string) (string, error) {
	v, ok := f.values[name]
	if !ok {
		return "", fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return v, nil
}

func (f *fakeKeyring) Set(_ context.Context, name, value string) error {
	if f.failSet != nil {
		return f.failSet
	}
	f.sets++
	f.values[name] = value
	return nil
}

func (f *fakeKeyring) Delete(_ context.Context, name string) error {
	if _, ok := f.values[name]; !ok {
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	delete(f.values, name)
	return nil
}

func newTestKeyringStore(t *testing.T) (*KeyringStore, *fakeKeyring) {
	t.Helper()
	ring := newFakeKeyring()
	return NewKeyringStore(ring, filepath.Join(t.TempDir(), "secrets-index.json")), ring
}

// The round trip every other test depends on, and the property that matters most: the VALUE
// goes to the keyring and never to a file on disk.
func TestKeyringStoreRoundTrip(t *testing.T) {
	s, ring := newTestKeyringStore(t)

	if err := s.Set("github", NewValue("ghp_secret")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, err := s.Lookup(context.Background(), "github")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Reveal() != "ghp_secret" {
		t.Errorf("Lookup = %q", got.Reveal())
	}
	if ring.values["github"] != "ghp_secret" {
		t.Errorf("the value did not reach the keyring: %v", ring.values)
	}

	// The index must hold the NAME and nothing else. A secret that leaked into the index
	// would be a plaintext credential on disk, which is the whole thing this store exists
	// to avoid.
	data, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatalf("reading the index: %v", err)
	}
	if !strings.Contains(string(data), "github") {
		t.Errorf("the index does not list the name: %s", data)
	}
	if strings.Contains(string(data), "ghp_secret") {
		t.Fatalf("THE SECRET IS IN THE INDEX FILE: %s", data)
	}
}

// A missing secret must say "not found" and not "the store is broken": the caller's response
// differs, and `boks run` decides whether to fail or continue on exactly this distinction.
func TestKeyringStoreMissingIsNotFound(t *testing.T) {
	s, _ := newTestKeyringStore(t)
	if _, err := s.Lookup(context.Background(), "absent"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Lookup of an absent secret = %v, want ErrNotFound", err)
	}
}

// The write order, asserted because it decides what an interruption leaves behind: the keyring
// is written first, so a crash leaves a value the index does not list — recoverable — rather
// than a name with no value, which would make `boks run` fail at injection on a credential the
// store claims to have.
func TestKeyringStoreWritesTheValueBeforeTheIndex(t *testing.T) {
	s, ring := newTestKeyringStore(t)
	ring.failSet = errors.New("the OS store refused")

	if err := s.Set("github", NewValue("x")); err == nil {
		t.Fatal("Set succeeded although the keyring refused")
	}
	// Nothing was recorded, because nothing was stored.
	names, err := s.Names()
	if err != nil {
		t.Fatalf("Names: %v", err)
	}
	if len(names) != 0 {
		t.Errorf("the index lists %v after a failed keyring write", names)
	}
}

// An OAuth record survives the trip as a record, and is refused by the plain Lookup — the same
// contract FileStore keeps, so that a JSON blob full of tokens can never be attached to a
// request as an Authorization header.
func TestKeyringStoreOAuth(t *testing.T) {
	s, _ := newTestKeyringStore(t)
	rec := OAuthRecord{
		V:            OAuthRecordVersion,
		Service:      "claude-code",
		AccessToken:  "access-token",
		RefreshToken: "refresh-token",
	}
	if err := s.SetOAuth("claude-code", rec); err != nil {
		t.Fatalf("SetOAuth: %v", err)
	}

	got, err := s.LookupOAuthRecord(context.Background(), "claude-code")
	if err != nil {
		t.Fatalf("LookupOAuthRecord: %v", err)
	}
	if got.AccessToken != "access-token" || got.RefreshToken != "refresh-token" {
		t.Errorf("record did not round trip: %+v", got)
	}
	if _, err := s.Lookup(context.Background(), "claude-code"); err == nil {
		t.Error("a plain Lookup returned an oauth credential; it would be sent as a header")
	}

	// The listing has to distinguish the two kinds, since one can be injected and the
	// other cannot.
	entries, err := s.Entries()
	if err != nil {
		t.Fatalf("Entries: %v", err)
	}
	if len(entries) != 1 || !entries[0].OAuth {
		t.Errorf("Entries = %+v, want one oauth entry", entries)
	}
}

// Delete removes both halves, and deleting something absent is not an error — a delete after
// a partial write has to be able to finish the job.
func TestKeyringStoreDelete(t *testing.T) {
	s, ring := newTestKeyringStore(t)
	if err := s.Set("github", NewValue("x")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("github"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, ok := ring.values["github"]; ok {
		t.Error("the value survived the delete")
	}
	names, _ := s.Names()
	if slices.Contains(names, "github") {
		t.Errorf("the index still lists the deleted name: %v", names)
	}
	if err := s.Delete("never-existed"); err != nil {
		t.Errorf("deleting an absent secret failed: %v", err)
	}
}

// Superseded by TestKeyringStoreReportsIndexDrift. This asserted that a name the keyring had
// lost was skipped SILENTLY, which was the behaviour that let a backend storing nothing look
// like an empty store. The half of it worth keeping — that the readable entries still come
// back — is asserted there.

// Names that cannot survive the OS stores are refused at the door, so the failure names the
// problem rather than surfacing as a keyring error three layers down.
func TestKeyringStoreRejectsImpossibleNames(t *testing.T) {
	s, _ := newTestKeyringStore(t)
	for _, name := range []string{"", "with\nnewline", "with\x00nul", strings.Repeat("x", 256)} {
		if err := s.Set(name, NewValue("x")); err == nil {
			t.Errorf("Set accepted the name %q", name)
		}
	}
}

// silentKeyring accepts every write and stores nothing — the exact shape of a backend whose
// external tool exits 0 without doing the work. It is not hypothetical: the macOS backend
// drives `security -i` and depends on that mode executing the command it is fed, which is the
// one assumption in this package that could not be verified without a Mac.
type silentKeyring struct{ fakeKeyring }

func (s *silentKeyring) Set(context.Context, string, string) error { return nil }

// A write that did not happen must fail LOUDLY, and must record nothing.
//
// Without this, the store reports success, the index records the name, and the credential is
// gone — discovered when `boks secret ls` does not list what was just stored, or worse when a
// sandbox cannot authenticate. That is how it was reported.
func TestKeyringStoreRefusesAWriteThatDidNotHappen(t *testing.T) {
	ring := &silentKeyring{*newFakeKeyring()}
	s := NewKeyringStore(ring, filepath.Join(t.TempDir(), "index.json"))

	err := s.Set("github", NewValue("ghp_secret"))
	if err == nil {
		t.Fatal("a write that stored nothing was reported as success")
	}
	if !strings.Contains(err.Error(), "NOT") {
		t.Errorf("error %q does not say the credential was not stored", err)
	}
	// Nothing recorded: a name in the index with no value behind it is the state that
	// makes a listing lie.
	names, nerr := s.Names()
	if nerr != nil {
		t.Fatalf("Names: %v", nerr)
	}
	if len(names) != 0 {
		t.Errorf("the index recorded %v after a write that did not happen", names)
	}
}

// wrongKeyring stores something other than what it was given — a mangled encoding, a truncated
// value. The store must not accept that either.
type wrongKeyring struct{ fakeKeyring }

func (w *wrongKeyring) Set(ctx context.Context, name, value string) error {
	return w.fakeKeyring.Set(ctx, name, value+"-mangled")
}

func TestKeyringStoreRefusesAValueThatChanged(t *testing.T) {
	ring := &wrongKeyring{*newFakeKeyring()}
	s := NewKeyringStore(ring, filepath.Join(t.TempDir(), "index.json"))

	err := s.Set("github", NewValue("ghp_secret"))
	if err == nil {
		t.Fatal("a value that did not round trip was accepted")
	}
	// Neither value may appear in the message: one of them is the secret.
	if strings.Contains(err.Error(), "ghp_secret") {
		t.Errorf("the error leaks the secret: %v", err)
	}
}

// A name the index lists that the keyring cannot return is REPORTED, not hidden — while the
// entries that are readable are still returned, because a partial answer beats none.
func TestKeyringStoreReportsIndexDrift(t *testing.T) {
	s, ring := newTestKeyringStore(t)
	for _, n := range []string{"kept", "lost"} {
		if err := s.Set(n, NewValue("x")); err != nil {
			t.Fatal(err)
		}
	}
	delete(ring.values, "lost")

	entries, err := s.Entries()
	if !errors.Is(err, ErrIndexDrift) {
		t.Errorf("Entries error = %v, want it to report the drift", err)
	}
	if err != nil && !strings.Contains(err.Error(), "lost") {
		t.Errorf("error %q does not name the missing credential", err)
	}
	// The readable one still comes back.
	if len(entries) != 1 || entries[0].Name != "kept" {
		t.Errorf("Entries = %+v, want the readable entry despite the drift", entries)
	}
}
