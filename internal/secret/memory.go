package secret

import (
	"context"
	"fmt"
	"sync"
)

// MemoryStore holds one sandbox's credentials for the life of one process.
//
// It exists because the network supervisor deliberately never learns the passphrase to the
// encrypted store: the CLI resolves the values it needs and hands them over on a pipe (see
// internal/enforce). That is a property worth keeping — a long-lived process that could
// decrypt every credential you own would be a much worse thing to have running — and it has
// a consequence for OAuth that is stated here rather than discovered later.
//
// **A rotation inside a sandbox is not durable.** When the proxy refreshes an OAuth
// credential, the new pair is kept here, used for the rest of the sandbox's life, and lost
// when the sandbox stops. For a provider that rotates refresh tokens on every exchange — most
// of them, including Anthropic — the pair left in the encrypted store is then stale, and the
// next sandbox that uses it will fail its first refresh and have to be re-imported. OnRotate
// exists so the caller can say so where the user is looking, at the moment it happens.
//
// The fix is not to give this process the passphrase. It is either a writeback channel to the
// process that has it, or refreshing in the CLI before the supervisor is spawned; neither is
// built.
type MemoryStore struct {
	mu     sync.Mutex
	values map[string]string
	oauth  map[string]OAuthRecord

	// OnRotate is called after a refresh that could not be persisted. It receives the
	// service name and never a value.
	OnRotate func(service string)
}

// NewMemoryStore builds a store from resolved values and OAuth records.
func NewMemoryStore(values map[string]string, oauth map[string]OAuthRecord) *MemoryStore {
	s := &MemoryStore{values: map[string]string{}, oauth: map[string]OAuthRecord{}}
	for k, v := range values {
		s.values[k] = v
	}
	for k, v := range oauth {
		s.oauth[k] = v
	}
	return s
}

// Lookup implements Provider.
func (s *MemoryStore) Lookup(_ context.Context, name string) (Value, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[name]
	if !ok {
		return Value{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return NewValue(v), nil
}

// LookupOAuth implements OAuthProvider.
func (s *MemoryStore) LookupOAuth(_ context.Context, name string) (OAuthTokens, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.oauth[name]
	if !ok {
		return OAuthTokens{}, fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	return r.Tokens(), nil
}

// SaveOAuth implements OAuthSaver, for this process only.
func (s *MemoryStore) SaveOAuth(_ context.Context, name string, tokens OAuthTokens) error {
	s.mu.Lock()
	r, ok := s.oauth[name]
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("%w: %q", ErrNotFound, name)
	}
	s.oauth[name] = r.WithTokens(tokens)
	notify := s.OnRotate
	s.mu.Unlock()

	if notify != nil {
		notify(name)
	}
	return nil
}

// Records returns the OAuth records currently held, for a caller that wants to hand them on.
// The values are live tokens, so this is host-side only and nothing prints its result.
func (s *MemoryStore) Records() map[string]OAuthRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]OAuthRecord, len(s.oauth))
	for k, v := range s.oauth {
		out[k] = v
	}
	return out
}
