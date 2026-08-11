// Package enforce puts a sandbox's network policy where the packets are.
//
// # Where the boundary actually is
//
// The enforcement point is the host-side network stack (internal/network), not the proxy
// and emphatically not the guest's environment. The stack terminates the guest's virtio-net
// link, so every frame the guest emits is handled by code on the host: a destination that
// no rule permits has nothing to answer it, whether the guest was cooperating or not.
//
// The filtering proxy listens **inside** that virtual network, on the gateway address, and
// the guest is told about it with HTTP_PROXY and friends. Those variables are a
// convenience, not the control: they let well-behaved clients get hostname-level filtering,
// credential injection and readable refusals instead of a connection that simply fails. A
// guest that ignores them does not escape anything — it loses the diagnostics and keeps the
// restrictions. **Never describe HTTP_PROXY as the boundary.** If the proxy were the only
// thing standing between a guest and the internet, a three-line Go program would be enough
// to walk past it.
//
// Nothing is bound on the host. The listener comes from the sandbox's own virtual network,
// so it is reachable from that one sandbox and nowhere else: not from the host, not from
// another sandbox, not from the LAN, and two sandboxes cannot collide on a port.
//
// # What has and has not been demonstrated
//
// The datapath here is exercised by tests with a fake guest attached to a real link socket
// (internal/network/vnettest): a real TCP handshake into the virtual network, a real HTTP
// client through the proxy, allow and deny both observed. What is **not** demonstrated
// anywhere in this repository is a real VM reaching this stack through libkrun's virtio-net
// device — that needs a hypervisor, and the machine this was written on has none. The
// transport was verified separately (see docs/architecture.md); the enforcement built on it
// has never met a guest.
package enforce

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/dagsommer/boks/internal/ca"
	"github.com/dagsommer/boks/internal/network"
	"github.com/dagsommer/boks/internal/policy"
	"github.com/dagsommer/boks/internal/proxy"
	"github.com/dagsommer/boks/internal/secret"
	"github.com/dagsommer/boks/internal/workspace"
)

const (
	// DefaultProxyPort is where the proxy listens on the gateway address inside a
	// sandbox's virtual network. It is a constant rather than an ephemeral port because
	// the value ends up in the container's recorded environment, and a sandbox that is
	// re-attached to tomorrow must still find its proxy where it was told to look.
	// Nothing else exists in that network, so there is nothing to collide with.
	DefaultProxyPort = 3128

	// GuestCADir is where a sandbox's copy of the CA certificate is mounted. It holds
	// the public half only; the signing key never leaves the host.
	GuestCADir = "/etc/boks"

	certFile   = "ca.pem"
	bundleFile = "ca-bundle.pem"
)

// Spec is everything one sandbox's network needs, in a form that survives being written to
// a pipe: the supervisor process that serves the link receives exactly this.
//
// Policy and credentials travel as the strings the user typed rather than as parsed
// structures, so that the CLI (which validates them and says which hosts will be decrypted)
// and the process that enforces them cannot end up with different rules.
type Spec struct {
	// Sandbox is the sandbox's name. It labels decisions in the log and names the
	// per-sandbox state directory.
	Sandbox string `json:"sandbox"`
	// Plan is the computed network, including the link socket path and the addressing.
	// It is passed rather than recomputed so both ends agree exactly.
	Plan network.Plan `json:"plan"`

	Preset string   `json:"preset,omitempty"`
	Allow  []string `json:"allow,omitempty"`
	Deny   []string `json:"deny,omitempty"`

	// Inject and GuestCredentials are the -inject and -guest-credential specs verbatim.
	Inject           []string `json:"inject,omitempty"`
	GuestCredentials []string `json:"guest_credentials,omitempty"`
	// Secrets maps a service to its resolved value.
	//
	// The values are resolved by the CLI, from the encrypted store, and handed over on a
	// pipe. The passphrase therefore never reaches a long-lived process and no secret
	// ever appears in a command line or an environment variable, where every other
	// process on the host could read it.
	Secrets map[string]string `json:"secrets,omitempty"`
	// Intercept permits terminating TLS for credential-bearing hosts. False means no CA
	// is opened and HTTPS credential rules never fire.
	Intercept bool `json:"intercept"`

	CADir    string `json:"ca_dir,omitempty"`
	StateDir string `json:"state_dir,omitempty"`
	LogPath  string `json:"log_path,omitempty"`
	// Address is the containerd socket. The supervisor uses it to watch the sandbox's
	// task, which is what bounds its own life.
	Address   string `json:"containerd_address,omitempty"`
	ProxyPort int    `json:"proxy_port,omitempty"`
}

