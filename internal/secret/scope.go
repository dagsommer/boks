package secret

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Secrets can be stored for one sandbox rather than for the machine, and this file is how a
// name carries that.
//
// # Why scoping is in the NAME and not in a second store
//
// Because everything else about a secret store — the keyring, the index, the encrypted file,
// the OAuth encoding — works on a name, and giving per-sandbox secrets their own store would
// mean two of each, kept in step. A scoped name is one store with a naming convention, which
// is the same trick internal/policy plays: a rule scoped to a sandbox is a rule with a scope
// label, not a second policy engine.
//
// # The resolution order, which is the whole feature
//
// A sandbox's own credential wins over the machine's. `boks secret set github --sandbox web`
// means "when the sandbox called web asks for github, give it this one" and must not disturb
// what every other sandbox gets. Falling back the other way — machine first — would make the
// per-sandbox secret unreachable whenever a global one existed, which is exactly when someone
// sets one.
//
// It falls back rather than requiring one, so a machine-wide credential still serves every
// sandbox that has no override. That is the common case and it must stay the case with no
// flags.

// scopePrefix marks a scoped name. The separator is one a service name cannot contain — see
// ValidateScopedName — so that a scoped and an unscoped name can never collide.
const scopePrefix = "sandbox:"

const scopeSeparator = "/"

// ScopedName is the storage name for a credential that belongs to one sandbox.
func ScopedName(sandbox, name string) string {
	return scopePrefix + sandbox + scopeSeparator + name
}

// SplitScopedName reports the sandbox and service in a stored name. The sandbox is empty for a
// machine-wide credential, which is what an unprefixed name is.
func SplitScopedName(stored string) (sandbox, name string) {
	if !strings.HasPrefix(stored, scopePrefix) {
		return "", stored
	}
	rest := strings.TrimPrefix(stored, scopePrefix)
	sandbox, name, found := strings.Cut(rest, scopeSeparator)
	if !found {
		// A name that begins with the prefix and has no separator is not something
		// ScopedName can produce. Treated as machine-wide rather than as a broken scope,
		// so that a store written by hand cannot make the lister fail.
		return "", stored
	}
	return sandbox, name
}

// ValidateScopedName refuses a service name that would be ambiguous once scoped.
//
// A service containing the separator could be read back as a different sandbox and service
// pair, so `github/extra` is refused rather than stored as something SplitScopedName would
// take apart wrongly. The prefix is refused for the same reason: a machine-wide credential
// literally called `sandbox:web/github` would be indistinguishable from a scoped one.
func ValidateScopedName(sandbox, name string) error {
	if name == "" {
		return errors.New("a credential needs a service name")
	}
	if strings.Contains(name, scopeSeparator) {
		return fmt.Errorf("service %q cannot contain %q: it is the separator that marks a "+
			"per-sandbox credential", name, scopeSeparator)
	}
	if strings.HasPrefix(name, scopePrefix) {
		return fmt.Errorf("service %q cannot start with %q: that prefix marks a per-sandbox "+
			"credential", name, scopePrefix)
	}
	if sandbox == "" {
		return nil
	}
	if strings.Contains(sandbox, scopeSeparator) {
		return fmt.Errorf("sandbox %q cannot contain %q", sandbox, scopeSeparator)
	}
	return nil
}

// ForSandbox returns a Provider that answers with a sandbox's own credentials first and the
// machine's otherwise.
//
// The sandbox name being empty returns the underlying provider unchanged, so a caller that
// does not know a sandbox — `boks proxy` outside a run — needs no special case.
func ForSandbox(p Provider, sandbox string) Provider {
	if sandbox == "" {
		return p
	}
	return scopedProvider{inner: p, sandbox: sandbox}
}

type scopedProvider struct {
	inner   Provider
	sandbox string
}

// Lookup tries the sandbox's own credential, then the machine's.
//
// Only ErrNotFound falls through. Any other failure — a keyring that will not open, a store
// that will not decrypt — is returned as it stands, because falling back on those would turn
// "your keychain is locked" into "no such credential" and send the user looking for a secret
// they already stored.
func (s scopedProvider) Lookup(ctx context.Context, name string) (Value, error) {
	v, err := s.inner.Lookup(ctx, ScopedName(s.sandbox, name))
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return Value{}, err
	}
	return s.inner.Lookup(ctx, name)
}
