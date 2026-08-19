package kit

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// FetchTimeout bounds a git fetch. A kit is read before a sandbox is created, so a repository
// that hangs would hang `boks run` itself with nothing on screen explaining why.
const FetchTimeout = 2 * time.Minute

// gitRef is the #ref= and #dir= pair a git reference may carry.
type gitRef struct {
	// URL is the repository, with the git+ prefix removed and the fragment stripped.
	URL string
	// Ref is a branch, tag or commit. Empty means the repository's default branch.
	Ref string
	// Dir is the subdirectory holding spec.yaml. Empty means the repository root.
	Dir string
}

// fullSHA matches the immutable form of a ref.
var fullSHA = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

// Immutable reports whether the ref names one exact commit and so cannot change under the
// user between two runs.
func (g gitRef) Immutable() bool { return fullSHA.MatchString(g.Ref) }

// parseGitRef splits a git+ reference into its parts.
//
// The grammar is Docker's: `git+https://host/org/repo.git#ref=<branch|tag|commit>&dir=<path>`,
// with `git+ssh://` as the other scheme and both fragment keys optional and URL-encoded.
func parseGitRef(reference string) (gitRef, error) {
	trimmed := strings.TrimPrefix(reference, "git+")

	// A URL that begins with a dash would be handed to git as a flag rather than as a
	// repository. git itself is called with `--` before positional arguments below, but
	// refusing the shape outright is the cheaper guarantee and costs nothing real: no
	// repository URL starts with a dash.
	if strings.HasPrefix(trimmed, "-") {
		return gitRef{}, fmt.Errorf("kit %q: a repository URL cannot begin with '-'", reference)
	}

	base, fragment, _ := strings.Cut(trimmed, "#")
	if base == "" {
		return gitRef{}, fmt.Errorf("kit %q names no repository", reference)
	}

	g := gitRef{URL: base}
	if fragment == "" {
		return g, nil
	}
	// url.ParseQuery rather than splitting by hand, because the fragment is documented as
	// URL-encoded and a `dir` containing an encoded slash is legitimate.
	values, err := url.ParseQuery(fragment)
	if err != nil {
		return gitRef{}, fmt.Errorf("kit %q: cannot read the #fragment: %w", reference, err)
	}
	for key := range values {
		if key != "ref" && key != "dir" {
			return gitRef{}, fmt.Errorf("kit %q: unknown fragment key %q (the grammar is "+
				"#ref=<branch|tag|commit>&dir=<path>)", reference, key)
		}
	}
	g.Ref, g.Dir = values.Get("ref"), values.Get("dir")

	// A dir that escapes the clone is refused. The clone is a temporary directory this
	// process created, and a reference of `dir=../../etc` would read a file outside it —
	// which is not a kit and is not something a reference should be able to ask for.
	if g.Dir != "" {
		// Refused rather than cleaned. filepath.Clean would turn `../../etc` into `etc`,
		// which is contained but is not what was asked for — and silently reading a
		// different directory than the reference names is a worse answer than an error.
		// An absolute dir is refused for the same reason: the clone is the root here.
		if strings.HasPrefix(g.Dir, "/") {
			return gitRef{}, fmt.Errorf("kit %q: dir=%q must be relative to the repository",
				reference, g.Dir)
		}
		// `path`, not `filepath`. This names a directory INSIDE A GIT REPOSITORY, where
		// the separator is always "/" whatever host is doing the reading. Cleaning it
		// with host semantics made `dir=vale` come out as `\vale` on Windows — caught by
		// the Windows CI job, and the reason this distinction is spelled out rather than
		// left to whoever edits next. It becomes a host path once, at the join below.
		//
		// A backslash is checked for too: it is not a separator here, so a `..\..` that
		// git would read as one odd directory name should still not slip past the guard
		// on a host where it might later be treated as a path.
		for _, part := range strings.FieldsFunc(g.Dir, func(r rune) bool { return r == '/' || r == '\\' }) {
			if part == ".." {
				return gitRef{}, fmt.Errorf("kit %q: dir=%q leaves the repository",
					reference, g.Dir)
			}
		}
		g.Dir = strings.TrimPrefix(path.Clean("/"+g.Dir), "/")
		if g.Dir == "" {
			return gitRef{}, fmt.Errorf("kit %q: dir=%q names no directory", reference, values.Get("dir"))
		}
	}
	return g, nil
}

