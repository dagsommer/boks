// Package purge inventories and removes the host-side state Boks writes.
//
// It exists because uninstalling Boks did not remove Boks. Measured on Windows on
// 2026-08-16, `winget uninstall` left %LOCALAPPDATA%\boks behind at 59 files and 1,768.8 MB,
// and that is arguably correct — a package manager owns the files it installed, not the ones
// a program later wrote. What was not correct is that no boks command removed them either,
// so the only route to that gigabyte and a half was knowing where it was. The same shape
// holds on Linux and macOS: measured here on 2026-08-16, a single `boks create shell` grew
// ~/.local/state/boks from 300 KiB to 1005 MiB — 219 MiB of compressed image blobs in
// containerd's content store and 785 MiB of layers unpacked by the erofs snapshotter.
//
// The package is deliberately small and deliberately paranoid, because every path through it
// ends in os.RemoveAll. Two properties carry that weight:
//
//   - Nothing is removed by traversal. Removal targets come from a fixed catalogue of names
//     that Boks itself writes, each resolved as a direct child of the state directory. A file
//     this build does not recognise is reported as left alone and can never be deleted, so a
//     mistyped BOKS_STATE_DIR pointed at a directory full of someone's work removes nothing
//     from it.
//   - The root is validated before anything is planned, let alone applied: not empty, not a
//     filesystem root, and not at or above the user's home directory.
//
// Symlinks are not followed. Sizes are measured with lstat semantics, and a catalogue entry
// that happens to be a symlink has the link removed rather than whatever it points at.
package purge

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

// Kind says what removing an entry costs, which is the only question a user has when
// deciding. Size answers "how much do I get back"; this answers "what do I lose".
type Kind int

const (
	// Foreign is the zero value, and means Boks did not write this and will not remove
	// it. It is first so that an entry nobody classified cannot default into a category
	// that gets deleted: an unclassified file is left alone, which is the only safe
	// direction for a zero value in this package to point.
	Foreign Kind = iota
	// Reclaimable means Boks writes it again by itself, at worst after a download.
	Reclaimable
	// Destructive means sandboxes and everything written inside them go with it.
	Destructive
	// Identity means a private key or a stored credential, which no re-run recreates.
	Identity
	// Configuration means rules the user typed, which no re-run recreates either.
	Configuration
	// Record means history: what Boks did, kept so it can be read afterwards.
	Record
)

func (k Kind) String() string {
	switch k {
	case Foreign:
		return "left alone"
	case Reclaimable:
		return "reclaimable"
	case Destructive:
		return "sandboxes"
	case Identity:
		return "cannot be recovered"
	case Configuration:
		return "configuration"
	case Record:
		return "record"
	}
	return "unknown"
}

// Scope is how far a purge reaches.
type Scope int

const (
	// ScopeReclaim gives the disk back and keeps everything a user would be upset to lose
	// silently: the certificate authority, stored credentials, the policy rules they
	// wrote, and the decision log. It still destroys sandboxes, because containerd's
	// content store and its snapshots share one root and there is no way to drop the image
	// layers without dropping the filesystem a sandbox is built from.
	ScopeReclaim Scope = iota
	// ScopeAll additionally removes the certificate authority, the credential store, the
	// policy rules and the decision log, leaving nothing of Boks on the machine.
	ScopeAll
)

// Entry is one thing Boks writes under the state directory.
type Entry struct {
	// Name is the entry's name directly under the state directory, which is also how it
	// is displayed. In the catalogue it is the exact name to match, unless Prefix is set.
	Name string
	// Prefix makes the catalogue entry match any name beginning with Name, for the two
	// places Boks writes through a randomly suffixed temporary file. It is deliberately a
	// separate field rather than a glob: a prefix cannot match upwards, cannot match a
	// separator, and cannot be widened by accident the way a pattern can.
	Prefix bool
	// Path is the absolute location, always filepath.Join(root, Name).
	Path string
	// Kind is what losing it costs.
	Kind Kind
	// Scope is the narrowest purge that includes this entry.
	Scope Scope
	// What is a sentence describing the entry in the user's terms.
	What string
	// Size is the bytes it occupies, and Files how many files that is spread over.
	Size  int64
	Files int
	// Symlink records that the entry is a symbolic link, in which case only the link
	// itself is removed and Size is the link's own size rather than its target's.
	Symlink bool
	// order is the catalogue position, so the listing reads in the order the catalogue
	// declares rather than in the order the filesystem happened to return.
	order int
}

