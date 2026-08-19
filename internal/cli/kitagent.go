package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dagsommer/boks/internal/agent"
	"github.com/dagsommer/boks/internal/kit"
	"github.com/dagsommer/boks/internal/secret"
)

// agentFromKit turns a `kind: sandbox` kit into an agent Boks can run.
//
// # Why this is the whole feature
//
// A kit of that kind names an image and an entrypoint, which is exactly what an agent is —
// `boks run udi-copilot-default --kit …` failed with "does not exist" and a list of built-in
// agents, because the agent it named was defined in the file being passed on the same command
// line. Registering the kit's agent before the positional arguments are interpreted makes
// every downstream mechanism work with no further changes: the name resolves, the sandbox is
// named after it, `boks policy ls` labels its layer, and a sandbox that already exists
// remembers which agent it runs.
//
// # Init is deliberately empty
//
// A Boks agent normally runs behind `/usr/bin/tini -s -- /usr/local/bin/boks-entrypoint`,
// which exists because Boks' own images put it there. A kit names someone else's image —
// uditemplates.azurecr.io/… in the case this was written for — and neither path is in it, so
// inheriting the prefix would produce a container that cannot start, failing on a path that
// has nothing to do with the kit.
//
// The cost is stated rather than hidden: a kit's agent gets no tini, so nothing reaps orphans
// inside it, and no boks-entrypoint, so the Boks CA is not installed into that image's trust
// store. An image that wants either can install them and set its own entrypoint. This is the
// same trade Agent.Bare() makes for `-t/--template`, and for the same reason.
func agentFromKit(spec *kit.Spec, interactive bool) (agent.Agent, error) {
	if spec == nil || spec.Kind != kit.KindSandbox {
		return agent.Agent{}, fmt.Errorf("only a kind: sandbox kit defines an agent")
	}
	if spec.Sandbox == nil || spec.Sandbox.Image == "" {
		return agent.Agent{}, fmt.Errorf("kit %q declares kind: sandbox but names no image", spec.Name)
	}

	a := agent.Agent{
		Name:    spec.Name,
		Summary: kitSummary(spec),
		Image:   spec.Sandbox.Image,
		// No Init: see above.
		Command: kitCommand(spec, interactive),
		Env:     kitEnv(spec),
	}
	return a, nil
}

// kitSummary is what `boks run --help` lists beside the agent's name.
func kitSummary(spec *kit.Spec) string {
	switch {
	case spec.DisplayName != "":
		return spec.DisplayName
	case spec.Description != "":
		return spec.Description
	default:
		return "from kit " + spec.Name
	}
}

// kitCommand is the argv the guest runs: the entrypoint followed by the mode's argument tail.
//
// The interactive tail is used when there is a terminal, which is the distinction
// sandbox.command.default and .interactive exist to draw. `boks run` allocates a pty exactly
// when both streams are terminals, so the same test decides both — a command chosen for a TTY
// that then does not get one would be the worse half of getting this wrong.
func kitCommand(spec *kit.Spec, interactive bool) []string {
	argv := append([]string{}, spec.Sandbox.Entrypoint...)
	tail := spec.Sandbox.Command.Default
	if interactive && len(spec.Sandbox.Command.Interactive) > 0 {
		tail = spec.Sandbox.Command.Interactive
	}
	return append(argv, tail...)
}

// kitEnv renders environment.variables in the KEY=VALUE form an agent carries.
//
// Sorted, because a map's iteration order is random and an agent definition that differs
// between two runs of the same kit would make a sandbox's recorded spec differ for no reason.
func kitEnv(spec *kit.Spec) []string {
	if spec.Environment == nil || len(spec.Environment.Variables) == 0 {
		return nil
	}
	keys := make([]string, 0, len(spec.Environment.Variables))
	for k := range spec.Environment.Variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	env := make([]string, 0, len(keys))
	for _, k := range keys {
		env = append(env, k+"="+spec.Environment.Variables[k])
	}
	return env
}