// String is redacted, because a Spec holds secret values and error paths print structs.
// MarshalJSON deliberately does not redact: the whole purpose of the JSON form is to hand
// those values to the process that will attach them to requests.
func (s Spec) String() string {
	return fmt.Sprintf("enforce.Spec{sandbox:%s mode:%s socket:%s secrets:%d}",
		s.Sandbox, s.Plan.Mode, s.Plan.Socket, len(s.Secrets))
}

// GoString covers %#v, which would otherwise print every field.
func (s Spec) GoString() string { return s.String() }

// Policy resolves the preset and the per-run rules into the policy to enforce.
func (s Spec) Policy() (policy.Policy, error) {
	preset := s.Preset
	if preset == "" {
		preset = policy.DefaultPreset
	}
	return policy.Resolve(preset, s.Allow, s.Deny)
}

// Credentials assembles the credential rules.
func (s Spec) Credentials() ([]secret.Credential, error) {
	return secret.ParseCredentials(s.Inject, s.GuestCredentials)
}

// ProxyURL is what the guest is told to send HTTP through.
func (s Spec) ProxyURL() string {
	return "http://" + net.JoinHostPort(s.Plan.Gateway.String(), strconv.Itoa(s.proxyPort()))
}

func (s Spec) proxyPort() int {
	if s.ProxyPort > 0 {
		return s.ProxyPort
	}
	return DefaultProxyPort
}

// intercepts reports whether this sandbox will terminate TLS for anything.
func (s Spec) intercepts() bool { return s.Intercept && len(s.Inject) > 0 }

// certDir is the host directory shared into the guest as GuestCADir. It is per-sandbox and
// contains public certificates only.
func (s Spec) certDir() string {
	return filepath.Join(s.StateDir, "certs", sanitize(s.Sandbox))
}

// Guest is what a sandbox has to be created and started with for the network to reach it.
type Guest struct {
	// Annotations wire the VM's NIC and the container's interface. Without them the
	// runtime falls back to TSI, where the guest's 127.0.0.1 is the host's.
	Annotations map[string]string
	// Env tells cooperating clients where the proxy is and which CA to trust. It is a
	// convenience, never the control — see the package comment.
	Env []string
	// Mounts are extra host directories the sandbox needs, currently at most one: the
	// public half of the CA, read-only.
	Mounts []workspace.Workspace
}

// Prepare computes what the sandbox must be created with, and writes the guest-visible
// copy of the CA when a credential rule means TLS will be terminated.
//
// It is separate from Open because the two happen at different moments: a container is
// created with annotations and an environment long before, and possibly by a different
// process than, the one that serves its link.
func (s Spec) Prepare() (Guest, error) {
	g := Guest{Annotations: s.Plan.Annotations()}
	if s.Plan.Mode == network.ModeNone {
		// No proxy exists and no destination is reachable, so there is nothing true to
		// put in the environment. Setting HTTP_PROXY here would point every client at a
		// gateway that is not wired to the container.
		return g, nil
	}

	env := []string{
		"HTTP_PROXY=" + s.ProxyURL(),
		"HTTPS_PROXY=" + s.ProxyURL(),
		// Lowercase too: curl, git and most of the Unix world read these, and several
		// tools read only the lowercase form.
		"http_proxy=" + s.ProxyURL(),
		"https_proxy=" + s.ProxyURL(),
		"NO_PROXY=" + noProxy,
		"no_proxy=" + noProxy,
	}

	if s.intercepts() {
		authority, err := ca.OpenOrCreate(s.CADir)
		if err != nil {
			return Guest{}, err
		}
		certEnv, mount, err := s.writeGuestCA(authority)
		if err != nil {
			return Guest{}, err
		}
		env = append(env, certEnv...)
		g.Mounts = append(g.Mounts, mount)
	}

	g.Env = env
	return g, nil
}