// classify matches a name on disk against the catalogue.
//
// Exact names are tried before prefixes, so a catalogue that ever grew a name that is also a
// prefix of another cannot resolve to the wrong entry depending on declaration order.
func classify(name string) (Entry, bool) {
	for i, e := range catalogue {
		if !e.Prefix && e.Name == name {
			e.order = i
			return e, true
		}
	}
	for i, e := range catalogue {
		// A prefix must be a real prefix of a longer name: matching the bare prefix
		// itself would let a catalogue entry claim a file Boks never writes.
		if e.Prefix && e.Name != "" && len(name) > len(e.Name) && strings.HasPrefix(name, e.Name) {
			e.order = i
			return e, true
		}
	}
	return Entry{}, false
}

// catalogue is every name Boks writes directly under the state directory.
//
// Adding state without adding it here means `boks purge` leaves it behind and reports it as
// "not written by boks", which is the safe direction to be wrong in: it is visible, and it is
// not a deletion. TestCatalogueCoversEveryStatePath checks the reverse direction — that no
// name in here has quietly stopped being written.
var catalogue = []Entry{
	{
		Name: "containerd", Kind: Destructive, Scope: ScopeReclaim,
		What: "containerd's root and state: every image pulled and every sandbox's filesystem",
	},
	{
		Name: "net", Kind: Reclaimable, Scope: ScopeReclaim,
		What: "per-sandbox network state, rebuilt whenever a sandbox starts",
	},
	{
		Name: "certs", Kind: Reclaimable, Scope: ScopeReclaim,
		What: "copies of the CA certificate shared into sandboxes, rewritten on each start",
	},
	{
		Name: "notices", Kind: Reclaimable, Scope: ScopeReclaim,
		What: "which one-time notices have already been shown",
	},
	{
		Name: "update.json", Kind: Reclaimable, Scope: ScopeReclaim,
		What: "the cached answer of the daily release check",
	},
	{
		Name: "ca", Kind: Identity, Scope: ScopeAll,
		What: "the local certificate authority and its private key; anything trusting it stops working",
	},
	{
		Name: "secrets.json", Kind: Identity, Scope: ScopeAll,
		What: "credentials stored with 'boks secret set', including saved OAuth tokens",
	},
	{
		Name: "policy", Kind: Configuration, Scope: ScopeAll,
		What: "the rules added with 'boks policy allow' and 'boks policy deny'",
	},
	{
		Name: "policy-log.jsonl", Kind: Record, Scope: ScopeAll,
		What: "the record of every network decision Boks has taken",
	},
	// The two half-written files an interrupted process can leave directly under the
	// state directory. Both are written-then-renamed, so what is here is by definition
	// stale; reporting them as "not written by boks" would be untrue, and leaving them out
	// of --all would leave the directory undeletable for a reason nothing explained.
	{
		Name: "update.json.tmp", Kind: Reclaimable, Scope: ScopeReclaim,
		What: "a half-written release check left by an interrupted run",
	},
	// Scoped with secrets.json rather than with the other leftovers: if a process died
	// between writing this and renaming it over secrets.json, it holds a credential that
	// secrets.json does not, and a purge that promised to keep credentials must keep it.
	{
		Name: ".boks-secrets-", Prefix: true, Kind: Identity, Scope: ScopeAll,
		What: "a half-written credential store left by an interrupted run",
	},
}

// Plan is what a purge would do, and is produced before anything is removed so that the user
// decides against a list rather than against a promise.
type Plan struct {
	// Root is the validated state directory. Every path in the plan is a direct child.
	Root string
	// Exists records whether Root is there at all. A machine that has never run Boks gets
	// an empty plan rather than an error.
	Exists bool
	// Remove is what this scope takes, Keep what it deliberately leaves, and Unknown what
	// Boks did not write and will therefore never touch.
	Remove  []Entry
	Keep    []Entry
	Unknown []Entry
}

// Freed is how many bytes applying this plan gives back.
func (p Plan) Freed() int64 {
	var total int64
	for _, e := range p.Remove {
		total += e.Size
	}
	return total
}

// Total is how much the state directory occupies altogether, removed and kept.
func (p Plan) Total() int64 {
	total := p.Freed()
	for _, e := range p.Keep {
		total += e.Size
	}
	for _, e := range p.Unknown {
		total += e.Size
	}
	return total
}

// Empty reports whether there is nothing to do.
func (p Plan) Empty() bool { return len(p.Remove) == 0 }

// Unrecoverable reports whether the plan takes something no re-run recreates. It is what
// decides whether a caller may accept a y/N, or must insist on a typed word.
func (p Plan) Unrecoverable() bool {
	for _, e := range p.Remove {
		if e.Kind == Identity || e.Kind == Configuration {
			return true
		}
	}
	return false
}

// ErrNoStateDir is returned when there is no state directory to reason about.
var ErrNoStateDir = errors.New("purge: no state directory resolved")

