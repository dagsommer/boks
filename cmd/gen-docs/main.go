// Command gen-docs writes the generated parts of docs/ from the code that produces them.
//
// Today that is docs/cli.md, rendered from the cobra command tree. It is a separate binary
// rather than a `go:generate` line so that `make docs` is the one way to do it, and so that
// the check which keeps the checked-in file honest — TestReferenceIsCurrent — can compare
// against the same function rather than against a subprocess.
//
// Usage:
//
//	go run ./cmd/gen-docs            # write the files
//	go run ./cmd/gen-docs -check     # exit 1 if any is out of date, write nothing
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dagsommer/boks/internal/cli"
)

func main() {
	check := flag.Bool("check", false, "report whether the files are current instead of writing them")
	dir := flag.String("dir", "docs", "directory the generated documents live in")
	flag.Parse()

	files := map[string]string{
		"cli.md": cli.Reference(),
	}

	stale := false
	for name, want := range files {
		path := filepath.Join(*dir, name)
		got, err := os.ReadFile(path)
		current := err == nil && string(got) == want

		if *check {
			if !current {
				fmt.Fprintf(os.Stderr, "%s is out of date; run `make docs`\n", path)
				stale = true
			}
			continue
		}
		if current {
			continue
		}
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "gen-docs: %v\n", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "wrote %s\n", path)
	}
	if stale {
		os.Exit(1)
	}
}