// noProxy keeps a guest's own loopback out of the proxy. Everything else goes through it,
// including the guest's own subnet: there is nothing else in that subnet but the gateway.
const noProxy = "localhost,127.0.0.1,::1"

// writeGuestCA materialises the public half of the authority in a directory of its own and
// returns the environment that points runtimes at it.
//
// Two files, because the variables are not equivalent. NODE_EXTRA_CA_CERTS *adds* to Node's
// built-in roots, so it can safely name a file holding only the Boks CA. SSL_CERT_FILE,
// REQUESTS_CA_BUNDLE and CURL_CA_BUNDLE *replace* the trust store, so naming a Boks-only
// file there would break every destination Boks does not intercept — which is nearly all of
// them. Those three are set only when a public root store was found to bundle the Boks CA
// with, and are left alone otherwise.
func (s Spec) writeGuestCA(authority *ca.Authority) ([]string, workspace.Workspace, error) {
	dir := s.certDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, workspace.Workspace{}, fmt.Errorf("enforce: creating %s: %w", dir, err)
	}
	// 0644: this is the public half, and it is read inside the guest by whatever user the
	// image runs as. The signing key stays in the authority's own directory, owner-only,
	// and is never copied here.
	if err := os.WriteFile(filepath.Join(dir, certFile), authority.CertPEM(), 0o644); err != nil {
		return nil, workspace.Workspace{}, fmt.Errorf("enforce: writing the guest CA: %w", err)
	}

	env := []string{
		ca.CertEnvVar + "=" + authority.CertBase64(),
		"BOKS_CA_CERT=" + filepath.Join(GuestCADir, certFile),
		"NODE_EXTRA_CA_CERTS=" + filepath.Join(GuestCADir, certFile),
	}

	if roots, ok := hostRoots(); ok {
		bundle := append(append([]byte{}, roots...), authority.CertPEM()...)
		if err := os.WriteFile(filepath.Join(dir, bundleFile), bundle, 0o644); err != nil {
			return nil, workspace.Workspace{}, fmt.Errorf("enforce: writing the guest CA bundle: %w", err)
		}
		guestBundle := filepath.Join(GuestCADir, bundleFile)
		env = append(env,
			"SSL_CERT_FILE="+guestBundle,
			"REQUESTS_CA_BUNDLE="+guestBundle,
			"CURL_CA_BUNDLE="+guestBundle,
		)
	} else {
		// Leave a stale bundle from an earlier run out of the guest rather than
		// shipping roots nothing points at.
		_ = os.Remove(filepath.Join(dir, bundleFile))
	}

	return env, workspace.Workspace{
		HostPath:  dir,
		GuestPath: GuestCADir,
		Mode:      workspace.ModeReadOnly,
	}, nil
}

// hostRootBundles are the usual locations of a PEM root store. The list is the same one
// Go's own x509 package walks, which is why it covers the distributions it does.
var hostRootBundles = []string{
	"/etc/ssl/certs/ca-certificates.crt", // Debian, Ubuntu, Alpine
	"/etc/pki/tls/certs/ca-bundle.crt",   // Fedora, RHEL
	"/etc/ssl/ca-bundle.pem",             // openSUSE
	"/etc/ssl/cert.pem",                  // macOS, Alpine
}

func hostRoots() ([]byte, bool) {
	for _, path := range hostRootBundles {
		data, err := os.ReadFile(path)
		if err == nil && len(data) > 0 {
			return data, true
		}
	}
	return nil, false
}

// Session is a running network for one sandbox: the host-side stack, and the filtering
// proxy listening inside it.
//
// One Session serves exactly one sandbox, for exactly as long as that sandbox's VM is
// running. See supervisor.go for who owns that lifetime and why it is not the CLI.
type Session struct {
	spec     Spec
	net      *network.Network
	proxy    *proxy.Server
	listener net.Listener
	sink     *policy.FileSink
	served   chan error

	closeOnce sync.Once
	closeErr  error
}

