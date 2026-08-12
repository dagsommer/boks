package sandbox

import (
	"strings"
	"testing"

	"github.com/dagsommer/boks/internal/policy"
)

func TestPolicyLabelRoundTrip(t *testing.T) {
	record := &policy.SandboxPolicy{
		V:       policy.SandboxPolicyVersion,
		Profile: "ci",
		Preset:  policy.PresetLocked,
		Allow:   []string{"allowed.test:443"},
		Deny:    []string{"blocked.test"},
	}
	raw, err := encodePolicyLabel(record)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	got := decodePolicy(map[string]string{LabelPolicy: raw})
	if got == nil {
		t.Fatal("the record did not survive the label")
	}
	if got.Profile != record.Profile || got.Preset != record.Preset ||
		len(got.Allow) != 1 || got.Allow[0] != record.Allow[0] ||
		len(got.Deny) != 1 || got.Deny[0] != record.Deny[0] {
		t.Errorf("record came back as %+v, want %+v", got, record)
	}
}

// TestPolicyLabelAbsentOrUnreadable: a sandbox made before the record existed, or by
// something else, is not broken. It gets what it got before — the default policy plus
// whatever the store says about it — rather than failing to start.
func TestPolicyLabelAbsentOrUnreadable(t *testing.T) {
	for name, labels := range map[string]map[string]string{
		"absent":     {},
		"empty":      {LabelPolicy: ""},
		"unreadable": {LabelPolicy: "{not json"},
	} {
		if got := decodePolicy(labels); got != nil {
			t.Errorf("%s label decoded to %+v, want nil", name, got)
		}
	}
	if raw, err := encodePolicyLabel(nil); err != nil || raw != "" {
		t.Errorf("a nil record encoded to %q (%v); it should write no label at all", raw, err)
	}
}

// TestPolicyLabelIsRefusedRatherThanTruncated: containerd caps a label's size, and a sandbox
// that silently recorded half its policy would come back up under half its containment.
func TestPolicyLabelIsRefusedRatherThanTruncated(t *testing.T) {
	record := &policy.SandboxPolicy{V: policy.SandboxPolicyVersion, Preset: policy.PresetLocked}
	for i := 0; i < 400; i++ {
		record.Allow = append(record.Allow, "host-with-a-fairly-long-name.example.com:443")
	}
	_, err := encodePolicyLabel(record)
	if err == nil {
		t.Fatal("an oversized policy record was accepted")
	}
	if !strings.Contains(err.Error(), "boks policy allow") {
		t.Errorf("error %q does not say where the rules belong instead", err)
	}
}

// TestPolicyLabelCarriesNoSecret is a guard rather than a test of behaviour: the record has
// no field a credential could travel in, and it must stay that way. A container label is
// readable by anything that can talk to containerd.
func TestPolicyLabelCarriesNoSecret(t *testing.T) {
	record := &policy.SandboxPolicy{
		V:      policy.SandboxPolicyVersion,
		Preset: policy.PresetLocked,
		Allow:  []string{"api.example.com:443"},
	}
	raw, err := encodePolicyLabel(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret", "token", "password", "credential", "inject"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Errorf("the policy label mentions %q: %s", forbidden, raw)
		}
	}
}