// registerKitAgent adds a kit's agent to the registry, if the kit defines one.
//
// A kit that is a mixin, or that carries no sandbox block, adds nothing and is not an error:
// `--kit` is also how a mixin's network rules are applied, and most published kits are mixins.
//
// A name that collides with a built-in is REFUSED rather than allowed to win or silently lose.
// Either behaviour would be a trap: shadowing means `boks run claude --kit x` quietly runs
// something else, and losing means a kit that names its agent `claude` is ignored with no
// indication. The user can rename the kit's agent, and nobody can do that for them.
func registerKitAgent(agents *agent.Registry, spec *kit.Spec, interactive bool) error {
	if spec == nil || spec.Kind != kit.KindSandbox {
		return nil
	}
	if agents.Known(spec.Name) {
		return fmt.Errorf("kit %q defines an agent called %q, and that is already a built-in "+
			"agent. Rename the kit's agent so the two can be told apart", spec.Name, spec.Name)
	}
	a, err := agentFromKit(spec, interactive)
	if err != nil {
		return err
	}
	return agents.Add(a)
}

// kitInjectSpecs renders a kit's credentials[] as --inject specifications.
//
// Rendering into the flag's own grammar rather than building secret.Credential valuesis
// the point: everything that validates, de-duplicates, describes and enforces an injection
// rule already works on that form, and a second path into the same machinery is a second place
// for the rules to be judged differently. A kit's credential is exactly what --inject
// expresses — attach this stored secret to these hosts, in this header, in this format — so it
// is expressed that way.
//
// The service name is the key in Boks' own store: a kit declaring `ghe-token` is asking for
// whatever `boks secret set ghe-token` put there, which is also what makes a kit shareable.
// It carries the rule, not the secret.
func kitInjectSpecs(spec *kit.Spec) ([]string, error) {
	if spec == nil {
		return nil, nil
	}
	var specs []string
	for _, c := range spec.Credentials {
		if c.APIKey == nil || len(c.APIKey.Inject) == 0 {
			// An oauth credential, or one that attaches to nothing. Skipped rather
			// than half-applied; docs/kits.md lists what is not wired.
			continue
		}
		// One --inject per attachment FORM, with its hosts gathered. A kit may attach
		// the same credential to different hosts with different headers, and the flag
		// grammar carries one form per spec.
		byForm := map[string][]string{}
		var order []string
		for _, in := range c.APIKey.Inject {
			form, err := kitAttachForm(in)
			if err != nil {
				return nil, fmt.Errorf("kit %q, credential %q: %w", spec.Name, c.Service, err)
			}
			if _, seen := byForm[form]; !seen {
				order = append(order, form)
			}
			byForm[form] = append(byForm[form], in.Domain)
		}
		for _, form := range order {
			specs = append(specs, fmt.Sprintf("%s@%s=%s",
				c.Service, strings.Join(byForm[form], ","), form))
		}
	}
	return specs, nil
}

// kitAttachForm renders one injection's header and format in --inject's grammar.
//
// The grammar's three forms are `bearer`, `basic[:user]` and `header[:format]`, and the last
// is the general one a kit's headerName/valueFormat maps onto. A format containing a colon is
// fine — ParseInject cuts on the FIRST colon, so `Authorization:Bearer %s` splits into the
// header and the whole remainder.
func kitAttachForm(in kit.APIKeyInject) (string, error) {
	if in.Scheme != "" {
		if in.Username != "" {
			return string(in.Scheme) + ":" + in.Username, nil
		}
		return string(in.Scheme), nil
	}
	header := in.Header
	if header == "" {
		header = secret.DefaultHeader
	}
	if strings.Contains(header, ":") {
		// A header name containing a colon would be split in the wrong place, and the
		// result would attach the credential under a header nobody named.
		return "", fmt.Errorf("header %q contains a colon", header)
	}
	if in.Format == "" {
		return header, nil
	}
	if !strings.Contains(in.Format, "%s") {
		return "", fmt.Errorf("format %q has no %%s for the secret", in.Format)
	}
	return header + ":" + in.Format, nil
}
