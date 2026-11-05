package sandbox

import (
	"strings"
	"testing"
)

func TestRenderPullBar(t *testing.T) {
	got := renderPullBar(512<<20, 1024<<20, 9)
	if !strings.Contains(got, "[█████░░░░░]") || !strings.Contains(got, "512.0 MiB / 1.0 GiB") {
		t.Errorf("bar = %q", got)
	}
	if got := renderPullBar(5, 0, 2); strings.ContainsAny(got, "█░[") {
		t.Errorf("a bar with no total should not draw one: %q", got)
	}
	if got := renderPullBar(2<<30, 1<<30, 1); !strings.Contains(got, "[██████████]") {
		t.Errorf("over-full bar should clamp: %q", got)
	}
}
