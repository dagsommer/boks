package secret

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

// A custom secret is a credential that does not belong to a named service: it is keyed on the
// hosts it may be sent to and the environment variable the guest reads it from, and the guest
// holds a generated placeholder (sbx-cs-<random>) that the proxy replaces with the real value
// wherever it appears in a request header on those hosts. This is `sbx secret set-custom`.
//
// It is stored as one value in the ordinary store, under a name derived from the env var and
// the hosts, with a marker the way an OAuth record is — so the stores, the index and the
// per-sandbox scoping need to know nothing about it.

const customMarker = "boks-custom:v1:"

// CustomPrefix starts the stored name of every custom secret.
const CustomPrefix = "custom."

// CustomPlaceholderPrefix is what a generated placeholder starts with, the same as sbx's.
const CustomPlaceholderPrefix = "sbx-cs-"

// Custom is a stored custom secret.
type Custom struct {
	Hosts       []string `json:"hosts"`
	Env         string   `json:"env"`
	Placeholder string   `json:"placeholder"`
	// Exactly one of Value, Ref and Command is set.
	Value   string `json:"value,omitempty"`
	Ref     string `json:"ref,omitempty"`
	Command string `json:"command,omitempty"`
}

// IsCustom reports whether a stored value is a custom secret.
func IsCustom(stored string) bool { return strings.HasPrefix(stored, customMarker) }

// IsCustomName reports whether a (possibly scoped) store name is a custom secret's.
func IsCustomName(stored string) bool {
	_, name := SplitScopedName(stored)
	return strings.HasPrefix(name, CustomPrefix)
}

// CustomName derives the store name from the env var and the hosts, so setting the same
// secret again replaces it — and keeps its placeholder — instead of adding a second one.
func CustomName(env string, hosts []string) string {
	h := append([]string(nil), hosts...)
	sort.Strings(h)
	sum := sha256.Sum256([]byte(strings.Join(h, "\n")))
	if env == "" {
		return CustomPrefix + hex.EncodeToString(sum[:4])
	}
	return CustomPrefix + env + "." + hex.EncodeToString(sum[:4])
}

// ValidatePlaceholder refuses a placeholder that could not sit in a header or be matched.
func ValidatePlaceholder(p string) error {
	if p == "" {
		return errors.New("the placeholder is empty")
	}
	for _, r := range p {
		if r <= ' ' || r > '~' || r == '=' {
			return fmt.Errorf("placeholder %q must be printable ASCII without spaces or '='", p)
		}
	}
	return nil
}

// ExpandPlaceholder turns a --placeholder template into a placeholder: "{rand}" becomes a
// random string, so 'github_pat_{rand}' is a placeholder that has the vendor's prefix. A
// template with no {rand} is the placeholder as written. existing is the placeholder of the
// secret being replaced: if it fits the template it is kept, so rotating a token does not
// change the value a running sandbox already holds.
func ExpandPlaceholder(template, existing string) (string, error) {
	prefix, suffix, ok := strings.Cut(template, "{rand}")
	if !ok {
		return template, nil
	}
	if existing != "" && len(existing) > len(prefix)+len(suffix) &&
		strings.HasPrefix(existing, prefix) && strings.HasSuffix(existing, suffix) {
		return existing, nil
	}
	r, err := NewCustomPlaceholder()
	if err != nil {
		return "", err
	}
	return prefix + strings.TrimPrefix(r, CustomPlaceholderPrefix) + suffix, nil
}

// NewCustomPlaceholder generates a placeholder.
func NewCustomPlaceholder() (string, error) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	for i := range b {
		b[i] = alphabet[int(b[i])%len(alphabet)]
	}
	return CustomPlaceholderPrefix + string(b), nil
}

// Validate checks a custom secret before it is stored.
func (c Custom) Validate() error {
	if len(c.Hosts) == 0 {
		return errors.New("at least one --host is required")
	}
	for _, r := range c.Env {
		if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return fmt.Errorf("--env %q is not a valid environment variable name", c.Env)
		}
	}
	if err := ValidatePlaceholder(c.Placeholder); err != nil {
		return err
	}
	n := 0
	for _, s := range []string{c.Value, c.Ref, c.Command} {
		if s != "" {
			n++
		}
	}
	if n != 1 {
		return errors.New("exactly one of a value, --ref and --command is needed")
	}
	return nil
}

// Encode renders the stored form.
func (c Custom) Encode() (string, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	return customMarker + string(b), nil
}

// DecodeCustom reads the stored form.
func DecodeCustom(stored string) (Custom, error) {
	var c Custom
	if !IsCustom(stored) {
		return c, errors.New("not a custom secret")
	}
	if err := json.Unmarshal([]byte(strings.TrimPrefix(stored, customMarker)), &c); err != nil {
		return c, fmt.Errorf("unreadable custom secret: %w", err)
	}
	return c, nil
}

// Resolve returns the real value: stored, read with `op read REF`, or the output of a host
// command run from a fresh temporary directory.
func (c Custom) Resolve(ctx context.Context) (string, error) {
	switch {
	case c.Value != "":
		return c.Value, nil
	case c.Ref != "":
		return runForSecret(ctx, "op", "read", c.Ref)
	case c.Command != "":
		return runForSecret(ctx, shell(), shellFlag(), c.Command)
	}
	return "", errors.New("custom secret holds no value")
}

func runForSecret(ctx context.Context, name string, args ...string) (string, error) {
	dir, err := os.MkdirTemp("", "boks-secret-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		// The command's stderr is deliberately not echoed: it may contain the value.
		return "", fmt.Errorf("resolving a custom secret with %s failed: %w", name, err)
	}
	v := strings.TrimRight(string(out), "\r\n")
	if v == "" {
		return "", fmt.Errorf("%s printed no value", name)
	}
	return v, nil
}
