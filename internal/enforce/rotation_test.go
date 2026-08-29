package enforce

import (
	"context"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/secret"
)

// savedTokens records what a rotation handed to the store, so the test can assert the NEW
// pair was persisted rather than the one the sandbox started with.
type savedTokens struct {
	service string
	tokens  secret.OAuthTokens
	err     error
	calls   int
}

func (s *savedTokens) SaveOAuth(_ context.Context, service string, tokens secret.OAuthTokens) error {
	s.calls++
	s.service, s.tokens = service, tokens
	return s.err
}

// rotationFixture builds a sandbox store holding one OAuth credential, and the engine whose
// log the handler writes to.
func rotationFixture(t *testing.T) (Spec, *secret.MemoryStore, *policy.Engine) {
	t.Helper()
	spec := Spec{
		Sandbox:  "s",
		StateDir: t.TempDir(),
		OAuth: map[string]secret.OAuthRecord{
			"claude-code": {
				Service:      "claude-code",
				TokenHost:    "platform.claude.test",
				AccessToken:  "old-access",
				RefreshToken: "old-refresh",
			},
		},
	}
	store := secret.NewMemoryStore(nil, spec.OAuth)
	pol := policy.Policy{Name: "test", Default: policy.Deny}
	return spec, store, policy.NewEngine(pol, policy.NewLog(policy.DefaultCapacity))
}

func reasons(e *policy.Engine) string {
	var b strings.Builder
	for _, d := range e.Log().Recent(0) {
		b.WriteString(d.Reason)
		b.WriteString("\n")
	}
	return b.String()
}

// The point of the whole file: a refresh inside a sandbox must reach the host's store.
//
// Providers that rotate retire the old refresh token at the moment of exchange, so a rotation
// that is not persisted does not merely fail to help the next sandbox — it breaks it. Measured
// on 2026-08-29, where an hour after one sandbox refreshed, a new one could not reach
// api.anthropic.com at all and every policy rule was allowing.
func TestARotationIsWrittenBackToTheStore(t *testing.T) {
	spec, store, engine := rotationFixture(t)
	saver := &savedTokens{}
	store.OnRotate = rotationHandler(spec, store, saver, engine, func(string, ...any) {})

	fresh := secret.OAuthTokens{Access: secret.NewValue("new-access"), Refresh: secret.NewValue("new-refresh")}
	if err := store.SaveOAuth(context.Background(), "claude-code", fresh); err != nil {
		t.Fatalf("SaveOAuth: %v", err)
	}

	if saver.calls != 1 {
		t.Fatalf("the store was written %d times, want 1", saver.calls)
	}
	if saver.service != "claude-code" {
		t.Errorf("wrote credential %q", saver.service)
	}
	// The NEW pair, not the one the sandbox was handed: persisting the old one would store
	// a token the provider has already retired.
	if got := saver.tokens.Refresh.Reveal(); got != "new-refresh" {
		t.Errorf("persisted refresh token = %q, want the rotated one", got)
	}
	if got := saver.tokens.Access.Reveal(); got != "new-access" {
		t.Errorf("persisted access token = %q, want the rotated one", got)
	}
	if r := reasons(engine); !strings.Contains(r, "stored copy updated") {
		t.Errorf("the decision log does not record that the rotation was kept:\n%s", r)
	}
}

// A store that refuses must be as visible as one that is absent. A credential the user
// believes is stored and is not is the exact failure this code exists to prevent.
func TestAFailedWritebackSaysSoAndSaysWhatToDo(t *testing.T) {
	spec, store, engine := rotationFixture(t)
	saver := &savedTokens{err: context.DeadlineExceeded}
	var logged strings.Builder
	store.OnRotate = rotationHandler(spec, store, saver, engine,
		func(f string, a ...any) { logged.WriteString(f) })

	fresh := secret.OAuthTokens{Access: secret.NewValue("new-access"), Refresh: secret.NewValue("new-refresh")}
	if err := store.SaveOAuth(context.Background(), "claude-code", fresh); err != nil {
		t.Fatalf("SaveOAuth: %v", err)
	}

	r := reasons(engine)
	if !strings.Contains(r, "could not be stored") {
		t.Errorf("a failed writeback was not recorded:\n%s", r)
	}
	if !strings.Contains(r, "boks secret") {
		t.Errorf("the note does not say how to recover:\n%s", r)
	}
	if logged.Len() == 0 {
		t.Error("nothing reached the supervisor's log")
	}
}

// A host with no keyring keeps the older behaviour, and the note keeps telling the truth
// about it rather than claiming a rotation was kept.
func TestNoKeyringKeepsTheOldWarning(t *testing.T) {
	spec, store, engine := rotationFixture(t)
	store.OnRotate = rotationHandler(spec, store, nil, engine, func(string, ...any) {})

	fresh := secret.OAuthTokens{Access: secret.NewValue("new-access"), Refresh: secret.NewValue("new-refresh")}
	if err := store.SaveOAuth(context.Background(), "claude-code", fresh); err != nil {
		t.Fatalf("SaveOAuth: %v", err)
	}

	r := reasons(engine)
	if !strings.Contains(r, "no keyring") || !strings.Contains(r, "stale") {
		t.Errorf("the note does not describe an unpersisted rotation:\n%s", r)
	}
	if strings.Contains(r, "stored copy updated") {
		t.Error("the log claims the rotation was kept when there was nowhere to keep it")
	}
}

// Whatever else changes, a token must never reach a log.
func TestRotationNotesCarryNoTokens(t *testing.T) {
	spec, store, engine := rotationFixture(t)
	saver := &savedTokens{err: context.DeadlineExceeded}
	store.OnRotate = rotationHandler(spec, store, saver, engine, func(string, ...any) {})

	fresh := secret.OAuthTokens{Access: secret.NewValue("new-access"), Refresh: secret.NewValue("new-refresh")}
	_ = store.SaveOAuth(context.Background(), "claude-code", fresh)

	r := reasons(engine)
	for _, canary := range []string{"new-access", "new-refresh", "old-access", "old-refresh"} {
		if strings.Contains(r, canary) {
			t.Errorf("the decision log carries %q:\n%s", canary, r)
		}
	}
}
