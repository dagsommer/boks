package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestParseCopyArgs(t *testing.T) {
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatalf("Abs(%q): %v", p, err)
		}
		return a
	}

	tests := []struct {
		name          string
		src, dst      string
		wantSandbox   string
		wantToSandbox bool
		wantHost      string
		wantGuest     string
	}{
		{"host to sandbox", "./local.txt", "web:/tmp/local.txt", "web", true, abs("./local.txt"), "/tmp/local.txt"},
		{"sandbox to host", "web:/var/log/app", "./logs", "web", false, abs("./logs"), "/var/log/app"},
		{
			// A colon inside a host path is legal on Unix and must not be read
			// as a sandbox name.
			//
			// The expectation goes through abs() like every other case rather than
			// naming the path literally. On Unix that is the same string — Abs of an
			// absolute path is itself — so this asserts exactly what it did before. On
			// Windows "/tmp/a:b" is rooted but has no volume, so Abs prefixes the
			// working directory's drive; hard-coding the literal would have been
			// asserting that hostPath does *not* absolutise, which is not the property
			// this row is about. What it is about is the split: "/tmp/a" is not a
			// sandbox name, and that is decided by parseCopyEnd on any platform.
			"colon in host path", "/tmp/a:b", "web:/tmp/x", "web", true, abs("/tmp/a:b"), "/tmp/x",
		},
		{"relative host path with colon", "./a:b", "web:/x", "web", true, abs("./a:b"), "/x"},
		{"derived sandbox name", "boks-1a2b3c:/etc/hosts", "./hosts", "boks-1a2b3c", false, abs("./hosts"), "/etc/hosts"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := parseCopyArgs(tt.src, tt.dst)
			if err != nil {
				t.Fatalf("parseCopyArgs(%q, %q): %v", tt.src, tt.dst, err)
			}
			if cfg.Name != tt.wantSandbox {
				t.Errorf("Name = %q, want %q", cfg.Name, tt.wantSandbox)
			}
			if cfg.ToSandbox != tt.wantToSandbox {
				t.Errorf("ToSandbox = %v, want %v", cfg.ToSandbox, tt.wantToSandbox)
			}
			if cfg.HostPath != tt.wantHost {
				t.Errorf("HostPath = %q, want %q", cfg.HostPath, tt.wantHost)
			}
			if cfg.GuestPath != tt.wantGuest {
				t.Errorf("GuestPath = %q, want %q", cfg.GuestPath, tt.wantGuest)
			}
		})
	}
}

func TestParseCopyArgsRejects(t *testing.T) {
	tests := []struct {
		name     string
		src, dst string
		wantMsg  string
	}{
		{"sandbox to sandbox", "a:/x", "b:/y", "between two sandboxes"},
		{"host to host", "./a", "./b", "must name a sandbox"},
		{"relative guest path", "./a", "web:tmp/x", "must be absolute"},
		{"missing guest path", "./a", "web:", "no path inside it"},
		{"empty argument", "", "web:/x", "empty path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parseCopyArgs(tt.src, tt.dst)
			if err == nil {
				t.Fatalf("parseCopyArgs(%q, %q) = nil, want an error", tt.src, tt.dst)
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("error = %q, want it to mention %q", err, tt.wantMsg)
			}
		})
	}
}
