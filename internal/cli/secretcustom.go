package cli

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/dagsommer/boks/internal/secret"
	"github.com/spf13/cobra"
)

// newSecretSetCustomCommand is `sbx secret set-custom`: a credential keyed on the hosts it goes
// to and the environment variable the guest reads, with a generated placeholder. Where `secret
// set` stores a value and leaves the host and header to a rule on every run, this stores the
// whole mapping with the value, so every sandbox that reaches those hosts carries it.
func newSecretSetCustomCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-custom [flags]",
		Short: "Store a credential for hosts and an env var, with a generated placeholder",
		Long: `Stores a credential keyed on the hosts it may be sent to and the environment variable the
guest reads it from (--env is optional: a kit may already put the placeholder in the guest's
environment itself). Inside the sandbox that variable holds a placeholder, generated unless
--placeholder gives one ('github_pat_{rand}' keeps a vendor prefix); wherever the placeholder appears in a request header on one of the hosts, the host
proxy replaces it with the real value. Headers only, never bodies. The guest never sees the value.

Setting the same hosts and variable again replaces the value and keeps the placeholder, so a
rotated token needs nothing changed in a sandbox that already holds it.

The value comes from the prompt, --value-stdin, --ref (read with 'op read') or --command (run on
the host from an empty temporary directory, once per sandbox start). A custom secret applies to
every sandbox unless --sandbox scopes it. Like any credential, it makes the hosts it names ones
whose TLS boks terminates, and it is not an allow rule: the host still has to be reachable.

--host takes the same patterns as network rules: *.example.com covers every subdomain
(**.example.com is accepted as the same thing).`,
		Example: `  printf '%s' "$API_KEY" | boks secret set-custom --host api.example.com --env API_KEY --value-stdin
  boks secret set-custom --sandbox web --host npm.pkg.github.com --env NODE_AUTH_TOKEN --command 'gh auth token'`,
		Args: noArgs,
	}
	var hosts []string
	var envName, value, ref, command, path, sandbox, placeholder string
	var valueStdin bool
	cmd.Flags().StringArrayVar(&hosts, "host", nil, "a host the secret may be sent to (repeatable)")
	cmd.Flags().StringVar(&envName, "env", "", "the environment variable the guest reads the placeholder from")
	cmd.Flags().StringVar(&placeholder, "placeholder", "",
		"what the guest holds instead; {rand} becomes random text, as in 'github_pat_{rand}' (default sbx-cs-{rand})")
	cmd.Flags().StringVar(&value, "value", "", "the credential; omit to be prompted. Visible in the process list")
	cmd.Flags().BoolVar(&valueStdin, "value-stdin", false, "read the credential from stdin")
	cmd.Flags().StringVar(&ref, "ref", "", "resolve the value from a reference such as op://Vault/Item/field")
	cmd.Flags().StringVar(&command, "command", "", "resolve the value by running this host command")
	cmd.Flags().StringVar(&sandbox, "sandbox", "", "scope this secret to one sandbox")
	storeFlag(cmd, &path)

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		c := secret.Custom{Env: envName, Ref: ref, Command: command}
		for _, h := range hosts {
			for _, part := range strings.Split(h, ",") {
				if part = strings.TrimSpace(part); part != "" {
					c.Hosts = append(c.Hosts, part)
				}
			}
		}
		sources := 0
		for _, set := range []bool{value != "", valueStdin, ref != "", command != ""} {
			if set {
				sources++
			}
		}
		if sources > 1 {
			return usagef("--value, --value-stdin, --ref and --command are alternatives")
		}
		if err := validateCustomHosts(c.Hosts); err != nil {
			return err
		}
		name := secret.CustomName(c.Env, c.Hosts)
		if err := secret.ValidateScopedName(sandbox, name); err != nil {
			return err
		}
		switch {
		case ref != "" || command != "":
		case valueStdin:
			data, err := io.ReadAll(env.Stdin)
			if err != nil {
				return fmt.Errorf("reading the credential from stdin: %w", err)
			}
			c.Value = strings.TrimRight(string(data), "\r\n")
		case value != "":
			c.Value = value
		default:
			v, err := confirmSecretValue(env.Stdin, env.Stdout, c.Env)
			if err != nil {
				return err
			}
			c.Value = v
		}
		store, err := openSecretStore(path)
		if err != nil {
			return err
		}
		stored := name
		if sandbox != "" {
			stored = secret.ScopedName(sandbox, name)
		}
		// Keep the placeholder of a secret being replaced: a sandbox that already holds it
		// would otherwise keep a placeholder nothing recognises.
		if old, err := store.Lookup(cmd.Context(), stored); err == nil && secret.IsCustom(old.Reveal()) {
			if prev, derr := secret.DecodeCustom(old.Reveal()); derr == nil {
				c.Placeholder = prev.Placeholder
			}
		}
		template := placeholder
		if template == "" {
			template = secret.CustomPlaceholderPrefix + "{rand}"
		}
		if placeholder != "" || c.Placeholder == "" {
			if c.Placeholder, err = secret.ExpandPlaceholder(template, c.Placeholder); err != nil {
				return err
			}
		}
		if err := c.Validate(); err != nil {
			return err
		}
		// Two secrets on one host are told apart by their placeholders, so two with the same
		// one could never be.
		if err := refuseDuplicatePlaceholder(cmd.Context(), store, stored, c.Placeholder); err != nil {
			return err
		}
		encoded, err := c.Encode()
		if err != nil {
			return err
		}
		if err := store.Set(stored, secret.NewValue(encoded)); err != nil {
			return explainSecretFailure(store.Path(), err)
		}
		scope := "every sandbox"
		if sandbox != "" {
			scope = "sandbox " + sandbox
		}
		held := "the guest holds " + c.Placeholder
		if c.Env != "" {
			held = "$" + c.Env + " in the guest = " + c.Placeholder
		}
		fmt.Fprintf(env.Stdout, "stored %s for %s in %s\n  %s\n  sent to: %s\n",
			stored, scope, store.Describe(), held, strings.Join(c.Hosts, ", "))
		return nil
	}
	return cmd
}

