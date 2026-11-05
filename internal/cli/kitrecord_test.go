package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/sandbox"
)

const recordedKit = `schemaVersion: "2"
kind: mixin
name: recorded
permissions:
  network:
    allow: [api.recorded.test]
credentials:
  - service: recorded-token
    apiKey:
      name: RECORDED_TOKEN
      proxyManaged: true
      inject:
        - domain: api.recorded.test
          scheme: bearer
setup:
  startup:
    - command: ["recorded-daemon"]
      background: true
`

// loadedFlags is what `boks run --kit dir` leaves in the flags, and the record a sandbox created
// by it would carry.
func loadedFlags(t *testing.T, dir string) (*policyFlags, *sandbox.KitRecord) {
	t.Helper()
	f := &policyFlags{kitRefs: []string{dir}}
	if err := f.loadKit(&bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	rec := f.kitRecord()
	if rec == nil {
		t.Fatal("no record for a loaded kit")
	}
	return f, rec
}

func TestKitRecordHoldsReferencesAndADigestNotTheSpec(t *testing.T) {
	dir := writeKit(t, recordedKit)
	f, rec := loadedFlags(t, dir)
	if !slices.Equal(rec.Refs, []string{dir}) || rec.Digest == "" || rec.V != sandbox.KitRecordVersion {
		t.Errorf("record = %+v", rec)
	}
	if rec.Digest != kitDigest(f.kitSpec) {
		t.Errorf("digest is not the merged spec's")
	}
	if (&policyFlags{}).kitRecord() != nil {
		t.Errorf("a run with no kit must record nothing")
	}
}

// A relative path means nothing to `boks start` run from somewhere else, so what is read on a
// restart is absolute; the reference itself is kept, because the approval is keyed by it.
func TestKitRecordMakesARelativePathAbsolute(t *testing.T) {
	dir := writeKit(t, recordedKit)
	t.Chdir(filepath.Dir(dir))
	rel := "./" + filepath.Base(dir)
	_, rec := loadedFlags(t, rel)
	if rec.Refs[0] != rel {
		t.Errorf("ref = %q, want %q as typed", rec.Refs[0], rel)
	}
	if got := rec.Target(rel); !filepath.IsAbs(got) {
		t.Errorf("Target(%q) = %q, want an absolute path", rel, got)
	}
}

func TestAdoptRecordedKitsReloadsWhatTheSandboxWasCreatedWith(t *testing.T) {
	t.Setenv(policy.StateDirEnv, t.TempDir())
	dir := writeKit(t, recordedKit)
	_, rec := loadedFlags(t, dir)

	var stderr bytes.Buffer
	f := &policyFlags{}
	f.adoptRecordedKits("box", rec, &bytes.Buffer{}, &stderr)
	if f.kitSpec == nil || f.kitSpec.Name != "recorded" || !slices.Equal(f.kitRefs, rec.Refs) {
		t.Fatalf("kit not reloaded: %+v refs %v\n%s", f.kitSpec, f.kitRefs, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("an unchanged kit said something:\n%s", stderr.String())
	}
	if setup := kitSetup(f.kitSpec); setup == nil || len(setup.Startup) != 1 {
		t.Errorf("the per-boot startup command did not come back: %+v", setup)
	}
}

// The approval gate is the same one `run` uses, and a restart can neither ask nor approve: the
// stored approval is what decides, and an unapproved credential is left out, with the way to
// approve it named.
func TestRecordedKitCredentialsStayBehindTheStoredApproval(t *testing.T) {
	t.Setenv(policy.StateDirEnv, t.TempDir())
	dir := writeKit(t, recordedKit)
	_, rec := loadedFlags(t, dir)

	var stderr bytes.Buffer
	f := &policyFlags{stdin: strings.NewReader("a\n"), stderr: &stderr}
	f.adoptRecordedKits("box", rec, &bytes.Buffer{}, &stderr)
	f.stdin = nil // `start` and `exec` have no terminal to hand the gate
	plan, err := f.planCredentials(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.inject) != 0 || len(plan.guest) != 0 {
		t.Errorf("an unapproved credential was attached: %v %v", plan.inject, plan.guest)
	}
	if !strings.Contains(stderr.String(), "boks run --name box") {
		t.Errorf("the way to approve is not named:\n%s", stderr.String())
	}

	// Once approved — by an interactive run, which keys it by the reference as typed — the
	// same restart attaches it, with its stand-in in the guest.
	unit := f.kitUnits()[0]
	specs, _ := kitInjectSpecs(unit.spec)
	asks, _ := kitCredentialAsks(specs, nil)
	approvals, _ := loadKitApprovals(kitApprovalsPath())
	approvals.record(unit.name+"\x00"+dir, asks[0])
	if err := approvals.save(); err != nil {
		t.Fatal(err)
	}
	plan, err = f.planCredentials(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.inject) != 1 || len(plan.guest) != 1 {
		t.Errorf("an approved credential was not attached: %v %v", plan.inject, plan.guest)
	}
}

func TestAdoptRecordedKitsWarnsAndContinuesWhenTheKitIsGone(t *testing.T) {
	dir := writeKit(t, recordedKit)
	_, rec := loadedFlags(t, dir)
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	f := &policyFlags{}
	f.adoptRecordedKits("box", rec, &bytes.Buffer{}, &stderr)
	if f.kitSpec != nil || len(f.kitRefs) != 0 {
		t.Errorf("a kit that cannot be read is still applied: %v %v", f.kitSpec, f.kitRefs)
	}
	for _, want := range []string{"WITHOUT it", "credentials are not attached", "setup.startup"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("warning lacks %q:\n%s", want, stderr.String())
		}
	}
}

func TestAdoptRecordedKitsNotesAKitThatChanged(t *testing.T) {
	dir := writeKit(t, recordedKit)
	_, rec := loadedFlags(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "spec.yaml"),
		[]byte(strings.Replace(recordedKit, "recorded-daemon", "other-daemon", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	f := &policyFlags{}
	f.adoptRecordedKits("box", rec, &bytes.Buffer{}, &stderr)
	if f.kitSpec == nil || !strings.Contains(stderr.String(), "has changed") {
		t.Errorf("a changed kit is applied with a note, got spec %v:\n%s", f.kitSpec, stderr.String())
	}
}

func TestAdoptRecordedKitsIgnoresASandboxWithNone(t *testing.T) {
	f := &policyFlags{}
	f.adoptRecordedKits("box", nil, &bytes.Buffer{}, &bytes.Buffer{})
	if f.kitSpec != nil || !f.kitAdopted {
		t.Errorf("spec %v adopted %v", f.kitSpec, f.kitAdopted)
	}
}

// A --kit on a re-attach has to be the kit the sandbox was created with, in the style of every
// other fixed value; a sandbox that recorded none has nothing to disagree with, and a run that
// was handed its kits by the record cannot disagree with itself.
func TestCheckKitsRefusesADifferentKit(t *testing.T) {
	inv := invocation{name: "box", info: sandbox.Info{Kits: &sandbox.KitRecord{Refs: []string{"./a"}}}}
	cases := []struct {
		name     string
		net      *policyFlags
		inv      invocation
		conflict bool
	}{
		{"same", &policyFlags{kitRefs: []string{"./a"}}, inv, false},
		{"different", &policyFlags{kitRefs: []string{"./b"}}, inv, true},
		{"extra", &policyFlags{kitRefs: []string{"./a", "./b"}}, inv, true},
		{"none given", &policyFlags{}, inv, false},
		{"adopted", &policyFlags{kitRefs: []string{"./a"}, recordedFor: "box"}, inv, false},
		{"nothing recorded", &policyFlags{kitRefs: []string{"./b"}}, invocation{name: "box"}, false},
	}
	for _, tc := range cases {
		c := fixedConflicts{sandbox: "box"}
		checkKits(tc.net, tc.inv, &c)
		if got := len(c.entries) > 0; got != tc.conflict {
			t.Errorf("%s: conflict = %v, want %v", tc.name, got, tc.conflict)
		}
	}
	c := fixedConflicts{sandbox: "box"}
	checkKits(&policyFlags{kitRefs: []string{"./b"}}, inv, &c)
	if err := c.err(); err == nil || !strings.Contains(err.Error(), "--kit ./b") ||
		!strings.Contains(err.Error(), "created with --kit ./a") {
		t.Errorf("the refusal must name both kits: %v", err)
	}
}
