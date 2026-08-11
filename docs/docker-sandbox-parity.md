# Docker Sandboxes → Boks parity matrix

Docker Sandboxes (`sbx`) is the behavioural reference for Boks. This document records what
Docker's product does, from its public documentation and from observing a live sandbox, and
what Boks intends to do about each behaviour.

Boks is an independent implementation. Nothing here is copied from Docker's source or docs;
descriptions are written from reading the public documentation and testing behaviour.

**Priority key** — `P0` needed for a useful first release · `P1` important parity ·
`P2` later · `Won't` not replicating.

**Status key** — `done` implemented and locally tested · `partial` implemented, incompletely
verified · `planned` designed, not built · `none` not started.

Last reviewed against Docker's docs: 2026-08-11.

---

## 1. Architecture and isolation

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Per-sandbox microVM | Each sandbox is a microVM with its own kernel; guest processes invisible to host | Same, one VM per sandbox via nerdbox + libkrun | P0 | done | Verified 2026-08-11: guest Linux 6.12.44 on a Darwin host, own boot_id and uptime |
| Hypervisor per platform | Apple Virtualization Framework on Apple silicon; KVM on Linux | libkrun: Hypervisor.framework on macOS, KVM on Linux | P0 | partial | macOS path verified; Linux/KVM path still unexercised |
| Guest OS | Ubuntu-based Linux image | Any OCI image; Debian/Ubuntu base for the agent image | P0 | partial | Boks takes an image reference, not a fixed distro; only alpine exercised so far |
| Root in guest | Agent has full control incl. `sudo` inside the VM | Same — the VM is the boundary, not in-guest permissions | P0 | done | Guest sees only its own processes; PID 1 is the sandboxed command |
| Resource sizing | Not documented in detail; root fs defaults to 20 GB | vCPU/memory via flags → nerdbox annotations | P1 | done | Verified: guest `nproc`/`MemTotal` track `-cpus`/`-memory` |
| Multiple containers per VM | Not exposed | Not planned; one VM per sandbox | P2 | none | nerdbox lists multi-container-per-VM as future work |

## 2. Lifecycle

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| `run` | Creates or re-attaches, then attaches to the agent | `boks run <ws> -- <cmd>`, persistent by default | P0 | partial | Re-attach implemented; exit codes propagate and Ctrl-C exits 128+signal; verified on the non-VM runtime only |
| Re-attach by workspace | Running again from the same workspace path reconnects instead of duplicating | Sandbox name derived from the workspace path digest | P1 | partial | No host state store: containerd's container record is the state |
| `create` | Builds in background without attaching | `boks create` | P1 | partial | Creates without starting; verified on the non-VM runtime |
| `start` / `stop` | Stop pauses; state survives | Same | P1 | partial | Stop kills the task, keeps container + snapshot; verified on the non-VM runtime |
| `rm` | Deletes sandbox and everything in it; `--force` for active | `boks rm`, `-f` | P1 | partial | Deletes the snapshot too; verified on the non-VM runtime |
| `ls` | Lists sandboxes with status and port mappings | `boks ls`, `-q`, `-json` | P1 | partial | No port mappings yet — none exist |
| `inspect` | Sandbox details | `boks inspect`, JSON output | P2 | partial | Verified on the non-VM runtime |
| `exec` | `sbx exec -it <name> bash` into a running sandbox | `boks exec [-i] [-t] [-it]` | P1 | partial | Streams IO, propagates the exit code; raw mode and resize unverified in a VM |
| Persistence across stop/start | Packages, Docker images, config, shell history persist until `rm` | Same, via the container's writable snapshot | P1 | partial | Confirmed across stop/start on the non-VM runtime |
| Naming | `--name`; reconnect from any directory | Same, `-name` | P1 | partial | An explicit name overrides the derived one |
| `reset` | Stops VMs, deletes data, can preserve secrets | `boks reset`, same secret-preserving default | P2 | none | |

## 2a. CLI surface — where Boks currently diverges

Boks grew command-first because that is what proved the runtime. sbx is **agent-first**, and
the difference shows up in almost every command. These are the gaps to close, kept separate
from the table above because they are shape mismatches rather than missing features.

