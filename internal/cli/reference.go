package cli

// The CLI reference in docs/cli.md is generated from this file, from the same cobra command
// tree that `boks --help` prints. Nothing in that document is typed by hand, so it cannot
// drift: a flag renamed in run.go changes the reference on the next `make docs`, and
// TestReferenceIsCurrent fails the build until it does.
//
// # Why this walks cobra rather than calling cobra/doc
//
// github.com/spf13/cobra/doc would do most of this, and the intent was to use it. It is a
// package of the cobra module, but it imports go-md2man for its man-page generator, which
// pulls github.com/cpuguy83/go-md2man/v2 and github.com/russross/blackfriday into go.mod as
// new module requirements — go.sum carries only their /go.mod hashes today, so they are not
// even downloaded. This project's brief is that the documentation work adds no dependency,
// and a man-page renderer is a lot of supply chain to take on for Markdown this page does
// not use. Walking the tree with cobra and pflag — both already direct dependencies — reads
// the same commands, the same flags and the same help text, and produces one page instead of
// forty files. The anti-drift property is identical, because the source is identical.
//
// # What is deliberately left out
//
// Hidden flags and hidden commands. The four development flags in devFlags are hidden from
// `boks --help` on purpose, and the root command's own long help already explains them in
// the words their author chose; repeating them in a table would be documenting an escape
// hatch as if it were an interface.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// referencePreamble is the only prose in docs/cli.md that is not cobra's own. It says the
// page is generated, because a reader who edits a generated file and loses the edit has
// been failed by the file, not by themselves.
const referencePreamble = `# CLI reference

Every command, flag and default below is read out of the command tree at build time, so this
page describes the ` + "`boks`" + ` you have rather than the one somebody wrote about. The same
text is available as ` + "`boks <command> --help`" + `.

**This file is generated. Do not edit it.** It is produced by ` + "`make docs`" + ` from
` + "[`internal/cli/reference.go`](https://github.com/dagsommer/boks/blob/main/internal/cli/reference.go)" + `,
and ` + "`go test ./internal/cli/`" + ` fails when the checked-in copy is out of date. To change
something here, change the command that produces it.

Four flags for developing Boks itself are accepted by every command and hidden from ` + "`--help`" + `:
` + "`--runtime`" + `, ` + "`--snapshotter`" + `, ` + "`--containerd-address`" + ` and
` + "`--i-know-this-is-not-isolated`" + `. They are omitted below for the same reason they are
hidden. The last one turns off the refusal to present a runtime with no VM boundary as a
sandbox, and must never be used to run anything untrusted.
`

// Reference renders the whole command tree as one Markdown document.
func Reference() string {
	// The reference describes the command line, not a build, so the version is left out:
	// embedding `git describe` output would make the generated file differ between a tagged
	// checkout and a dirty one, and TestReferenceIsCurrent would fail for everyone.
	root := newRootCommand(Env{})
	root.InitDefaultCompletionCmd()

	var b strings.Builder
	b.WriteString(referencePreamble)

	// Commands, alphabetically at each level — cobra's own order in `--help`, so the page
	// and the help output list things in the same sequence.
	writeCommands(&b, root.Commands(), 2)

	// Only if there are any that are not hidden. Today every persistent flag on the root is
	// a development flag, so this section does not appear — an empty table with a heading
	// over it would read as a bug in the page.
	if hasVisibleFlags(root.PersistentFlags()) {
		b.WriteString("\n## Flags every command takes\n\n")
		writeFlags(&b, root.PersistentFlags())
	}

	return generalisePaths(b.String())
}

// generalisePaths turns the machine the generator ran on back into a machine.
//
// Several defaults are absolute paths derived from the environment — the decision log is
// under XDG_STATE_HOME or ~/.local/state — so a reference generated on a laptop would say
// /Users/someone/... and a reference generated on a CI runner would say /home/runner/...,
// and the check that keeps the file current would fail for everyone who is not the last
// person to have run it. This was found by CI disagreeing with a clean local run, which is
// the cheapest possible way to find it.
//
// Rewriting them is not only a fix for determinism. `~/.local/state/boks/policy-log.jsonl`
// is what a reader needs; the generator's home directory is noise that happens to be true.
//
// Order matters: the XDG directories are usually under the home directory, so they are
// replaced first, or the home substitution would get there first and leave `~/state`.
func generalisePaths(s string) string {
	home, err := os.UserHomeDir()

	for _, sub := range []struct{ env, generic string }{
		{"XDG_STATE_HOME", "~/.local/state"},
		{"XDG_DATA_HOME", "~/.local/share"},
		{"XDG_CONFIG_HOME", "~/.config"},
		{"XDG_CACHE_HOME", "~/.cache"},
	} {
		if v := strings.TrimRight(os.Getenv(sub.env), "/"); v != "" {
			s = strings.ReplaceAll(s, v, sub.generic)
		} else if err == nil {
			// Unset: the default the CLI computed is the generic path already, and it is
			// spelled with the real home directory.
			s = strings.ReplaceAll(s, filepath.Join(home, strings.TrimPrefix(sub.generic, "~/")), sub.generic)
		}
	}
	if err == nil && home != "" && home != "/" {
		s = strings.ReplaceAll(s, home, "~")
	}
	return s
}

