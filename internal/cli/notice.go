package cli

// What a sandbox has already been told, and therefore what this run has to say.
//
// # The problem this solves
//
// Every `boks run` used to print the resolved policy table, the TLS-interception
// instructions, a description of the network architecture and a citation of the run that
// verified it — about fifty lines, before any of the user's own output. The first time that
// is genuinely educational. By the tenth it is something to grep away, and a security notice
// people have learned to skip is worse than no notice at all: the habit is what carries over
// to the notice that mattered.
//
// # The rule
//
// Say a thing loudly the first time it is true of a sandbox, and briefly afterwards. "The
// first time" is per sandbox rather than per run, because a sandbox is the unit whose
// containment the text describes. What counts as a thing:
//
//   - the resolved policy, remembered as a digest of exactly the text that was shown, so
//     that a policy which *changed* is shown again — that is the case a user must not miss,
//     and a rule added to the global store changes it;
//   - the interception set, remembered as the hosts named. A host that has never been shown
//     is announced whether or not others have been, and whether or not --quiet was passed:
//     "boks will decrypt your traffic to this host" is not something a person may meet
//     silently, and a flag asking for less output is not consent to that;
//   - the enforcement note, which describes the mechanism rather than this sandbox's
//     configuration. It is the same paragraph every time; once is enough.
//
// The memory is a small JSON file per sandbox under the state directory. It is a display
// convenience and nothing else: losing it costs one extra printing of text the user has
// already seen, which is why nothing here fails a run when it cannot be read or written.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// noticeVersion is the record's schema version. An unrecognised one is treated as "nothing
// has been shown", which errs towards saying too much rather than too little.
const noticeVersion = 1

// notices is what one sandbox has already been shown.
type notices struct {
	V int `json:"v"`
	// Policy is a digest of the policy text last printed for this sandbox.
	Policy string `json:"policy,omitempty"`
	// Intercept is every host whose interception has been announced, ever. It is a union
	// rather than the last set, so that removing a credential rule and adding it back does
	// not re-announce a host, while adding a *new* one always does.
	Intercept []string `json:"intercept,omitempty"`
	// Enforcement records that the note about how enforcement works has been shown.
	Enforcement bool `json:"enforcement,omitempty"`
}

// noticePath is where a sandbox's record lives. The name is a validated container
// identifier by the time it reaches here; Base is belt and braces, since this value ends up
// in a path.
func noticePath(stateDir, sandbox string) string {
	name := filepath.Base(strings.TrimSpace(sandbox))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return ""
	}
	return filepath.Join(stateDir, "notices", name+".json")
}

// loadNotices reads what a sandbox has been shown. Anything unreadable, unparseable or from
// a future version reads as an empty record: the failure mode is repeating a notice, never
// swallowing one.
func loadNotices(stateDir, sandbox string) notices {
	path := noticePath(stateDir, sandbox)
	if path == "" {
		return notices{}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return notices{}
	}
	var n notices
	if err := json.Unmarshal(raw, &n); err != nil || n.V != noticeVersion {
		return notices{}
	}
	return n
}

// save writes the record, and reports nothing. A run must not fail because Boks could not
// remember that it had already printed something.
func (n notices) save(stateDir, sandbox string) {
	path := noticePath(stateDir, sandbox)
	if path == "" {
		return
	}
	n.V = noticeVersion
	raw, err := json.Marshal(n)
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// Owner-only: the record names the hosts a sandbox decrypts traffic to, which is the
	// same kind of information as the decision log, kept the same way.
	_ = os.WriteFile(path, raw, 0o600)
}

// newHosts returns the interception hosts this sandbox has never been told about.
func (n notices) newHosts(hosts []string) []string {
	var out []string
	for _, h := range hosts {
		if !slices.Contains(n.Intercept, h) {
			out = append(out, h)
		}
	}
	return out
}

// withHosts returns the record with these hosts remembered as announced.
func (n notices) withHosts(hosts []string) notices {
	merged := append([]string(nil), n.Intercept...)
	for _, h := range hosts {
		if !slices.Contains(merged, h) {
			merged = append(merged, h)
		}
	}
	slices.Sort(merged)
	n.Intercept = merged
	return n
}

// digest fingerprints the exact text that was shown, rather than the policy structure.
//
// The text is what the user read, so the text is what "has this changed since you were last
// shown it?" has to be asked about. A digest rather than the text itself keeps the record
// small and makes it obvious that nothing here is meant to be read back out.
func digest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:12])
}
