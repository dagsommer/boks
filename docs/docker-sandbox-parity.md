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
| Per-sandbox microVM | Each sandbox is a microVM with its own kernel; guest processes invisible to host | Same, one VM per sandbox via nerdbox + libkrun | P0 | partial | Orchestration built; VM boot unverified on our hardware |
| Hypervisor per platform | Apple Virtualization Framework on Apple silicon; KVM on Linux | libkrun: Hypervisor.framework on macOS, KVM on Linux | P0 | partial | Same underlying OS facilities, different VMM |
| Guest OS | Ubuntu-based Linux image | Any OCI image; Debian/Ubuntu base for the agent image | P0 | partial | Boks takes an image reference, not a fixed distro |
| Root in guest | Agent has full control incl. `sudo` inside the VM | Same — the VM is the boundary, not in-guest permissions | P0 | partial | |
| Resource sizing | Not documented in detail; root fs defaults to 20 GB | vCPU/memory via flags → nerdbox annotations | P1 | partial | `io.containerd.nerdbox.resources.{cpu,memory}` |
| Multiple containers per VM | Not exposed | Not planned; one VM per sandbox | P2 | none | nerdbox lists multi-container-per-VM as future work |

## 2. Lifecycle

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| `run` | Creates or re-attaches, then attaches to the agent | `boks run <ws> -- <cmd>`; ephemeral first | P0 | done | Re-attach semantics deferred |
| Re-attach by workspace | Running again from the same workspace path reconnects instead of duplicating | Planned once sandboxes are named/persistent | P1 | none | Requires host state store |
| `create` | Builds in background without attaching | `boks create` | P1 | none | |
| `start` / `stop` | Stop pauses; state survives | Same | P1 | none | Needs persistent snapshot |
| `rm` | Deletes sandbox and everything in it; `--force` for active | `boks rm` | P1 | none | |
| `ls` | Lists sandboxes with status and port mappings | `boks ls` | P1 | none | |
| `inspect` | Sandbox details | `boks inspect`, JSON output | P2 | none | |
| `exec` | `sbx exec -it <name> bash` into a running sandbox | `boks exec` | P1 | none | Needs long-lived sandboxes first |
| Persistence across stop/start | Packages, Docker images, config, shell history persist until `rm` | Same, via writable snapshot | P1 | none | Boks is ephemeral-only today |
| Naming | `--name`; reconnect from any directory | Same | P1 | none | |
| Dashboard TUI | Interactive dashboard with live CPU/memory, shells, attach | Not planned near-term | P2 | none | Nice UX, not a correctness feature |
| `reset` | Stops VMs, deletes data, can preserve secrets | `boks reset` | P2 | none | |

## 3. Workspace and filesystem

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Exact-path mounting | Workspace appears at the same absolute path as on host | Same | P0 | partial | Implemented via OCI mount destination; unverified in VM |
| Passthrough mechanism | virtiofs, caching on by default (`DOCKER_SANDBOXES_ENABLE_VIRTIOFS_CACHE=0` disables) | virtiofs via nerdbox | P0 | partial | Cache tuning not exposed yet |
| Only workspace exposed | Parent directories are not shared | Same; parents exist as empty guest dirs | P0 | partial | nerdbox shares only the named directory |
| Multiple workspaces | Extra paths mount alongside the primary one | `--mount` repeated | P1 | none | |
| Read-only mounts | `path:ro` suffix | Same suffix | P1 | partial | Spec supports `ro`; CLI parsing pending |
| Clone mode | `--clone` makes an in-VM Git clone; host repo read-only at `/run/sandbox/source`; fixed at creation | Equivalent planned; keep the read-only host mount idea | P1 | none | Good default for hostile code |
| Live host writes | Direct mode changes are immediately live on the host | Same, and documented as a real risk | P0 | done | See security-model.md |
| Large-repo slowness | `git status` etc. can be slow over passthrough | Expected; measure before optimizing | P2 | none | |
| `cp` to/from sandbox | `sbx cp` with `SANDBOX:PATH` on one side; no sandbox-to-sandbox | `boks cp`, same restriction | P2 | none | |
| Root disk size | 20 GB default | Configurable | P2 | none | |

