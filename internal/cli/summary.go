package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/dagsommer/boks/internal/sandbox"
)

// sandboxSummary is what the SANDBOX block says about a sandbox about to be started.
type sandboxSummary struct {
	cfg sandbox.Config
	kit string
}

// printSandboxSummary writes the block a person reads to confirm what they are about to run:
// the name, the agent, the kit it came from, where the work is shared from, the image and the
// size of the machine.
//
// It is printed when a sandbox is created, and with -v. Not on every run: the block is
// identical each time an unchanged sandbox is re-attached, and output that is the same every
// time is output people learn to skip — which is how a security notice printed beside it goes
// unread. See notice.go for what is printed on every run for that reason.
func printSandboxSummary(w io.Writer, s sandboxSummary) {
	cfg := s.cfg
	fmt.Fprintf(w, "\n  SANDBOX\n")
	row := func(label, value string) { fmt.Fprintf(w, "    %-11s %s\n", label+":", value) }
	row("Name", cfg.Name)
	if cfg.Agent != "" {
		row("Agent", cfg.Agent)
	}
	if s.kit != "" {
		row("Kit", s.kit)
	}
	for i, ws := range cfg.Workspaces {
		label := "Workspace"
		if i > 0 {
			label = ""
		}
		row(label, fmt.Sprintf("%s (%s)", tildePath(ws.HostPath), ws.Mode))
	}
	row("Image", cfg.Image)
	row("CPU", fmt.Sprint(cfg.CPUs))
	row("Memory", memoryText(cfg.MemoryMiB))
	fmt.Fprintln(w)
}

// tildePath shortens a path under the home directory, as a shell prompt would.
func tildePath(p string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return p
	}
	if p == home {
		return "~"
	}
	if rest, ok := strings.CutPrefix(p, home+string(filepath.Separator)); ok {
		return filepath.Join("~", rest)
	}
	return p
}

// memoryText renders a size in binary units, whole GiB where it is one.
func memoryText(mib int) string {
	if mib >= 1024 && mib%1024 == 0 {
		return fmt.Sprintf("%d GiB", mib/1024)
	}
	return fmt.Sprintf("%d MiB", mib)
}
