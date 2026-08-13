package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/dagsommer/boks/internal/enforce"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/ports"
)

// newPortsCommand is `boks ports`, sbx's spelling and sbx's flags.
//
// The command exists because a sandbox you cannot reach is a sandbox you cannot develop in:
// a dev server inside the VM answers on the guest's own interface, and nothing on the host
// can open a connection to it without a forwarder.
//
// A second motivation is often claimed for this feature and is worth stating carefully,
// because this repository has since measured it. An OAuth flow whose redirect is a
// `127.0.0.1` listener cannot work in a sandbox without a published port: the browser is on
// the host and the listener is in the guest. That is true of the *shape*. It is not true of
// the login Boks actually has to support — Claude Code 2.1.228 was driven headless and uses
// paste-a-code, with a vendor-hosted redirect and no loopback callback in the binary at all
// (see docs/docker-sandbox-parity.md). So this command is for dev servers first, and for a
// loopback-redirect login only if one turns up.
func newPortsCommand(env Env) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ports SANDBOX [flags]",
		Short: "List, publish and unpublish a sandbox's ports",
		Long: `Publishes a port inside a sandbox on the host, and lists what is published.

  --publish     [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]
  --unpublish   [HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL]

If HOST_PORT is omitted, an ephemeral port is allocated automatically. If HOST_IP is omitted,
the port is bound on loopback, expanded based on PROTOCOL and the sandbox's address families:
tcp/udp binds both 127.0.0.1 and ::1 (or only 127.0.0.1 if the sandbox is IPv4-only); tcp4/udp4
binds only 127.0.0.1; tcp6/udp6 binds only ::1. PROTOCOL defaults to tcp. Supported protocols:
tcp, tcp4, tcp6, udp, udp4, udp6.

A boks sandbox's virtual network is IPv4-only, so the default binds 127.0.0.1 alone.

Binding loopback rather than every interface is the point, not a limitation. A published port
is a hole from this host into a VM running code you have not audited; on 0.0.0.0 it would be a
hole from the local network into it. Name a HOST_IP only when you mean it.

The service inside the sandbox must listen on the VM's external interface — bind 0.0.0.0 or
::, not only 127.0.0.1 — or there is nothing on the far end of the forward.

Unpublishes are applied before publishes, so one invocation can move a port.`,
		Example: `  boks ports web --publish 3000            # an ephemeral host port -> 3000 in the sandbox
  boks ports web --publish 8080:3000       # 127.0.0.1:8080 -> 3000
  boks ports web --publish 127.0.0.1:8080:3000/tcp
  boks ports web                           # what is published now
  boks ports web --json
  boks ports web --unpublish 8080:3000`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return usagef("a sandbox name is required; run 'boks ls' to see what you can publish from")
			}
			if len(args) > 1 {
				return usagef("ports takes one sandbox; got %d", len(args))
			}
			return nil
		},
	}

	var (
		publish   []string
		unpublish []string
		asJSON    bool
	)
	// StringArray, not StringSlice: a StringSlice splits on commas, and while no
	// specification contains one today, silently turning one argument into two is the
	// mistake this project already made once with policy rules.
	cmd.Flags().StringArrayVar(&publish, "publish", nil,
		"Publish a port (can be repeated): [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]")
	cmd.Flags().StringArrayVar(&unpublish, "unpublish", nil,
		"Unpublish a port (can be repeated): [HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL]")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Output in JSON format (for port listing)")

	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		name := args[0]
		// Parse before connecting to anything: a typo should cost a message, not a
		// half-applied change to a running sandbox.
		if err := checkPublishSpecs(publish); err != nil {
			return err
		}
		for _, spec := range unpublish {
			if _, err := ports.ParseUnpublish(spec); err != nil {
				return err
			}
		}

		ctx := cmd.Context()
		stateDir := policy.StateDir()

		// Unpublish first, so that `--unpublish 8080:3000 --publish 8080:4000` moves a
		// port rather than colliding with itself.
		if len(unpublish) > 0 {
			_, removed, err := enforce.Unpublish(ctx, stateDir, name, unpublish)
			if err != nil {
				return portsError(name, err)
			}
			for _, p := range removed {
				fmt.Fprintf(env.Stderr, "unpublished %s\n", p)
			}
		}
		if len(publish) > 0 {
			_, added, err := enforce.Publish(ctx, stateDir, name, publish)
			if err != nil {
				return portsError(name, err)
			}
			for _, p := range added {
				fmt.Fprintf(env.Stderr, "published %s\n", p)
			}
		}

		list, err := enforce.Ports(ctx, stateDir, name)
		if err != nil {
			return portsError(name, err)
		}
		if asJSON {
			return writeJSON(env.Stdout, listOrEmpty(list))
		}
		writePortsTable(env.Stdout, env.Stderr, list)
		return nil
	}
	return cmd
}

// checkPublishSpecs validates every --publish argument, and turns the one refusal that is not
// a typo into an explanation.
func checkPublishSpecs(specs []string) error {
	for _, text := range specs {
		spec, err := ports.ParsePublish(text)
		if err != nil {
			return err
		}
		if spec.Protocol.IsUDP() {
			return fmt.Errorf("%q: %w", text, ports.ErrUDPNotCarried)
		}
	}
	return nil
}

// portsError turns "there is no supervisor" into the sentence that says what to do about it.
// Every other failure is the supervisor's own message, which already names the port.
func portsError(name string, err error) error {
	if errors.Is(err, enforce.ErrNoSupervisor) {
		return fmt.Errorf("sandbox %q has no running network stack, so it has no ports to publish.\n"+
			"Start it first: boks start %s", name, name)
	}
	return err
}

// listOrEmpty keeps --json emitting `[]` rather than `null` for a sandbox with nothing
// published, so that a script can iterate the result without a nil check.
func listOrEmpty(list []ports.Published) []ports.Published {
	if list == nil {
		return []ports.Published{}
	}
	return list
}

func writePortsTable(stdout, stderr io.Writer, list []ports.Published) {
	if len(list) == 0 {
		fmt.Fprintln(stderr, "no ports are published for this sandbox")
		return
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "HOST\tSANDBOX\tPROTOCOL")
	for _, p := range list {
		fmt.Fprintf(w, "%s\t%d\t%s\n", hostAddress(p), p.SandboxPort, p.Protocol)
	}
	_ = w.Flush()

	// A port whose last connection did not reach the guest is the common failure, and it
	// is almost never a boks fault: the service is bound to the guest's own loopback. It
	// goes to stderr so that the table stays parseable.
	for _, p := range list {
		if p.LastError != "" {
			fmt.Fprintf(stderr, "\n%s: %s\n", p, p.LastError)
		}
	}
}

func hostAddress(p ports.Published) string {
	if strings.Contains(p.HostIP, ":") {
		return fmt.Sprintf("[%s]:%d", p.HostIP, p.HostPort)
	}
	return fmt.Sprintf("%s:%d", p.HostIP, p.HostPort)
}

// portsColumn renders the PORTS column of `boks ls`, in Docker's and sbx's notation.
func portsColumn(list []ports.Published) string {
	if len(list) == 0 {
		return ""
	}
	out := make([]string, 0, len(list))
	for _, p := range list {
		out = append(out, p.String())
	}
	return strings.Join(out, ", ")
}