func refuseDuplicatePlaceholder(ctx context.Context, store secret.Store, stored, placeholder string) error {
	entries, err := store.Entries()
	if err != nil {
		return nil // the store failing here will fail the write with a better message
	}
	for _, e := range entries {
		if !e.Custom || e.Name == stored {
			continue
		}
		v, err := store.Lookup(ctx, e.Name)
		if err != nil {
			continue
		}
		if c, err := secret.DecodeCustom(v.Reveal()); err == nil && c.Placeholder == placeholder {
			return fmt.Errorf("the placeholder %q is already held by %s; two secrets cannot share one, "+
				"because the placeholder is how the proxy tells them apart", placeholder, e.Name)
		}
	}
	return nil
}

func validateCustomHosts(hosts []string) error {
	if len(hosts) == 0 {
		return usagef("at least one --host is required")
	}
	probe := secret.Custom{Hosts: hosts, Env: "X", Value: "x", Placeholder: "p"}
	_, _, err := secret.ParseInject(customInjectSpec("probe", probe))
	return err
}

func customInjectSpec(service string, c secret.Custom) string {
	return service + "@" + strings.Join(c.Hosts, ",") + "=substitute"
}

// customPlan adds the stored custom secrets that apply to a sandbox: an inject rule that
// substitutes the placeholder, and the guest variable that holds it. A secret stored for this
// sandbox replaces the machine-wide one of the same name, as everywhere else.
func customPlan(ctx context.Context, store secret.Store, entries []secret.Entry, sandbox string) (inject, guest, adopted []string, err error) {
	chosen := map[string]string{} // service → stored name
	for _, e := range entries {
		if !e.Custom {
			continue
		}
		scope, name := secret.SplitScopedName(e.Name)
		if scope != "" && scope != sandbox {
			continue
		}
		if prev, ok := chosen[name]; ok && prev != name {
			continue // already have this sandbox's own
		}
		chosen[name] = e.Name
	}
	for _, name := range sortedKeys(chosen) {
		v, lerr := store.Lookup(ctx, chosen[name])
		if lerr != nil {
			return nil, nil, nil, lerr
		}
		c, derr := secret.DecodeCustom(v.Reveal())
		if derr != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", chosen[name], derr)
		}
		inject = append(inject, customInjectSpec(name, c))
		if c.Env != "" {
			guest = append(guest, name+"="+c.Env+"="+c.Placeholder)
			adopted = append(adopted, "$"+c.Env+" (custom)")
		} else {
			// No variable: the guest already holds the placeholder some other way — a kit
			// sets GH_TOKEN to it — and the proxy only has to know what to swap.
			guest = append(guest, name+"="+c.Placeholder)
			adopted = append(adopted, c.Placeholder+" (custom)")
		}
	}
	return inject, guest, adopted, nil
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// resolveCustomValue turns a stored custom secret into the value the proxy substitutes.
func resolveCustomValue(ctx context.Context, stored string) (string, error) {
	c, err := secret.DecodeCustom(stored)
	if err != nil {
		return "", err
	}
	v, err := c.Resolve(ctx)
	if err != nil {
		return "", err
	}
	return v, nil
}
