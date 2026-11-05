package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/dagsommer/boks/internal/kit"
)

// newKitCommand groups the commands that answer questions about kits without creating
// anything. A kit is a file that grants network access and runs commands as root, so reading
// what it will do is something to do before running it.
func newKitCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kit",
		Short: "Inspect kits",
		Long: `Commands that read a kit and say what it declares, without creating a sandbox.
See docs/kits.md.`,
	}
	cmd.AddCommand(newKitInspectCommand(env))
	return cmd
}

func newKitInspectCommand(env Env) *cobra.Command {
	var resolved bool
	cmd := &cobra.Command{
		Use:   "inspect REF [REF...]",
		Short: "Print what a kit declares, or what stacked kits resolve to",
		Long: `Reads each kit and prints its spec: the same fields 'boks run --kit' reads, with the
files/ tree listed rather than dumped. Nothing is created and no credential is read.

With --resolved the references are composed exactly as repeated --kit flags are: each kit's
extends chain is merged into it, the kits its mixins field names follow, and the references
are stacked in the order given. The output is then the members in application order, the
merged spec, the credentials the stack would ask you to approve, and the execution plan —
what runs in the guest, stage by stage, and which kit each step comes from.`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("%s needs a kit reference", cmd.CommandPath())
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			if resolved {
				return inspectResolved(env, args)
			}
			for i, ref := range args {
				if i > 0 {
					fmt.Fprintln(env.Stdout)
				}
				spec, warnings, err := kit.Load(ref)
				if err != nil {
					return err
				}
				if err := kit.Validate(spec); err != nil {
					return fmt.Errorf("kit %s: %w", ref, err)
				}
				for _, w := range warnings {
					fmt.Fprintf(env.Stderr, "kit %s: %s\n", ref, w)
				}
				fmt.Fprintf(env.Stdout, "# %s\n", ref)
				if err := writeSpec(env.Stdout, spec); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&resolved, "resolved", false,
		"compose the kits as --kit does and print the merged spec and the execution plan")
	return cmd
}

func inspectResolved(env Env, refs []string) error {
	comp, err := kit.Resolve(refs, nil)
	if err != nil {
		return err
	}
	for _, w := range comp.Warnings {
		fmt.Fprintln(env.Stderr, w)
	}
	out := env.Stdout

	fmt.Fprintln(out, "kits, in the order they are applied:")
	for i, m := range comp.Members {
		note := ""
		switch {
		case m.Via != "":
			note = ", " + m.Via
		case len(m.Lineage) > 0:
			note = ", extends " + strings.Join(m.Lineage, " > ")
		}
		fmt.Fprintf(out, "  %d. %s (%s) from %s%s\n", i+1, m.Spec.Name, m.Spec.Kind, m.Ref, note)
	}
	for _, m := range comp.Members {
		if r := m.Spec.Requires; r != nil && r.Agent != "" {
			fmt.Fprintf(out, "  %s requires agent %s\n", m.Spec.Name, r.Agent)
		}
	}

	fmt.Fprintln(out, "\nmerged spec:")
	if err := writeSpec(out, comp.Merged); err != nil {
		return err
	}

	var creds []string
	for _, m := range comp.Members {
		for _, c := range m.Spec.Credentials {
			kind := "API key"
			if c.APIKey == nil && c.OAuth != nil {
				kind = "OAuth login"
			}
			creds = append(creds, fmt.Sprintf("  %s (%s) declared by %s", c.Service, kind, m.Spec.Name))
		}
	}
	if len(creds) > 0 {
		fmt.Fprintln(out, "\ncredentials (each kit's are approved on their own; nothing is attached without it):")
		fmt.Fprintln(out, strings.Join(creds, "\n"))
	}

	fmt.Fprintln(out, "\nexecution plan:")
	fmt.Fprint(out, kit.RenderPlan(kit.Plan(comp.Members)))
	return nil
}

// writeSpec prints a spec as YAML, followed by a summary of its files/ tree. The tree is
// summarised because its content is arbitrary bytes that arrived with the kit.
func writeSpec(w io.Writer, spec *kit.Spec) error {
	data, err := yaml.Marshal(spec)
	if err != nil {
		return err
	}
	fmt.Fprint(w, string(data))
	if spec.Tree.Empty() {
		return nil
	}
	fmt.Fprintln(w, "files:")
	for _, f := range spec.Tree.Home {
		fmt.Fprintf(w, "  home/%s (%d bytes)\n", f.Path, len(f.Data))
	}
	for _, f := range spec.Tree.Workspace {
		fmt.Fprintf(w, "  workspace/%s (%d bytes)\n", f.Path, len(f.Data))
	}
	return nil
}