// Root validates the directory a purge may touch and returns it in absolute, symlink-resolved
// form.
//
// The three refusals are the ones that turn a configuration mistake into a catastrophe.
// BOKS_STATE_DIR is a plain environment variable that a shell can set to the empty string or
// to "$HOME" with a typo in the suffix, and every one of those resolves to a directory full
// of somebody's work.
func Root(stateDir string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", ErrNoStateDir
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("purge: resolving %s: %w", stateDir, err)
	}
	// Judge the directory where it actually lives. A state directory that is a symlink is
	// legitimate — people move state to another disk — but the checks below have to run
	// against the target, or a link named ~/.local/state/boks pointing at $HOME passes
	// them all.
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if abs == filepath.Dir(abs) {
		return "", fmt.Errorf("purge: refusing to treat %s as a state directory: it is a filesystem root", abs)
	}
	if home, err := os.UserHomeDir(); err == nil && strings.TrimSpace(home) != "" {
		h, err := filepath.Abs(home)
		if err == nil {
			if resolved, err := filepath.EvalSymlinks(h); err == nil {
				h = resolved
			}
			// At or above: "$HOME" itself, and also "/" and "/home", which would
			// otherwise only be caught by the filesystem-root check for the first.
			if within(abs, h) {
				return "", fmt.Errorf("purge: refusing to treat %s as a state directory: "+
					"it is your home directory, or contains it", abs)
			}
		}
	}
	return abs, nil
}

// within reports whether target is root itself or lies underneath it.
func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// Inspect builds the plan for a scope without changing anything.
func Inspect(stateDir string, scope Scope) (Plan, error) {
	root, err := Root(stateDir)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{Root: root}

	info, err := os.Lstat(root)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return plan, nil
	case err != nil:
		return Plan{}, fmt.Errorf("purge: reading %s: %w", root, err)
	case !info.IsDir():
		return Plan{}, fmt.Errorf("purge: %s is not a directory", root)
	}
	plan.Exists = true

	present, err := os.ReadDir(root)
	if err != nil {
		return Plan{}, fmt.Errorf("purge: reading %s: %w", root, err)
	}
	for _, d := range present {
		entry, ok := classify(d.Name())
		// The name on the entry is always the name on disk, never the catalogue's
		// pattern: it is what is displayed, and Apply re-derives the path from it.
		entry.Name = d.Name()
		entry.Path = filepath.Join(root, d.Name())
		entry.Symlink = d.Type()&fs.ModeSymlink != 0
		entry.Size, entry.Files = measure(entry.Path, entry.Symlink)
		switch {
		case !ok:
			// Never a removal target, whatever it is. It is listed so the user can
			// see it rather than wonder what survived.
			plan.Unknown = append(plan.Unknown, entry)
		case entry.Scope <= scope:
			plan.Remove = append(plan.Remove, entry)
		default:
			plan.Keep = append(plan.Keep, entry)
		}
	}
	// Catalogue order for what is being acted on, so the largest and most consequential
	// entry is read first; alphabetical for what Boks did not write, since it has none.
	byCatalogue := func(list []Entry) {
		sort.SliceStable(list, func(i, j int) bool { return list[i].order < list[j].order })
	}
	byCatalogue(plan.Remove)
	byCatalogue(plan.Keep)
	sort.Slice(plan.Unknown, func(i, j int) bool { return plan.Unknown[i].Name < plan.Unknown[j].Name })
	return plan, nil
}

// measure returns the bytes and file count under path, without following symlinks.
//
// A symlink is measured as itself and never traversed: a link in the state directory pointing
// at a home directory must not make the plan claim it is about to free that home directory's
// worth of bytes, and it must certainly not be walked.
func measure(path string, symlink bool) (int64, int) {
	if symlink {
		if info, err := os.Lstat(path); err == nil {
			return info.Size(), 1
		}
		return 0, 1
	}
	var total int64
	var files int
	// WalkDir uses ReadDir and never follows symlinks, so a link encountered underneath is
	// counted as the link it is.
	_ = filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not a reason to abandon the count; the plan
			// is an estimate of size and an exact list of paths, and it is the list
			// that has to be right.
			return nil
		}
		if d.IsDir() {
			return nil
		}
		files++
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total, files
}

// Result is what Apply did.
type Result struct {
	// Freed is the bytes removed, summed from the plan's own measurements.
	Freed int64
	// RootRemoved reports whether the state directory itself is gone, which it is only
	// when nothing was left in it. A caller that says "Boks has no state on this machine"
	// has to read this rather than assume it from the scope: a file Boks did not write
	// keeps the directory, and saying otherwise would be a claim contradicted by `ls`.
	RootRemoved bool
}

