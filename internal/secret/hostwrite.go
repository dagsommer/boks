package secret

// Writing a refreshed login back to where the agent keeps it.
//
// A login adopted from the host is one OAuth session with two holders: Boks's copy and the
// agent's own. The provider rotates refresh tokens, so when Boks refreshes, the agent's copy
// is retired and the user is asked to log in again on the host. Reading the host's login first
// (SetHostSource) only helps when the host refreshed first. This is the other half: when Boks
// is the one that refreshes, it puts the new pair where the agent will find it.
//
// Only the three token fields are replaced. The rest of the document — subscription type,
// scopes, anything the agent adds in a later release — is carried through untouched, because
// the document is the agent's, not Boks's.

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// CredentialSink is a CredentialSource that can also be written. Both the Keychain and a
// file can.
type CredentialSink interface {
	CredentialSource
	Write(ctx context.Context, data []byte) error
}

// Write implements CredentialSink: a temporary file beside the target, renamed over it, so a
// reader never sees half a document, with the target's mode kept (0600 when new).
func (s FileSource) Write(_ context.Context, data []byte) error {
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(s.Path); err == nil {
		mode = fi.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.Path), ".boks-credentials-*")
	if err != nil {
		return fmt.Errorf("writing the credential file: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the credential file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return fmt.Errorf("writing the credential file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing the credential file: %w", err)
	}
	if err := os.Rename(name, s.Path); err != nil {
		return fmt.Errorf("writing the credential file: %w", err)
	}
	return nil
}

// Write implements CredentialSink with `security -i`, which reads its commands from standard
// input. The document is a token, and a command-line argument is visible to every process on
// the machine for as long as the command runs; standard input is not. It is passed as hex so
// that no quoting can go wrong.
func (s KeychainSource) Write(ctx context.Context, data []byte) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("%w (this is %s)", ErrNoKeychain, runtime.GOOS)
	}
	if s.Service == "" {
		return errors.New("no keychain service name")
	}
	account := s.Account
	if account == "" {
		account = s.existingAccount(ctx)
	}
	if account == "" {
		if u, err := user.Current(); err == nil {
			account = u.Username
		}
	}
	if account == "" || strings.ContainsAny(account+s.Service, " \t\r\n\"'\\") {
		return errors.New("cannot write the keychain item: its service or account name needs quoting")
	}
	cmd := exec.CommandContext(ctx, "security", "-i")
	cmd.Stdin = strings.NewReader(fmt.Sprintf("add-generic-password -U -s %s -a %s -X %s\n",
		s.Service, account, hex.EncodeToString(data)))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		detail := strings.TrimSpace(out.String())
		if len(detail) > 200 {
			detail = detail[:200]
		}
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("writing %q to the keychain: %s", s.Service, detail)
	}
	// `security -i` reports a failed command on its output, not in its exit status.
	if text := out.String(); strings.Contains(text, "security:") || strings.Contains(strings.ToLower(text), "error") {
		detail := strings.TrimSpace(text)
		if len(detail) > 200 {
			detail = detail[:200]
		}
		return fmt.Errorf("writing %q to the keychain: %s", s.Service, detail)
	}
	return nil
}

// existingAccount is the account name of the item as it stands, so an update lands on that
// item instead of creating a second one beside it.
func (s KeychainSource) existingAccount(ctx context.Context) string {
	out, err := exec.CommandContext(ctx, "security", "find-generic-password", "-s", s.Service).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, `"acct"<blob>="`); ok {
			return strings.TrimSuffix(rest, `"`)
		}
	}
	return ""
}

// mergeClaudeCode replaces the token fields of a Claude Code credential document and keeps
// every other field.
func mergeClaudeCode(raw []byte, t ImportedTokens) ([]byte, error) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		return nil, errors.New("this is not a Claude Code credential document")
	}
	var oauth map[string]json.RawMessage
	if err := json.Unmarshal(doc["claudeAiOauth"], &oauth); err != nil || oauth == nil {
		return nil, errors.New("the credential document has no claudeAiOauth object")
	}
	set := func(key string, v any) error {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		oauth[key] = b
		return nil
	}
	if err := set("accessToken", t.Access); err != nil {
		return nil, err
	}
	if t.Refresh != "" {
		if err := set("refreshToken", t.Refresh); err != nil {
			return nil, err
		}
	}
	if t.ExpiresAt > 0 {
		if err := set("expiresAt", t.ExpiresAt); err != nil {
			return nil, err
		}
	}
	inner, err := json.Marshal(oauth)
	if err != nil {
		return nil, err
	}
	doc["claudeAiOauth"] = inner
	return json.Marshal(doc)
}

// WriteHostLogin puts a refreshed pair where the profile's agent keeps its login, so the
// agent and Boks share one session instead of retiring each other's.
//
// from is the refresh token the new pair was obtained with. The write happens only if the
// host still holds exactly that one: if the agent has moved on since (it refreshed, or the
// user logged in again), the host's login is newer than what Boks has, and overwriting it
// would be the very harm this exists to prevent. It reports whether it wrote.
func WriteHostLogin(ctx context.Context, profileName string, from Value, fresh OAuthTokens) (bool, error) {
	profile, err := Profile(profileName)
	if err != nil || profile.DefaultSource == nil || profile.Parse == nil || profile.Merge == nil {
		return false, ErrNotFound
	}
	sink, ok := profile.DefaultSource().(CredentialSink)
	if !ok {
		return false, ErrNotFound
	}
	return writeHostLogin(ctx, profile, sink, from, fresh)
}

func writeHostLogin(ctx context.Context, profile OAuthProfile, sink CredentialSink, from Value, fresh OAuthTokens) (bool, error) {
	raw, err := sink.Read(ctx)
	if err != nil {
		return false, err
	}
	current, err := profile.Parse(raw)
	if err != nil {
		return false, err
	}
	if from.IsZero() || current.Refresh != from.Reveal() {
		return false, nil
	}
	merged, err := profile.Merge(raw, ImportedTokens{
		Access:    fresh.Access.Reveal(),
		Refresh:   fresh.Refresh.Reveal(),
		ExpiresAt: unixMilli(fresh),
	})
	if err != nil {
		return false, err
	}
	if err := sink.Write(ctx, merged); err != nil {
		return false, err
	}
	return true, nil
}

func unixMilli(t OAuthTokens) int64 {
	if t.Expiry.IsZero() {
		return 0
	}
	return t.Expiry.UnixMilli()
}