func writeCommands(b *strings.Builder, cmds []*cobra.Command, level int) {
	for _, cmd := range cmds {
		// `help` is cobra's own, and documenting it would document cobra.
		if cmd.Hidden || cmd.Name() == "help" || !cmd.IsAvailableCommand() {
			continue
		}
		writeCommand(b, cmd, level)
		writeCommands(b, cmd.Commands(), level+1)
	}
}

func writeCommand(b *strings.Builder, cmd *cobra.Command, level int) {
	fmt.Fprintf(b, "\n%s %s\n\n", strings.Repeat("#", level), cmd.CommandPath())

	if short := strings.TrimSpace(cmd.Short); short != "" {
		fmt.Fprintf(b, "%s\n\n", short)
	}

	fmt.Fprintf(b, "```\n%s\n```\n", strings.TrimSpace(cmd.UseLine()))

	// The long help is the author's prose and is reproduced unchanged. It is hard-wrapped
	// for a terminal; kramdown is configured with hard_wrap off, so the wrapping does not
	// survive as line breaks and the paragraphs read as paragraphs.
	if long := strings.TrimSpace(cmd.Long); long != "" && long != strings.TrimSpace(cmd.Short) {
		fmt.Fprintf(b, "\n%s\n", strings.TrimRight(indentedBlocksAsCode(long), "\n"))
	}

	if len(cmd.Aliases) > 0 {
		fmt.Fprintf(b, "\nAlso spelled: %s\n", codeList(cmd.Aliases))
	}

	if ex := strings.TrimSpace(cmd.Example); ex != "" {
		fmt.Fprintf(b, "\n```\n%s\n```\n", ex)
	}

	if hasVisibleFlags(cmd.NonInheritedFlags()) {
		b.WriteString("\n")
		writeFlags(b, cmd.NonInheritedFlags())
	}
}

// indentedBlocksAsCode protects a long help text from Markdown.
//
// Help text written for a terminal indents its examples, and Markdown reads an indented
// block as code, which is what is wanted. What is not wanted is a line that happens to begin
// with `#` becoming a heading, or a wrapped line beginning with `-` becoming a list item, in
// the middle of a paragraph. Rather than guess, every paragraph that contains an indented
// line is emitted verbatim inside a fence, and the rest is emitted as prose with the handful
// of characters that start a block escaped.
func indentedBlocksAsCode(long string) string {
	var out []string
	for _, para := range strings.Split(long, "\n\n") {
		lines := strings.Split(para, "\n")
		indented := false
		for _, line := range lines {
			if strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "\t") {
				indented = true
				break
			}
		}
		if indented {
			out = append(out, "```\n"+strings.TrimRight(para, "\n")+"\n```")
			continue
		}
		for i, line := range lines {
			lines[i] = escapeBlockStart(line)
		}
		out = append(out, strings.Join(lines, "\n"))
	}
	return strings.Join(out, "\n\n") + "\n"
}

// escapeBlockStart neutralises a leading character that would start a Markdown block. Only
// the first character of a line can do that, so only the first character is touched, and the
// backslash is invisible once rendered.
func escapeBlockStart(line string) string {
	if line == "" {
		return line
	}
	switch line[0] {
	case '#', '-', '+', '>', '=':
		return "\\" + line
	}
	// `1.` and friends start an ordered list.
	if i := strings.IndexByte(line, '.'); i > 0 && i <= 9 && allDigits(line[:i]) {
		return line[:i] + "\\" + line[i:]
	}
	return line
}

func allDigits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func hasVisibleFlags(set *pflag.FlagSet) bool {
	found := false
	set.VisitAll(func(f *pflag.Flag) {
		if !f.Hidden {
			found = true
		}
	})
	return found
}

// writeFlags renders a flag set as a table: name, default, meaning. A table rather than
// cobra's aligned block because the site wraps tables in something that scrolls, and a
// pre-aligned block on a phone is a horizontal scroll through the whole page.
func writeFlags(b *strings.Builder, set *pflag.FlagSet) {
	b.WriteString("| Flag | Default | Meaning |\n|---|---|---|\n")
	set.VisitAll(func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name := "`--" + f.Name
		varname, usage := pflag.UnquoteUsage(f)
		if varname != "" && varname != "bool" {
			name += " " + varname
		}
		name += "`"
		if f.Shorthand != "" {
			name = "`-" + f.Shorthand + "`, " + name
		}
		def := ""
		if f.DefValue != "" && f.DefValue != "false" && f.DefValue != "[]" {
			def = "`" + f.DefValue + "`"
		}
		fmt.Fprintf(b, "| %s | %s | %s |\n", name, def, cell(usage))
	})
}

// cell flattens a usage string into one table cell. A `|` inside one would end the cell.
func cell(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

func codeList(items []string) string {
	quoted := make([]string, len(items))
	for i, item := range items {
		quoted[i] = "`" + item + "`"
	}
	return strings.Join(quoted, ", ")
}
