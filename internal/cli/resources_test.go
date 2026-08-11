package cli

import (
	"strings"
	"testing"
)

func TestParseMemory(t *testing.T) {
	tests := []struct {
		text string
		want int
	}{
		{"1024m", 1024},
		{"8g", 8192},
		{"8G", 8192},
		{"2gib", 2048},
		{"512M", 512},
		{"1t", 1024 * 1024},
		{"2097152k", 2048},
		{"1073741824", 1024}, // a bare number is bytes
		{" 4g ", 4096},
	}
	for _, tt := range tests {
		got, err := parseMemory(tt.text)
		if err != nil {
			t.Errorf("parseMemory(%q): %v", tt.text, err)
			continue
		}
		if got != tt.want {
			t.Errorf("parseMemory(%q) = %d MiB, want %d", tt.text, got, tt.want)
		}
	}
}

func TestParseMemoryRejects(t *testing.T) {
	for _, text := range []string{"", "g", "-1g", "0", "12x", "1.5g", "1 g"} {
		if got, err := parseMemory(text); err == nil {
			t.Errorf("parseMemory(%q) = %d, want an error", text, got)
		}
	}
}

// A bare number is bytes, so `-memory 2048` is 2 KiB rather than 2 GiB. That is a mistake
// worth catching with the spelling the user meant, not one worth booting.
func TestParseMemoryExplainsAMissingSuffix(t *testing.T) {
	_, err := parseMemory("2048")
	if err == nil {
		t.Fatal("2048 bytes was accepted as a guest memory size")
	}
	if !strings.Contains(err.Error(), "2048m") {
		t.Errorf("error = %q, want it to suggest 2048m", err)
	}
}

// Automatic sizing has to produce something a guest can actually boot with, on any host.
func TestAutoSizing(t *testing.T) {
	if got := autoCPUs(); got < 1 {
		t.Errorf("autoCPUs() = %d, want at least 1", got)
	}
	got := autoMemoryMiB()
	if got < fallbackMemoryMiB || got > autoMemoryCapMiB {
		t.Errorf("autoMemoryMiB() = %d, want between %d and %d", got, fallbackMemoryMiB, autoMemoryCapMiB)
	}
}