| Aspect | Docker behavior | Boks today | Prio | Status |
|---|---|---|---|---|
| `run` argument order | `sbx run [flags] AGENT [workspace...]` — the agent is the first positional, workspaces follow, and the primary workspace defaults to the current directory | `boks run [flags] <workspace> -- <cmd>` — inverted, and a command is mandatory | P0 | none |
| Agents as a concept | A named set: Claude Code, Codex, Copilot, Cursor, Droid, Gemini, Kiro, OpenCode, Docker Agent, and **Shell**. Each selects an image and a startup command | No concept of an agent; `-image` plus an explicit command | P0 | none |
| Arbitrary commands | `Shell` is an agent, so a plain shell is reachable within the same grammar | The only mode there is | P1 | partial |
| Default sandbox name | `<agent>-<workspace directory name>` — confirmed against a live `sbx ls`. Case, dots and existing hyphens are preserved: `claude` in `~/git_repos/finndato.no` gives `claude-finndato.no`; a `udi-copilot-yolo` agent in `efm-integrasjonspunkt` gives `udi-copilot-yolo-efm-integrasjonspunkt` | `boks-<12 hex of a path digest>` — stable and collision-free, but unreadable and impossible to guess | P0 | none |
| `-name` without an agent | When the named sandbox already exists, the agent positional is optional and is read back from the sandbox | `-name` requires the full invocation | P1 | none |
| Bare `sbx` | Opens an interactive terminal dashboard: sandbox cards with live status, CPU and memory. `c` create, `s` start/stop, `Enter` attach, `x` shell, `r` remove, `tab` switches to the network governance panel, `?` lists shortcuts | Prints usage and exits 2 | P1 | none |
| `ls` columns | `SANDBOX  AGENT  STATUS  PORTS  WORKSPACE` — confirmed from a live run | name, status, image, workspace, age | P1 | partial |
| `ssh` | `sbx ssh` opens an SSH session into a sandbox | None | P2 | none |
| `daemon` | `sbx daemon start|stop` controls a background service | Boks has no daemon and needs none today; it drives containerd directly | P2 | none |

**Agents are user-extensible.** The same live listing shows agents named
`udi-copilot-default` and `udi-copilot-yolo` alongside the built-in `claude`. Custom agents
are therefore a real, in-use capability rather than a fixed menu, and since a kit is the
documented way to "define a new agent from scratch", agents and kits are almost certainly
the same mechanism seen from two directions. That raises the priority of kits from "later"
to "the thing that makes agents extensible", and means the agent registry Boks builds should
be data-driven from the start rather than a hardcoded switch.

**`forward` vs `forward-proxy` in the network log.** sbx's decision log distinguishes these
two, which is a strong hint about where it terminates TLS: a plain `forward` is a TCP tunnel
it cannot read, while `forward-proxy` is a flow it terminates and re-originates and therefore
*can* read and modify. That split is what makes header injection into HTTPS possible at all,
and Boks should reproduce the distinction explicitly — every logged flow saying whether it
was inspected or merely tunnelled. A user is entitled to know which of their traffic was
decrypted.

## 3. Workspace and filesystem

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Exact-path mounting | Workspace appears at the same absolute path as on host | Same | P0 | done | Verified in a VM, including deep paths; symlinks are resolved first, so `/tmp/x` becomes `/private/tmp/x` on macOS |
| Passthrough mechanism | virtiofs, caching on by default (`DOCKER_SANDBOXES_ENABLE_VIRTIOFS_CACHE=0` disables) | virtiofs via nerdbox | P0 | done | Cache tuning not exposed yet |
| Only workspace exposed | Parent directories are not shared | Same; parents exist as empty guest dirs | P0 | done | Verified: intermediate dirs auto-created, each holding only the next path component |
| Multiple workspaces | Extra paths mount alongside the primary one | `-mount` repeated | P1 | partial | Implemented; covered by an integration test but not yet run behind a VM |
| Read-only mounts | `path:ro` suffix | Same suffix | P1 | done | Verified: guest writes rejected, nothing reaches the host |
| Clone mode | `--clone` makes an in-VM Git clone; host repo read-only at `/run/sandbox/source`; fixed at creation | Equivalent planned; keep the read-only host mount idea | P1 | none | Good default for hostile code |
| Live host writes | Direct mode changes are immediately live on the host | Same, and documented as a real risk | P0 | done | See security-model.md |
| Large-repo slowness | `git status` etc. can be slow over passthrough | Expected; measure before optimizing | P2 | none | |
| `cp` to/from sandbox | `sbx cp` with `SANDBOX:PATH` on one side; no sandbox-to-sandbox | `boks cp`, same restriction | P2 | partial | Tar stream through `exec`; needs a running sandbox and `tar` in the image |
| Root disk size | 20 GB default | Configurable | P2 | none | |