// fetchGit clones the repository into dest and returns the commit it checked out.
//
// Shallow, and only the ref asked for: a kit is a spec.yaml and possibly a files/ tree, so
// there is no reason to pay for a repository's history.
//
// git is invoked rather than a Go implementation because it is the only thing that already
// knows how to reach a repository the way the user's machine does — an SSH agent, a credential
// helper, .netrc, a corporate proxy, a mirror in ~/.gitconfig. A kit fetched over git+ssh from
// a private host works because git handles it, and reimplementing that is how a feature ends up
// working only for public HTTPS.
func fetchGit(ctx context.Context, g gitRef, dest string) (string, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", fmt.Errorf("git is needed to fetch a kit from %s and is not on PATH", g.URL)
	}

	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dest
		// Never prompt. Without this a private repository with no usable credential
		// stops `boks run` on a password prompt the user cannot see the reason for.
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		return cmd.CombinedOutput()
	}

	steps := [][]string{
		{"init", "--quiet"},
		{"remote", "add", "origin", "--", g.URL},
	}
	// An explicit ref is fetched by name, which works for a branch, a tag and — on any
	// server that allows fetching one, which GitHub and GitLab both do — a commit SHA.
	// With no ref, a plain fetch of the default branch is what the product docs describe.
	if g.Ref != "" {
		steps = append(steps, []string{"fetch", "--depth", "1", "--quiet", "origin", "--", g.Ref})
	} else {
		steps = append(steps, []string{"fetch", "--depth", "1", "--quiet", "origin", "HEAD"})
	}
	steps = append(steps, []string{"checkout", "--quiet", "FETCH_HEAD"})

	for _, args := range steps {
		if out, err := run(args...); err != nil {
			if ctx.Err() != nil {
				return "", fmt.Errorf("fetching %s timed out after %s", g.URL, FetchTimeout)
			}
			// git's own message, trimmed: it names the real problem (no such ref,
			// authentication failed, host unreachable) far better than a rewrite.
			detail := strings.TrimSpace(string(out))
			what := g.URL
			if g.Ref != "" {
				what += " at " + g.Ref
			}
			return "", fmt.Errorf("fetching %s: %w\n%s", what, err, detail)
		}
	}

	out, err := run("rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("reading the commit fetched from %s: %w", g.URL, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// loadGit fetches a kit from a git reference and reads its spec.
//
// The commit is reported in the warnings whatever the ref was, because that is the fact a
// reader needs later: "which kit did this sandbox actually run" has one answer, and a branch
// name is not it.
func loadGit(reference string) (*Spec, []string, error) {
	g, err := parseGitRef(reference)
	if err != nil {
		return nil, nil, err
	}

	dest, err := os.MkdirTemp("", "boks-kit-")
	if err != nil {
		return nil, nil, err
	}
	// The clone is read and dropped. Only spec.yaml is used today; when a slice needs the
	// files/ tree it will need to keep this, and that is the moment to decide where a
	// fetched kit lives rather than now.
	defer os.RemoveAll(dest)

	ctx, cancel := context.WithTimeout(context.Background(), FetchTimeout)
	defer cancel()

	commit, err := fetchGit(ctx, g, dest)
	if err != nil {
		return nil, nil, err
	}

	// FromSlash: g.Dir is a repository path and dest is a host path, so this is the one
	// place the two meet.
	specPath := filepath.Join(dest, filepath.FromSlash(g.Dir), SpecFileName)
	data, err := os.ReadFile(specPath)
	if err != nil {
		where := "the repository root"
		if g.Dir != "" {
			where = g.Dir
		}
		return nil, nil, fmt.Errorf("%s has no %s in %s (at commit %s)",
			g.URL, SpecFileName, where, commit)
	}

	spec, warnings, err := ParseSpec(data)
	if err != nil {
		return nil, nil, fmt.Errorf("%s#%s: %w", g.URL, commit, err)
	}

	warnings = append(warnings, fmt.Sprintf("fetched from %s at commit %s", g.URL, commit))
	if !g.Immutable() {
		// Reported, not refused. Docker's normative specification requires an immutable
		// ref and its product documentation says the opposite — `#ref=<branch|tag|commit>`,
		// "Defaults to the repository's default branch", with a tag in its own example —
		// and real kits are published and referenced by tag. Refusing what Docker's users
		// actually write would make "a kit written for sbx is a kit Boks reads" false.
		//
		// So the mutable form works and says what it resolved to. That keeps the property
		// the pinning rule exists for — knowing exactly what ran — without refusing the
		// reference. Someone who wants the guarantee up front writes the SHA, and gets no
		// warning.
		named := g.Ref
		if named == "" {
			named = "the default branch"
		}
		warnings = append(warnings, fmt.Sprintf(
			"%s is not a fixed commit, so this kit may differ on the next run; "+
				"use #ref=%s to pin it", named, commit))
	}
	return spec, warnings, nil
}