// Open starts the host-side stack and, unless the sandbox has no network, the proxy inside
// it.
//
// It must return before the sandbox's task starts: the VM connects to the link socket while
// it boots, and a socket that appears late is a boot failure rather than a retry.
func Open(ctx context.Context, spec Spec, logger io.Writer) (*Session, error) {
	n, err := network.NewFromPlan(spec.Plan)
	if err != nil {
		return nil, err
	}
	n.SetLogger(logger)
	if err := n.Start(ctx); err != nil {
		return nil, err
	}
	s := &Session{spec: spec, net: n}

	if spec.Plan.Mode == network.ModeNone {
		// -net none: the link socket exists so the VM's NIC has somewhere to write, and
		// nothing else does. No stack, no proxy, no listener, no policy to evaluate —
		// the containment is the absent wiring, not a decision anything takes at
		// runtime.
		return s, nil
	}

	if err := s.startProxy(spec, logger); err != nil {
		// Undo the stack: a half-open session would leave a socket behind and a guest
		// with a link to nothing.
		_ = s.Close()
		return nil, err
	}
	return s, nil
}

func (s *Session) startProxy(spec Spec, logger io.Writer) error {
	pol, err := spec.Policy()
	if err != nil {
		return err
	}
	credentials, err := spec.Credentials()
	if err != nil {
		return err
	}

	var provider secret.Provider
	if len(credentials) > 0 {
		provider = secret.MapProvider(spec.Secrets)
	}
	injector, err := secret.NewInjector(provider, credentials...)
	if err != nil {
		return err
	}

	// The authority is opened only when a credential rule justifies it. A sandbox with no
	// injection configured never terminates anything and has no reason to touch a signing
	// key.
	var authority *ca.Authority
	if spec.intercepts() {
		if authority, err = ca.OpenOrCreate(spec.CADir); err != nil {
			return err
		}
	}

	decisions := policy.NewLog(policy.DefaultCapacity)
	if spec.LogPath != "" {
		sink, err := policy.NewFileSink(spec.LogPath)
		if err != nil {
			return err
		}
		s.sink = sink
		decisions.AddSink(sink)
	}

	srv, err := proxy.New(proxy.Config{
		Engine:   policy.NewEngine(pol, decisions).WithSandbox(spec.Sandbox),
		Injector: injector,
		CA:       authority,
		ErrorLog: log.New(orDiscard(logger), "boks net "+spec.Sandbox+": ", log.LstdFlags),
	})
	if err != nil {
		return err
	}
	s.proxy = srv

	l, err := s.net.Listen(spec.proxyPort())
	if err != nil {
		return err
	}
	s.listener = l

	s.served = make(chan error, 1)
	go func() { s.served <- srv.Serve(l) }()
	return nil
}

// ProxyURL is where the guest reaches the proxy, or "" for a sandbox with no network.
func (s *Session) ProxyURL() string {
	if s.spec.Plan.Mode == network.ModeNone {
		return ""
	}
	return s.spec.ProxyURL()
}

// Network exposes the stack, so a test can dial into the virtual network and a future port
// forwarder can reach a guest service.
func (s *Session) Network() *network.Network { return s.net }

// Close tears everything down: the proxy, the listener inside the virtual network, the
// stack, the link socket and its directory, and the decision-log sink.
//
// It is idempotent and safe on a half-open session, so it can sit in a defer next to every
// other piece of cleanup without a guard. A leaked socket or a stack still answering after
// this returns is a bug, not an untidiness: the next run of the same sandbox would fail to
// bind, and a guest could keep a network Boks thinks it has taken away.
func (s *Session) Close() error {
	s.closeOnce.Do(func() {
		var errs []error
		if s.proxy != nil {
			errs = append(errs, s.proxy.Close())
		}
		if s.listener != nil {
			// Serve closes the listener itself; closing twice is harmless and
			// covers the paths where Serve never started.
			if err := s.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if s.served != nil {
			<-s.served // the serving goroutine must be gone before we claim to be
		}
		if s.net != nil {
			errs = append(errs, s.net.Stop())
		}
		if s.sink != nil {
			errs = append(errs, s.sink.Close())
		}
		s.closeErr = errors.Join(errs...)
	})
	return s.closeErr
}

// sanitize keeps a sandbox name usable as a directory component.
func sanitize(name string) string {
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}

func orDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}
