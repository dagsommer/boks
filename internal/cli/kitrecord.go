package cli

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/sandbox"
)

// A sandbox outlives the command that created it, and what a kit does at every boot has to
// outlive it too. `boks run --kit x` attaches x's credentials to the sandbox's traffic and runs
// its setup.startup commands; `boks start` and `boks exec` take no --kit, so a sandbox they
// brought back up had neither — the credentials silently stopped being attached and the
// per-boot commands silently stopped running.
//
// So a sandbox records the kits it was created with (sandbox.KitRecord) and the commands that
// bring one up read them again. What is recorded is the references and a digest, not the spec:
// a label is limited to a few kilobytes and a kit carries scripts and files.
//
// # What re-reading a kit does and does not do
//
// It applies the parts of a kit that are per boot — the credentials, and the startup commands —
// and leaves the once-per-sandbox parts to their markers. It never widens anything: a
// credential is attached only if the stored approval in kit-credential-approvals.json still
// matches what the kit asks for, there is no prompt on these paths, and nothing is approved
// anew. A kit that cannot be read any more is a warning and the sandbox comes up without it,
// which is the direction that costs a credential and not a containment.

// kitDigest identifies what a composition says: the merged spec, which includes the files/ tree.
// Empty if it cannot be computed, which only disables the "kit changed" note.
func kitDigest(spec *kit.Spec) string {
	if spec == nil {
		return ""
	}
	data, err := json.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}

// kitRecord is what a sandbox created by this run should remember about its kits, or nil when
// the run applied none.
//
// Each reference is kept as the user typed it, because the credential approval is keyed by it,
// and what to read in its place is kept beside it: a remote reference pinned to the commit that
// was fetched, so a branch that moves cannot change what a restart reads, and a relative path
// made absolute, because `boks start` is run from anywhere.
func (f *policyFlags) kitRecord() *sandbox.KitRecord {
	if f.kitComp == nil || len(f.kitRefs) == 0 {
		return nil
	}
	pins := maps.Clone(f.kitPins)
	if pins == nil {
		pins = map[string]string{}
	}
	for _, ref := range f.kitRefs {
		if _, ok := pins[ref]; ok || strings.HasPrefix(ref, "git+") {
			continue
		}
		if abs, err := filepath.Abs(ref); err == nil && abs != ref {
			pins[ref] = abs
		}
	}
	rec := &sandbox.KitRecord{
		V:      sandbox.KitRecordVersion,
		Refs:   slices.Clone(f.kitRefs),
		Digest: kitDigest(f.kitComp.Merged),
	}
	if len(pins) > 0 {
		rec.Pins = pins
	}
	return rec
}

// adoptRecordedKits loads the kits a sandbox was created with, for a run that named none. It is
// a no-op when the run did name some — checkKits compares those with the record — or when the
// sandbox recorded none.
//
// A kit that cannot be read is not an error. The sandbox exists and the user is trying to use
// it; refusing would make it unusable over a file that moved. It is said plainly instead,
// including what is therefore missing, and the sandbox comes up without the kit.
func (f *policyFlags) adoptRecordedKits(sandboxName string, rec *sandbox.KitRecord, warn, stderr io.Writer) {
	f.kitAdopted = true
	if rec == nil || len(f.kitRefs) > 0 {
		return
	}
	f.kitRefs, f.kitReplay, f.recordedFor = slices.Clone(rec.Refs), rec.Pins, sandboxName
	if err := f.loadKit(warn); err != nil {
		fmt.Fprintf(stderr, "warning: sandbox %q was created with kit %s, which can no longer be read:\n"+
			"         %v\n"+
			"         Continuing WITHOUT it: its credentials are not attached and its setup.startup\n"+
			"         commands do not run. Restore the kit where it was, or recreate the sandbox.\n",
			sandboxName, strings.Join(rec.Refs, ", "), err)
		f.kitRefs, f.kitComp, f.kitSpec = nil, nil, nil
		return
	}
	if d := kitDigest(f.kitComp.Merged); rec.Digest != "" && d != "" && d != rec.Digest {
		fmt.Fprintf(stderr, "note: kit %s has changed since sandbox %q was created. The current version is\n"+
			"      applied; a credential it now asks for differently is asked about again, not attached.\n",
			strings.Join(rec.Refs, ", "), sandboxName)
	}
}

// checkKits refuses a --kit that is not the one the sandbox was created with. What a kit sets up
// is decided when the sandbox is created — its agent, environment, volumes and ports — so a
// re-attach naming a different kit would be asking for a sandbox that is not the one it reaches.
// A sandbox with no recorded kit has nothing to disagree with.
func checkKits(net *policyFlags, inv invocation, c *fixedConflicts) {
	have := inv.info.Kits
	if net == nil || have == nil || len(net.kitRefs) == 0 || net.recordedFor != "" {
		return
	}
	if slices.Equal(net.kitRefs, have.Refs) {
		return
	}
	c.add("--kit "+strings.Join(net.kitRefs, " --kit "),
		"the sandbox was created with --kit "+strings.Join(have.Refs, " --kit "))
}
