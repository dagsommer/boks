package kit

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads a kit from a reference and returns its spec.
//
// # What a reference may be, and what it may not be yet
//
// Docker's kits are referenced in five forms: a built-in name, a local directory, a local ZIP,
// a Git URL (`git+https://`, `git+ssh://`) and an OCI artifact. This function implements the
// local directory and a direct path to a spec.yaml, and REFUSES the rest by name.
//
// The refusal is deliberate and is not a stub that was never finished. A kit names an image,
// declares network rules and runs commands as uid 0 (`setup.install`), so a fetched kit is
// remote input with more authority than anything else Boks downloads — and everything else
// Boks downloads is pinned: packaging/nerdbox/NERDBOX_REV pins a commit, the Homebrew formula
// and scripts/build-nerdbox-guest.sh pin tarballs by SHA-256, images resolve by digest.
//
// Docker's own normative specification agrees, and more strictly than its tutorials do: an OCI
// reference MUST be a digest and a Git ref MUST be a full 40-character commit SHA, with tags
// and branches rejected (see docs/kits-design.md §2, which quotes both sides of that disagreement).
// Shipping the fetch before the pinning would ship the one form of this feature that cannot be
// made safe afterwards, because by then people would have unpinned references written down.
//
// So an unsupported form fails with the reason rather than with "no such file or directory",
// which is what a bare os.Stat would have said about `oci://…`.
func Load(reference string) (*Spec, []string, error) {
	if reference == "" {
		return nil, nil, fmt.Errorf("no kit reference given")
	}
	if scheme, ok := remoteForm(reference); ok {
		// "a"/"an" from the word itself: "a OCI reference" is the kind of wrongness that
		// makes an error read as machine output nobody proofread.
		article := "a"
		if strings.ContainsRune("AEIOU", rune(scheme[0])) {
			article = "an"
		}
		return nil, nil, fmt.Errorf("kit %q is %s %s reference, which Boks cannot load yet: "+
			"only a local directory or a path to %s works today. Fetching a kit is not "+
			"implemented rather than not written — a kit sets network rules and runs "+
			"commands as root, so it has to be pinned the way Boks pins everything else "+
			"it downloads. See docs/kits.md for how to use one, and docs/kits-design.md for why",
			reference, article, scheme, SpecFileName)
	}

	path := reference
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading kit %q: %w", reference, err)
	}
	if info.IsDir() {
		path = filepath.Join(path, SpecFileName)
		if _, err := os.Stat(path); err != nil {
			return nil, nil, fmt.Errorf("kit directory %q has no %s", reference, SpecFileName)
		}
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	spec, warnings, err := ParseSpec(data)
	if err != nil {
		// The path, because a kit is usually one of several and the spec's own error
		// names a field rather than a file.
		return nil, nil, fmt.Errorf("%s: %w", path, err)
	}
	return spec, warnings, nil
}

// remoteForm reports whether a reference names something Boks would have to fetch, and what to
// call it in the refusal.
//
// Recognised by prefix rather than by parsing, because the only decision being made is
// "is this local?" — and every form that is not local must be refused with its own name, not
// silently treated as a filename. An OCI reference is the awkward one: it has no scheme in
// Docker's own documentation (`ghcr.io/org/kit:1.0`), so it is recognised by looking like a
// registry reference — a dot or a colon in the first path segment — which is the same shape
// containerd uses to tell a registry host from a local name.
func remoteForm(reference string) (string, bool) {
	for prefix, name := range map[string]string{
		"git+https://": "git",
		"git+ssh://":   "git",
		"git://":       "git",
		"http://":      "HTTP",
		"https://":     "HTTPS",
		"oci://":       "OCI",
	} {
		if strings.HasPrefix(reference, prefix) {
			return name, true
		}
	}
	if strings.HasSuffix(reference, ".zip") {
		return "ZIP archive", true
	}
	// A local path is the common case and must not be mistaken for a registry: anything
	// that starts with a path separator, a dot segment or `file://` is local by shape.
	if strings.HasPrefix(reference, "/") || strings.HasPrefix(reference, "./") ||
		strings.HasPrefix(reference, "../") || strings.HasPrefix(reference, "file://") ||
		filepath.IsAbs(reference) {
		return "", false
	}
	if head, _, _ := strings.Cut(reference, "/"); strings.ContainsAny(head, ".:") {
		// `ghcr.io/org/kit:1.0` — a registry host in the first segment. A bare
		// `my-kit/` has no dot or colon there and stays local.
		if strings.Contains(reference, "/") {
			return "OCI", true
		}
	}
	return "", false
}

// NetworkRules returns the destinations a kit's spec declares under permissions.network.
//
// Returned as two lists rather than one, because the caller puts them in different places: a
// deny only ever narrows and is safe to apply as it stands, while an allow is an addition that
// a deny in any scope still beats. See internal/policy's kit layer.
func NetworkRules(s *Spec) (allow, deny []string) {
	if s == nil || s.Permissions == nil || s.Permissions.Network == nil {
		return nil, nil
	}
	return s.Permissions.Network.Allow, s.Permissions.Network.Deny
}