## 4. Networking

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Enforcement point | Outside the guest — raw TCP/UDP/ICMP blocked at the network layer | Host-side userspace netstack (gvisor-tap-vsock) | P0 | planned | Env vars alone are not a boundary |
| Egress via proxy | All outbound HTTP/HTTPS routed through a host proxy | Same | P0 | planned | |
| Default deny | Deny-by-default with an allowlist | Same | P0 | planned | |
| Policy presets | Open / Balanced / Locked Down, chosen at first run | Equivalent presets, names TBD | P1 | none | Balanced is Docker's recommended start |
| Exact + wildcard hosts | Allowlist includes broad wildcards such as `*.googleapis.com` | Exact and wildcard, plus ports | P0 | planned | Docker's own docs flag broad defaults as a risk |
| Deny precedence | Local deny rules still apply under org governance | Deny always wins over allow | P0 | planned | |
| Rule inspection | `sbx policy ls`, `sbx policy log` show rules and recent decisions | `boks policy ls` / `log` | P1 | none | Observability is what makes policy usable |
| Per-run allow flags | Policy configured per sandbox/host | `--allow host` repeatable | P0 | planned | |
| TLS interception | Not used for filtering; HTTPS filtered without MITM | No MITM; filter on CONNECT host + SNI | P0 | planned | Avoids a custom CA in the guest |
| Non-HTTP protocols | UDP and ICMP blocked and cannot be re-enabled by policy | Same initially | P1 | planned | Simplifies the enforcement surface |
| Hostname rules for non-HTTP | Don't work; IP addresses required | Same limitation, documented | P1 | planned | Inherent: no hostname on a raw socket |
| Host-local services | `127.0.0.1` and LAN services may be unreachable | Same; explicit opt-in later | P2 | none | |
| Upstream proxy | Honors `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`; `DOCKER_SANDBOXES_PROXY` supports http/https/socks5/socks5h | Support an upstream proxy env var | P2 | none | socks5h delegates DNS to the proxy |
| Port publishing | `--publish` at creation; `sbx ports` add/remove later; ignored when re-attaching | `boks ports` equivalent | P1 | none | |
| Guest binding requirement | Service must listen on the VM's external interface, not just loopback | Same constraint | P1 | none | |

## 5. Credentials and secrets

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Injection model | Real value stays on host; proxy injects auth headers into approved outbound requests | Same principle, vendor-neutral | P0 | planned | Guest holds a placeholder only |
| Secret storage | OS keychain; Linux uses desktop keyring or an encrypted file | Encrypted local file first, keychains later | P1 | planned | No account, no cloud |
| Scope | Injection tied to configured destinations | Only for explicitly configured hosts | P0 | planned | |
| Guest secret access | Guest never receives raw values | Same; no host API for the guest to query | P0 | planned | |
| `secret set` | `sbx secret set <sandbox> <name> -t <value>`; global variant | `boks secret set` | P1 | none | |
| Git/GitHub credentials | Injected transparently for HTTPS Git; `gh` CLI shows logged-out but pushes work | Same approach | P1 | planned | Observed behaviour, documented by Docker |
| SSH agent forwarding | Supported; SSH key signing works, GPG/S-MIME do not | P2 | none | | Host agent socket forwarding |
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
| macOS | Sonoma 14+, Apple silicon only | Supported target, second | P1 | none | libkrun + nerdbox support macOS |
| Linux | Ubuntu 24.04+, x86_64/aarch64, KVM required, user in `kvm` group, nested virt if in a VM | First target, same requirements | P0 | partial | `doctor` checks these |
| Windows | Windows 11 x86_64, Hypervisor Platform enabled | Blocked on nerdbox | P2 | none | nerdbox lists Windows as future work |
| Docker Desktop | Not required | Not required | — | done | |

---

## Open questions

1. **Enforcement mechanics.** Docker states raw TCP/UDP/ICMP are blocked at the network
   layer, but not how. A host userspace netstack is the natural fit and what Boks plans;
   this is inference, not documented fact.
2. **Exact-path mounting for non-existent guest parents.** Whether the guest runtime creates
   `/home/alice/src` implicitly for the mount point, or whether Boks must pre-create it, is
   unverified pending a working VM.
3. **Re-attach identity.** Docker keys sandbox reuse on workspace path. Boks needs to decide
   between path-hash identity and explicit names before building persistence.
4. **Kit schema.** Deliberately unanswered until real runtime features exist to configure.
5. **TSI vs external network provider.** TSI is the nerdbox default and simpler, but places
   the policy decision inside libkrun. Confirm gvisor-tap-vsock is workable before
   committing.
