package secret

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// The round trip a stored name has to survive, including the shapes that could be read back as
// the wrong sandbox.
func TestScopedNameRoundTrip(t *testing.T) {
	for _, tc := range []struct{ sandbox, name string }{
		{"web", "github"},
		{"claude-myrepo", "anthropic"},
		{"a", "b"},
	} {
		stored := ScopedName(tc.sandbox, tc.name)
		sandbox, name := SplitScopedName(stored)
		if sandbox != tc.sandbox || name != tc.name {
			t.Errorf("SplitScopedName(%q) = (%q, %q), want (%q, %q)",
				stored, sandbox, name, tc.sandbox, tc.name)
		}
	}
}

// A machine-wide name has no scope and must not acquire one by being read.
func TestUnscopedNamesStayUnscoped(t *testing.T) {
	for _, name := range []string{"github", "anthropic", "sandbox:no-separator"} {
		sandbox, got := SplitScopedName(name)
		if sandbox != "" || got != name {
			t.Errorf("SplitScopedName(%q) = (%q, %q), want (\"\", %q)", name, sandbox, got, name)
		}
	}
}

// The names that would be ambiguous once stored are refused when they are set, not discovered
// when they are read back as something else.
func TestValidateScopedName(t *testing.T) {
	for _, tc := range []struct {
		sandbox, name string
		wantErr       bool
	}{
		{"", "github", false},
		{"web", "github", false},
		{"", "", true},
		{"", "github/extra", true},
		{"", "sandbox:web/github", true},
		{"web/other", "github", true},
	} {
		err := ValidateScopedName(tc.sandbox, tc.name)
		if (err != nil) != tc.wantErr {
			t.Errorf("ValidateScopedName(%q, %q) = %v, wantErr %v",
				tc.sandbox, tc.name, err, tc.wantErr)
		}
	}
}

// The resolution order, which is the feature: a sandbox's own credential wins, and the
// machine's serves everyone else.
func TestForSandboxPrefersTheSandboxsOwn(t *testing.T) {
	p := MapProvider{
		"github":                      "machine-wide",
		ScopedName("web", "github"):   "web-only",
		ScopedName("other", "github"): "other-only",
	}
	ctx := context.Background()

	web, err := ForSandbox(p, "web").Lookup(ctx, "github")
	if err != nil {
		t.Fatalf("web lookup: %v", err)
	}
	if web.Reveal() != "web-only" {
		t.Errorf("sandbox web got %q, want its own credential", web.Reveal())
	}

	// A sandbox with no credential of its own falls back, which is what keeps a
	// machine-wide secret working for everything that has not overridden it.
	none, err := ForSandbox(p, "unconfigured").Lookup(ctx, "github")
	if err != nil {
		t.Fatalf("fallback lookup: %v", err)
	}
	if none.Reveal() != "machine-wide" {
		t.Errorf("an unconfigured sandbox got %q, want the machine-wide credential", none.Reveal())
	}

	// And one sandbox's credential is not another's.
	other, _ := ForSandbox(p, "other").Lookup(ctx, "github")
	if other.Reveal() != "other-only" {
		t.Errorf("sandbox other got %q", other.Reveal())
	}
}

// No sandbox means no wrapper, so a caller that does not know one behaves exactly as before.
func TestForSandboxWithoutASandboxIsTheProviderItself(t *testing.T) {
	p := MapProvider{"github": "machine-wide"}
	if got := ForSandbox(p, ""); got == nil {
		t.Fatal("ForSandbox returned nil")
	}
	v, err := ForSandbox(p, "").Lookup(context.Background(), "github")
	if err != nil || v.Reveal() != "machine-wide" {
		t.Errorf("Lookup = (%q, %v)", v.Reveal(), err)
	}
}

// failingProvider returns something that is NOT ErrNotFound, to prove the fallback does not
// swallow a real failure.
type failingProvider struct{ err error }

func (f failingProvider) Lookup(context.Context, string) (Value, error) { return Value{}, f.err }

// A store that cannot be opened must not read as "no such credential". Falling back on any
// error would tell a user with a locked keychain that the secret they stored does not exist,
// and send them to store it again.
func TestForSandboxDoesNotFallBackOnRealFailures(t *testing.T) {
	boom := errors.New("the keyring is locked")
	_, err := ForSandbox(failingProvider{err: boom}, "web").Lookup(context.Background(), "github")
	if !errors.Is(err, boom) {
		t.Errorf("Lookup error = %v, want the underlying failure", err)
	}
	if strings.Contains(err.Error(), "not found") {
		t.Errorf("a real failure was reported as absence: %v", err)
	}
}