// Apply removes everything in the plan and reports what it freed.
//
// Each target is re-checked against the plan's root immediately before it is removed rather
// than trusted from when the plan was built. The check is cheap and the failure it guards
// against — a path outside the state directory reaching os.RemoveAll — is unrecoverable.
func Apply(p Plan) (Result, error) {
	var res Result
	if p.Root == "" {
		return res, ErrNoStateDir
	}
	// Revalidate the root itself: a Plan is an ordinary struct and a caller could have
	// built one by hand.
	root, err := Root(p.Root)
	if err != nil {
		return res, err
	}
	for _, e := range p.Remove {
		if err := checkTarget(root, e); err != nil {
			return res, err
		}
		if err := os.RemoveAll(e.Path); err != nil {
			return res, fmt.Errorf("purge: removing %s: %w", e.Path, err)
		}
		res.Freed += e.Size
	}
	// A state directory with nothing left in it is itself a leftover. Removed only when
	// empty, and with os.Remove rather than os.RemoveAll, so this can never take anything
	// the plan did not list.
	if rest, err := os.ReadDir(root); err == nil && len(rest) == 0 {
		if err := os.Remove(root); err == nil {
			res.RootRemoved = true
		}
	}
	return res, nil
}

// checkTarget refuses any removal that is not a direct, named child of the root.
//
// "Direct child" is stricter than "inside the root" on purpose. Every catalogue entry is one,
// so nothing legitimate is refused, and the stricter rule rejects a traversal-shaped Name
// (".." or "a/../../b") without having to reason about how filepath.Join canonicalised it.
func checkTarget(root string, e Entry) error {
	if e.Name == "" || e.Path == "" {
		return fmt.Errorf("purge: refusing to remove an unnamed entry under %s", root)
	}
	if strings.ContainsRune(e.Name, '/') || strings.ContainsRune(e.Name, filepath.Separator) ||
		e.Name == "." || e.Name == ".." {
		return fmt.Errorf("purge: refusing to remove %q: not a plain name", e.Name)
	}
	if e.Path != filepath.Join(root, e.Name) {
		return fmt.Errorf("purge: refusing to remove %s: it is not %s under %s", e.Path, e.Name, root)
	}
	if !within(root, e.Path) || filepath.Clean(e.Path) == root {
		return fmt.Errorf("purge: refusing to remove %s: outside %s", e.Path, root)
	}
	return nil
}

// Write renders the plan for a human to decide against.
func (p Plan) Write(w io.Writer) {
	if !p.Exists {
		fmt.Fprintf(w, "no boks state directory at %s; there is nothing to purge\n", p.Root)
		return
	}
	fmt.Fprintf(w, "state directory  %s\n", p.Root)
	fmt.Fprintf(w, "total            %s\n", Bytes(p.Total()))

	if len(p.Remove) > 0 {
		fmt.Fprintf(w, "\nwill be removed:\n")
		writeEntries(w, p.Remove)
		fmt.Fprintf(w, "\n  frees %s\n", Bytes(p.Freed()))
	} else {
		fmt.Fprintf(w, "\nnothing in this scope is present; there is nothing to remove\n")
	}
	if len(p.Keep) > 0 {
		fmt.Fprintf(w, "\nkept:\n")
		writeEntries(w, p.Keep)
		fmt.Fprintf(w, "\n  'boks purge --all' removes these too.\n")
	}
	if len(p.Unknown) > 0 {
		fmt.Fprintf(w, "\nleft alone — boks did not write these and will not remove them:\n")
		writeEntries(w, p.Unknown)
	}
}

func writeEntries(w io.Writer, entries []Entry) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, e := range entries {
		name := e.Name
		if e.Symlink {
			name += " (symlink)"
		}
		what := e.What
		if what == "" {
			what = "not written by boks"
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\t%s\n", name, Bytes(e.Size), e.Kind, what)
	}
	_ = tw.Flush()
}

// Bytes renders a size the way a person reads one. Binary units, because that is what every
// other size in Boks is quoted in — MiB for a guest's memory, GiB for a link's bounds — and
// one figure in decimal MB among them would read as a different quantity.
//
// The count behind it is apparent file size, so it matches `du --apparent-size` rather than
// plain `du`: a sparse or small file occupies whole blocks on disk, and this will read lower
// than `du -h` for a directory of them. It is what the user gets back that is being promised,
// and for the gigabyte of image layers that dominates here the two agree closely.
func Bytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	value := float64(n)
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	i := -1
	for value >= unit && i < len(units)-1 {
		value /= unit
		i++
	}
	if value >= 100 {
		return fmt.Sprintf("%.0f %s", value, units[i])
	}
	return fmt.Sprintf("%.1f %s", value, units[i])
}