## 4. Networking

**Where Boks stands (2026-08-11).** The policy engine, the host forward proxy and the
host-side network configuration are built and tested; **none of it is applied to a running
sandbox**, so Boks enforces nothing today. What changed this week is that the transport
enforcement needs was verified: attaching a virtio-net link to a host-side userspace stack
does displace libkrun's TSI, so a point at which Boks can drop a flow now demonstrably
exists. See [security-model.md](security-model.md#network).

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Enforcement point | Outside the guest — raw TCP/UDP/ICMP blocked at the network layer | Host-side userspace netstack (gvisor-tap-vsock, embedded as a library) | P0 | partial | Transport verified; nothing wired into `run`. `internal/network` builds the annotations and supervises the stack |
| No-network mode | Not offered as such | `-net none`: NIC on the VM, container not wired to it | P0 | partial | Costs nothing, turns TSI off, refuses host loopback. Strongest containment available |
| Egress via proxy | All outbound HTTP/HTTPS routed through a host proxy | Same | P0 | partial | `boks proxy` works standalone (`internal/proxy`); not wired into `run` |
| Default deny | Deny-by-default with an allowlist | Same | P0 | partial | Default preset `standard` is deny-by-default; asserted explicitly in the netstack config too |
| Policy presets | Open / Balanced / Locked Down, chosen at first run | `open` / `standard` / `locked` | P1 | done | `standard` is the default; entries justified in `internal/policy/preset.go` |
| Exact + wildcard hosts | Allowlist includes broad wildcards such as `*.googleapis.com` | Exact and wildcard, plus ports, IPs and CIDR | P0 | done | No multi-tenant wildcards in any preset — they allow every tenant's bucket |
| Deny precedence | Local deny rules still apply under org governance | Deny always wins over allow | P0 | done | Order- and specificity-independent; tested |
| Rule inspection | `sbx policy ls`, `sbx policy log` show rules and recent decisions | `boks policy ls` / `boks policy log` | P1 | done | Decisions recorded as JSON lines under the state dir; local only |
| Per-run allow flags | Policy configured per sandbox/host | `-allow`/`-deny` repeatable, `-policy <preset>`, `-net <mode>` | P0 | partial | Parsed and validated on `boks run`, which says plainly that they are not applied |
| TLS interception | Not used for filtering; HTTPS filtered without MITM | No MITM; filter on CONNECT host + SNI | P0 | done | Verified end to end: the client validates the origin's own chain through the proxy |
| SNI cross-check | Not documented | Deny a tunnel whose ClientHello names a forbidden host | P1 | done | Only possible after the `200`, so the client sees a broken handshake; the reason is in the log |
| Resolved-address recheck | Not documented | Deny rules re-applied to the address a permitted name resolved to | P1 | done | Stops `allowed.test A 127.0.0.1` from becoming a path to host services |
| Non-HTTP protocols | UDP and ICMP blocked and cannot be re-enabled by policy | Same initially | P1 | planned | Belongs to the netstack, which is not yet in the datapath |
| Hostname rules for non-HTTP | Don't work; IP addresses required | Same limitation, documented | P1 | done | Address and CIDR rules exist for exactly this |
| IPv6 | Not documented | Covered by the rule language from the start | P1 | partial | TSI had no v6; a real NIC does. No routable v6 is handed to the guest |
| DNS mediation | Not documented | Guest resolver pointed at the host-side gateway | P1 | partial | `io.containerd.nerdbox.ctr.dns`; the hook for name policy, not yet a closed channel |
| Host-local services | `127.0.0.1` and LAN services may be unreachable | Must become unreachable by default | P0 | partial | **Live regression vs Docker until the netstack is wired into `run`**: the default path is still TSI, where the guest's `127.0.0.1` is the host's. `-net none` and the presets close it; nothing applies them yet |
| Upstream proxy | Honors `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`; `DOCKER_SANDBOXES_PROXY` supports http/https/socks5/socks5h | Support an upstream proxy env var | P2 | none | socks5h delegates DNS to the proxy |
| Port publishing | `--publish` at creation; `sbx ports` add/remove later; ignored when re-attaching | `boks ports` equivalent | P1 | none | The netstack can forward, and Boks explicitly asserts it does not |
| Guest binding requirement | Service must listen on the VM's external interface, not just loopback | Same constraint | P1 | none | |

## 5. Credentials and secrets

**Where Boks stands.** `internal/secret` implements the model and `boks proxy` applies it;
`boks secret set/ls/rm` manages an encrypted host-side store. It is not wired into
`boks run`, and injection is limited to plaintext HTTP because Boks will not terminate TLS
silently.

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Injection model | Real value stays on host; proxy injects auth headers into approved outbound requests | Same principle, vendor-neutral | P0 | partial | Works through `boks proxy`; not wired into `run` |
| Schemes | Not documented in detail | `bearer`, `basic`, arbitrary `header` | P0 | done | Covers GitHub, Anthropic and registries with no vendor-specific code |
| HTTPS injection | Supported | **Not possible without terminating TLS, so not done** | P0 | none | Docker must be terminating TLS somewhere. Boks treats interception as an explicit per-host decision, not a default |
| Secret storage | OS keychain; Linux uses desktop keyring or an encrypted file | Encrypted local file first, keychains later | P1 | partial | AES-256-GCM, PBKDF2-HMAC-SHA256 over `BOKS_SECRETS_PASSPHRASE`; names encrypted too |
| Keychain providers | macOS Keychain, Secret Service, Credential Manager | Same, behind the `Provider` interface | P1 | none | Interface exists; no implementation |
| Scope | Injection tied to configured destinations | Only for explicitly configured hosts | P0 | done | A catch-all `*` destination is rejected outright |
| Placeholder replacement | Guest holds a placeholder | Existing header is overwritten, never appended | P0 | done | A surviving placeholder would be a silent auth failure at best |
| Guest secret access | Guest never receives raw values | Same; no host API for the guest to query | P0 | done | The store's only consumer is the proxy's request path. Adding a lookup endpoint would end the guarantee |
| Never logged | Not documented | Values redacted in every printed and serialised form | P1 | done | Enforced by the `Value` type and asserted by tests |
| `secret set` | `sbx secret set <sandbox> <name> -t <value>`; global variant | `boks secret set` | P1 | done | Reads from stdin by default; `-value` documented as visible in the process list |
| Git/GitHub credentials | Injected transparently for HTTPS Git; `gh` CLI shows logged-out but pushes work | Same approach | P1 | none | Needs HTTPS injection, hence TLS termination — blocked on that decision |
| SSH agent forwarding | Supported; SSH key signing works, GPG/S-MIME do not | Host agent socket forwarding | P2 | none | |
| OAuth device login | Agent login inside sandbox; session tokens stay on host | Out of scope near-term | P2 | none | |

## 6. Docker inside the sandbox

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Private daemon | Each sandbox runs its own Docker daemon | Same — dockerd inside the guest | P1 | none | |
| Host socket | Never exposed to the guest | Never | P0 | done | Boks does not mount any host socket |
| Image cache sharing | Not shared between sandboxes | Not shared | P1 | none | |
| Separate data disk | `/var/lib/docker` on its own disk | Same idea | P2 | none | |
| `docker build` / `compose` | Work inside the sandbox | Target once nested Docker lands | P1 | none | |

## 7. Configuration and kits

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Kits | Declarative YAML applied at creation: install commands, files, network rules, credential rules; can define a new agent or mix into an existing one | Boks equivalent, schema derived from our own runtime | P2 | none | Docker labels kits experimental and subject to change |
| Templates | Base image a kit builds on (`sandbox.image`) | Base image reference | P2 | none | |
| Mixin composition | Kits layer onto agent runs | Layering wanted; semantics TBD | P2 | none | |
| Distribution | Kit install restricted to an allowlist, Docker Hub by default | Local files first; no registry near-term | P2 | none | Avoid a premature kit registry |
| Env vars in guest | User-level agent config does not transfer; custom env via a persistent shell file | Explicit `--env` and config, no implicit host inheritance | P1 | none | Not inheriting host env is a security win |

## 8. Agents and integrations

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Bundled agents | Named agents (e.g. `sbx run claude`) with prepared images | Boks runs any command; agent images later | P1 | none | Boks is agent-agnostic by design |
| MCP gateway | Single endpoint per sandbox, brokered on the **host** side | Not near-term | P2 | none | Note: local MCP servers run outside the VM |
| Shared skills store | Host-side store mounted read-write; one sandbox can affect another | Deliberately **not** replicating the shared mutable store | P2 | Won't | Docker documents this as a cross-sandbox trust issue |
| IDE integrations | Various | Out of scope | P2 | Won't | |

## 9. Governance, accounts, telemetry

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Account requirement | Free Docker account required; sign-in ties sandboxes to a person | **Never required** | — | done | Core Boks principle |
| Telemetry | CLI reports command name, success, duration, username; `SBX_NO_TELEMETRY=1` opts out | **None, ever** — nothing to opt out of | — | done | No network calls except those you ask for |
| Org governance | Central network/filesystem/MCP policy, audit logs, sign-in enforcement; paid tier | Local policy files only; no central control plane | — | Won't | Would require an account and a server |
| Audit log | Governance audit records | Local decision log (`policy log`) | P2 | planned | Local, not uploaded |
| Filesystem policy | Admins restrict which host paths may be mounted | Local config could express this | P2 | none | |

## 10. Platform support

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| macOS | Sonoma 14+, Apple silicon only | Supported; **the verified platform** | P1 | done | Needs the shim codesigned with `com.apple.security.hypervisor` and a user-owned `/var/run/containerd` |
| Linux | Ubuntu 24.04+, x86_64/aarch64, KVM required, user in `kvm` group, nested virt if in a VM | Supported, same requirements | P0 | partial | `doctor` checks these, but no VM has been booted on Linux yet |
| Windows | Windows 11 x86_64, Hypervisor Platform enabled | Blocked on nerdbox | P2 | none | nerdbox lists Windows as future work |
| Docker Desktop | Not required | Not required | — | done | |

---

## Answered

1. **Exact-path mounting for non-existent guest parents.** *(2026-08-11)* The runtime creates
   them. `boks run /private/tmp/probe/deep/a/b/c/project -- pwd` printed that exact path, and
   each intermediate directory contained only the next component. Boks does not need to
   pre-create anything. One nuance worth documenting for users: Boks resolves symlinks when
   parsing a workspace, so `/tmp/x` becomes `/private/tmp/x` on macOS — correct, but not
   literally the string typed.
2. **TSI vs external network provider.** *(2026-08-11)* Settled, and not on the axis it was
   framed on. TSI is not merely "simpler but less controllable": because libkrun performs the
   guest's connections on the host, the guest's `127.0.0.1` **is the host's**, verified
   against a live host service. That is a containment failure, not an ergonomics trade-off,
   so an external network provider is required rather than preferred.
3. **Docker's enforcement mechanics.** Still inference, but better supported: Docker
   documents that hostname rules do not work for non-HTTP connections and that IP addresses
   are required there, which is what a host-side netstack plus an HTTP-aware proxy would
   produce. Boks is designing to the same shape.

## Open questions

1. **Enforcement mechanics.** Docker states raw TCP/UDP/ICMP are blocked at the network
   layer, but not how. A host userspace netstack is the natural fit and what Boks plans;
   this is inference, not documented fact.
2. **Exact-path mounting for non-existent guest parents.** Whether the guest runtime creates
   `/home/alice/src` implicitly for the mount point, or whether Boks must pre-create it, is
   unverified pending a working VM.
3. ~~**Re-attach identity.**~~ **Answered.** Both, with the path as the default. A sandbox's
   containerd identifier is `boks-<first 12 hex of sha256(workspace host path)>` unless
   `-name` is given, in which case that name is used verbatim. The path digest is what makes
   `boks run .` twice from one directory reach one sandbox — the path is the only thing two
   invocations share, and it cannot be the identifier itself because containerd identifiers
   allow neither slashes nor 76+ characters. An explicit name is what lets one workspace hold
   several sandboxes and lets a sandbox be reached from any directory, matching `sbx --name`.
   The readable path is kept in a container label, so `boks ls` shows it.

   Consequences worth knowing: renaming or moving a workspace directory orphans its sandbox
   (it is still listed, still removable, but a new one is created for the new path), and a
   symlinked workspace resolves to its target before hashing, so two paths to the same
   directory share one sandbox.
4. **Kit schema.** Deliberately unanswered until real runtime features exist to configure.
5. ~~**TSI vs external network provider.**~~ **Answered 2026-08-11.** The external provider
   displaces TSI: with it attached the guest has `eth0` and a host loopback probe that TSI
   answered is refused. Two annotations are needed (`io.containerd.nerdbox.network.N` for
   the VM, `io.containerd.nerdbox.ctr.network.N` keyed by `vmmac` for the container), `addr`
   must be CIDR whatever nerdbox's docs show, and one stack serves exactly one VM. What
   remains open is the enforcement built on top: no policy has been applied to a real guest.
