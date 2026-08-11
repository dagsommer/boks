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
| `run` | Creates or re-attaches, then attaches to the agent | `boks run [agent] [ws...] [-- args]`, persistent by default | P0 | partial | Agent-first grammar; re-attach implemented; exit codes propagate and Ctrl-C exits 128+signal; verified on the non-VM runtime only |
| Re-attach by workspace | Running again from the same workspace path reconnects instead of duplicating | Same: the derived name **is** the re-attach key | P1 | partial | Name is `<agent>-<workspace dir>`, so naming and re-attach are one mechanism. No host state store: containerd's container record is the state |
| `create` | Builds in background without attaching | `boks create` | P1 | partial | Creates without starting; verified on the non-VM runtime |
| `start` / `stop` | `stop` exists; **no top-level `start`** — run and exec start a sandbox implicitly | `boks stop`; `boks start` is a Boks addition | P1 | partial | Stop kills the task, keeps container + snapshot, and TERMs every process in the guest first; verified on the non-VM runtime |
| `rm` | Deletes sandbox and everything in it; `--force` for active | `boks rm`, `-f` | P1 | partial | Deletes the snapshot too; verified on the non-VM runtime |
| `ls` | Lists sandboxes with status and port mappings | `boks ls`/`boks list`, `-q`, `-json` | P1 | partial | sbx's columns exactly: SANDBOX AGENT STATUS PORTS WORKSPACE. PORTS renders empty — nothing publishes ports yet |
| `inspect` | **Not a top-level command** — sbx has only `policy inspect` | `boks inspect`, JSON output | P2 | partial | A deliberate Boks addition, not parity: the detail `ls` no longer shows has to be reachable |
| `exec` | `sbx exec [flags] SANDBOX COMMAND [ARG...]`, docker-exec flags; starts a stopped sandbox first | `boks exec [-i] [-t] [-it] [-e] [-w] [-u]` | P1 | partial | Starts a stopped sandbox, as sbx does. Streams IO, propagates the exit code; raw mode and resize unverified in a VM. `-u` takes numeric ids only; `--detach`, `--detach-keys`, `--env-file`, `--privileged` not implemented |
| Persistence across stop/start | Packages, Docker images, config, shell history persist until `rm` | Same, via the container's writable snapshot | P1 | partial | Confirmed across stop/start on the non-VM runtime |
| Naming | `--name`; reconnect from any directory | Same, `-name` | P1 | done | An explicit name overrides the derived one, and a `-name` run with no workspace argument does not re-mount the current directory — it reaches that sandbox from anywhere |
| `reset` | Stops sandboxes gracefully (30s), clears image cache, registries, all sandbox state, policies and stored secrets, signs out, stops the daemon, removes state/cache/config dirs. Prompts `y/N`; `-f/--force` skips it, `--preserve-secrets` keeps credentials | `boks reset`, minus the account and daemon steps | P2 | none | Copy the confirmation prompt and `--preserve-secrets`: losing every stored credential to a cleanup command is a bad surprise |

## 2a. CLI surface — where Boks matches sbx, and where it does not

Boks grew command-first because that is what proved the runtime. sbx is **agent-first**, and
the difference showed up in almost every command. Most of that is now closed; what is left
is listed as such. Rows are kept separate from the table above because they are shape
questions rather than missing features.

The reference for this section is real `sbx --help`, `sbx run --help` and `sbx exec --help`
output, plus a live `sbx ls`.

| Aspect | Docker behavior | Boks today | Prio | Status |
|---|---|---|---|---|
| `run` argument order | `sbx run [flags] [AGENT] [PATH...] [-- AGENT_ARGS...]` — the agent is the first positional, workspaces follow, and the primary workspace defaults to the current directory | Same grammar, same defaults: `boks run`, `boks run shell`, `boks run shell . ~/lib:ro` | P0 | done |
| Meaning of `--` | Arguments for the agent, appended to its command (`sbx run claude -- --continue`) | Same. Per-agent: an agent with its own command gets them appended, the shell agent takes them *as* the command, since that is what arguments to a shell are | P0 | done |
| Agents as a concept | A named set: `claude, codex, copilot, cursor, docker-agent, droid, gemini, kiro, opencode, shell`. Each selects an image and a startup command | `internal/agent`: all ten names registered, **only `shell` has an image**. The rest report "no image yet" and run with an explicit `-template` | P0 | partial |
| User-defined agents | Real and in use: a live `sbx ls` shows `udi-copilot-default` and `udi-copilot-yolo` beside the built-ins | `Registry.Add` is the seam, including override-a-built-in. **No loader and no file format** — deliberately not designed until there are runtime features worth declaring | P1 | partial |
| Arbitrary commands | `shell` is an agent, so a plain shell is inside the same grammar | Same: `boks run shell . -- uname -a` | P1 | done |
| Default sandbox name | `<agent>-<workspace directory name>`, case, dots and hyphens preserved: `claude-finndato.no`, `udi-copilot-yolo-efm-integrasjonspunkt` | Same, plus the decisions sbx's rule leaves open (see below) | P0 | done |
| `-name` without an agent | The agent positional is optional when the named sandbox exists, and is read from its spec; passing both re-attaches *and* asserts the agent | Same, including the assertion: a mismatch is an error naming both agents | P1 | done |
| `ls` columns | `SANDBOX  AGENT  STATUS  PORTS  WORKSPACE` | Identical. PORTS is rendered empty rather than omitted — nothing publishes ports yet | P1 | done |
| `ls` alias | `sbx list` | `boks list` | P2 | done |
| Image flag | `-t, --template` — "Container image to use for the sandbox (default: agent-specific image)" | Same name and alias. Was `-image` | P1 | done |
| Sizing defaults | `--cpus` 0 = all host CPUs; `-m, --memory` in binary units, default half the host's memory capped at 32 GiB | Same, including the unit parser. A bare number is bytes, as for `docker --memory`, and a value too small to boot is refused with the spelling the user meant | P1 | done |
| Detached run | `-d, --detached` prints the sandbox and exits without an interactive session | Same | P1 | done |
| Interactive by default | `sbx run` attaches an interactive session | Boks allocates a pty when stdin *and* stdout are terminals, and none when either is a pipe. There is no `-t` for it: `-t` is the template flag | P1 | done |
| Bare `boks` | Opens an interactive terminal dashboard: sandbox cards with live status, CPU and memory. `c` create, `s` start/stop, `Enter` attach, `x` shell, `r` remove, `tab` network panel, `?` shortcuts | Prints usage and exits 2 | P1 | none |
| `--clone`, `--kit`, `--profile`, `-p/--publish` | Run flags for clone mode, kits, profiles and port publishing | Not implemented; nothing in the current design blocks them | P1 | none |
| `ssh` | `sbx ssh` opens an SSH session into a sandbox | None | P2 | none |
| `daemon` | `sbx daemon start\|stop` controls a background service | Boks has no daemon and needs none today; it drives containerd directly | P2 | none |
| Boks-only commands | — | `boks start`, `boks inspect`, `boks run -rm`, `boks doctor`, `boks proxy`, `boks secret`, `boks policy` have no sbx equivalent at that spelling. Kept deliberately; none of them contradicts sbx's shape | — | done |

**What the naming rule leaves open, and what Boks decided.** sbx's `<agent>-<workspace>` is
confirmed, but three cases follow from it that a listing cannot show:

- *Characters containerd will not take.* containerd identifiers are alphanumeric runs joined
  by single `.`, `_` or `-`, capped at 76 characters — exactly the characters sbx's rule
  preserves, so ordinary directories pass through unchanged. Anything else becomes a single
  `-`, runs of separators collapse, and separators at either end are dropped: `my  project`
  and `my--project` both give `my-project`. A basename with nothing usable left — `...`, or
  a name written entirely in non-ASCII script — falls back to a six-hex-character digest of
  the path, which is unreadable but still correct and still re-attaches.
- *Two directories with the same basename.* Now possible, where the old path digest made it
  impossible. Boks does not reuse (the second project would silently attach to the first
  project's sandbox, with the wrong workspace mounted) and does not refuse (that leaves the
  user stuck at a name they never chose). The second directory deterministically gets
  `<agent>-<dir>-<digest>` and is told on stderr why its name is not the obvious one. The
  qualified name is checked first on later runs, so a sandbox that was bumped keeps its name
  even after the sandbox that bumped it is removed.
- *Roots and long paths.* A workspace at a filesystem root has no basename worth using and
  becomes `<agent>-root`; there is one per machine, so it stays unique. A name over
  containerd's 76-character limit is truncated — and truncation is exactly what could make
  two directories collide, so it always brings the path digest with it.

**Naming and re-attach are the same mechanism.** There is no separate identity: running the
same agent in the same directory derives the same name, and that name is what finds the
sandbox. `-name` overrides the derivation, which is what lets one workspace hold several
sandboxes and lets a sandbox be reached from any directory.

### 2b. Command inventory

Taken from real `sbx --help` output, so this is the actual surface rather than what the
website documents.

| sbx command | Purpose | Boks |
|---|---|---|
| *(bare)* / `tui` | Interactive dashboard; `tui` also opens it explicitly | none |
| `run` | Run an agent in a sandbox | `run`, command-first — realignment in progress |
| `create` | Create a sandbox for an agent | `create` |
| `exec` | Execute a command in a sandbox; **starts it first if stopped** | `exec`, but errors instead of auto-starting |
| `ls` / `list` | List sandboxes | `ls` (no `list` alias) |
| `stop` | Stop without removing | `stop` |
| `rm` | Remove sandboxes | `rm` |
| `cp` | Copy between sandbox and host | `cp` |
| `ports` | Manage port publishing | none |
| `policy` | `allow`, `deny`, `check`, `init`, `inspect`, `log`, `ls`, `profile`, `reset`, `rm` | `policy ls`, `policy log` only |
| `secret` | Manage stored secrets | `secret set/ls/rm` |
| `kit` | (Experimental) Manage kit artifacts | none |
| `template` | Manage sandbox templates (the image an agent runs in) | none; `-image` flag only |
| `skills` | (Experimental) Shared agent skills store | **won't** — Docker documents it as a cross-sandbox trust hole |
| `daemon` | `start`, `stop`, `status`, `log-level` for the `sandboxd` daemon | none, and none needed — see below |
| `diagnose` | Diagnose installation issues | `doctor` (same job, different name) |
| `setup` | (Experimental) Detect host configuration and prepare | partly `doctor`'s remedies |
| `reset` | Reset all sandboxes and clean up state | none |
| `login` / `logout` | Sign in to Docker | **won't** — no accounts, ever |
| `completion` | Shell autocompletion | none |
| `version` | Version information | `version` |

Boks additionally has `start` and `inspect`, which sbx does not: sbx starts implicitly via
`run`/`exec`, and its only `inspect` is `policy inspect`. These are deliberate additions, not
parity gaps.

**sbx runs a daemon; Boks does not.** `sandboxd` presumably owns VM supervision and state,
which is the natural consequence of managing hypervisors directly. Boks delegates that to
containerd and its shims, so there is nothing left for a daemon to own. This is a genuine
architectural divergence and the better position for a local-first tool — one less privileged
long-lived process — but it does mean anything sbx does *between* CLI invocations has no
equivalent here, and that needs watching as features land.

**Persistent, scoped policy is a bigger gap than it looked.** `sbx policy allow/deny/rm` write
rules that survive the invocation, and rules are either global or scoped to a single sandbox.
Boks only has per-run flags. `policy check` — asking whether a given access *would* be
permitted, without making the request — is a genuinely good idea worth copying: it makes a
policy testable instead of something you discover by being blocked.

**Resource defaults differ, and ours are worse.** sbx defaults to all host CPUs, and to half
of host memory capped at 32 GiB, with memory given in binary units (`8g`). Boks hardcodes
2 vCPUs and 2048 MiB, which will feel broken on a real workload.

### 2d. The kit format, from a real kit and the v2 reference

A working production kit (`schemaVersion: "1"`) and the documented v2 grammar together give a
clear picture. **Boks should not copy this schema**, but it should understand it, because it
is a mature answer to the same problem and it reveals requirements we would otherwise
discover late.

Top-level v2 shape: `schemaVersion`, `kind` (`sandbox` or `mixin`), `name`, `version`,
`displayName`, `description`, `sourceURL`, `licenses`, `locked`, `security`,
`agentInstructions`, `permissions`, `ports`, `credentials`, `environment`, `setup`,
`volumes`, and either `sandbox` or `extends` (with `requires` for mixins).

What matters for Boks' own design:

- **Kits are the agent mechanism.** A `kind: sandbox` kit names an image and an entrypoint,
  which is exactly what "an agent" is. Agents cannot be user-extensible without something
  kit-shaped, which is why kits move from P2 to P1.
- **Composition is first class.** `extends` inherits from a parent sandbox kit, `mixin` +
  `requires` layers onto an existing agent, and `locked` lists dotted paths a child may not
  override. That last one is a governance primitive worth remembering: it is how an
  organisation pins a network rule that a derived kit cannot loosen.
- **Credentials carry injection rules.** `credentials[].apiKey` has `name` (the guest
  environment variable), `proxyManaged`, and an `inject` array of `{domain, header, format |
  scheme, username}`. One credential, many domains — see `internal/secret`.
- **OAuth is handled without the token entering the guest.** `oauth` defines
  `tokenEndpoint`, `sentinels.{accessToken,refreshToken}`, `resourceHosts`, and a
  `credentialFile.{path,template}` Go template. The guest gets a credential file full of
  sentinel values; the proxy substitutes real tokens on requests to `resourceHosts`. This is
  how an interactive login works in a sandbox, and it means a credential is not always a
  single header value.
- **Setup has phases with different privileges.** `setup.install[]` runs as `user: "0"` by
  default, `setup.startup[]` as `user: "1000"` with an optional `background` flag, and
  `setup.files[]` writes content with a mode and `onlyIfMissing`. Static files can also be
  laid out on disk, with `files/home/` mapping to the agent's home and `files/workspace/` to
  the primary workspace.
- **`agentInstructions.{filename,content}`** injects instructions the agent will read — the
  v1 file called this `agentContext` with `sandbox.aiFilename`. Worth noting that a kit can
  shape agent *behaviour*, not just its environment; the real kit used it to forbid commits
  and warn against printing a token.
- **`volumes[]`** declares block-backed or `tmpfs` storage with a size, which is how the
  nested Docker data disk is expressed rather than being a special case.
- **`ports[]`**, `security.privileged`, and `sandbox.resources.{cpu,memory,gpu}` round it out.
- `sandbox.command` splits `default` from `interactive`, so an agent can behave differently
  with and without a TTY.

`permissions.network.{allow,deny}` documents the pattern forms supported: exact host, host
with port, single-label wildcard, **multi-label wildcard**, port ranges, port wildcard and
CIDR. Boks' policy engine implements one wildcard form, not two — a real gap, recorded here
rather than silently absent.

### 2c. Proxy modes, inferred from a real decision log

Real `sbx policy log` output shows a `PROXY` column with **three** values — `forward`,
`forward-bypass` and `transparent` — and their distribution across hosts is diagnostic:

| Mode | Hosts observed |
|---|---|
| `forward` | `api.anthropic.com`, `platform.claude.com`, `mcp-proxy.anthropic.com`, `github.com`, `api.github.com`, `raw.githubusercontent.com`, `auth.docker.io`, `registry-1.docker.io`, `ports.ubuntu.com:80` |
| `forward-bypass` | `proxy.golang.org`, `sum.golang.org`, `storage.googleapis.com`, `docs.docker.com`, `download.docker.com`, `production.cloudfront.docker.com`, `downloads.claude.ai`, `www.apache.org`, a Datadog log intake |
| `transparent` | `example.com:443`, `github.com:22` |

Every `forward` host is one with a credential to inject — Anthropic, GitHub, Docker registry
auth — plus one plaintext HTTP host. Every `forward-bypass` host has no credential: module
proxies, checksum databases, CDNs, telemetry, documentation. `transparent` covers flows that
never used the proxy at all, including SSH on port 22, which cannot traverse a `CONNECT`.

The reading Boks builds on — **inference from correlation, not documented fact**:

- **`forward`** — handled at HTTP level: plaintext HTTP directly, or HTTPS terminated and
  re-originated. Readable, and therefore injectable. The intercepted case.
- **`forward-bypass`** — tunnelled via `CONNECT` without terminating. Unreadable. "Bypass"
  means bypassing *inspection*, not bypassing the proxy.
- **`transparent`** — caught at the network layer without proxy cooperation. TCP-level policy
  only, so hostname rules cannot apply and IP/port rules are all there is.

Boks adopts these three names. Matching the reference vocabulary is worth more than inventing
clearer terms, and the semantics coincide with the design already chosen: interception is
opt-in per destination, and the mode is precisely "did this host have a credential rule".

**Log shape.** Columns are `SANDBOX  TYPE  HOST  PROXY  RULE  REASON  LAST SEEN  COUNT`,
split into blocked and allowed sections, and **aggregated** rather than one line per request —
one host showed 480 hits on a single row. `TYPE` is `network`, leaving room for the MCP and
filesystem policies documented elsewhere.

**Structured decisions.** A blocked row's rule reads:

```
no applicable policies for op(action=net:connect:tcp, resource=net:domain:api.anthropic.com:443)
```

So decisions are expressed over a typed action/resource vocabulary rather than free text,
which generalises to non-network policy cleanly. Boks' decision log should record an action
and a typed resource, leaving formatting and aggregation to the display layer.

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
| Port publishing | `--publish` at creation; `sbx ports SANDBOX --publish/--unpublish/--json` later; ignored when re-attaching | `boks ports` equivalent | P1 | none | The netstack can forward, and Boks explicitly asserts it does not. Spec `[[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]`; omitting HOST_PORT allocates an ephemeral one |
| Port binding defaults | Binds **loopback** by default, expanded per address family: `tcp`/`udp` bind both `127.0.0.1` and `::1`, or v4 only for a v4-only sandbox; `tcp4`/`udp4` v4 only; `tcp6`/`udp6` v6 only. Protocols: tcp, tcp4, tcp6, udp, udp4, udp6 | Same | P1 | none | Defaulting to loopback rather than all interfaces is the safe choice and should be copied exactly |
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
| Kits | Declarative YAML applied at creation: install commands, files, network rules, credential rules; can define a new agent or mix into an existing one | Boks equivalent, schema derived from our own runtime | P1 | none | Docker labels kits experimental and subject to change. Raised from P2: kits *are* the agent mechanism, so agents cannot be extensible without them. See below |
| Kit distribution | `sbx kit pack/push/pull/inspect/validate/add` — packaged as **OCI artifacts** in a registry | Local files first; OCI packaging later if it earns its place | P2 | none | Reusing OCI registries rather than inventing distribution is worth copying eventually |
| Templates | `sbx template save/load/ls/rm` — a template is a **saved snapshot of a sandbox**, reusable as the base for new ones, selected with `sbx run -t TAG`. Listed with a `FLAVOR` column naming the underlying agent type | Not started; `-image` takes a plain reference | P2 | none | Snapshot-and-reuse is a distinct feature from "pick an image", and a genuinely good one: configure a sandbox interactively, then freeze it |
| Templates | Base image a kit builds on (`sandbox.image`) | Base image reference | P2 | none | |
| Mixin composition | Kits layer onto agent runs | Layering wanted; semantics TBD | P2 | none | |
| Distribution | Kit install restricted to an allowlist, Docker Hub by default | Local files first; no registry near-term | P2 | none | Avoid a premature kit registry |
| Env vars in guest | User-level agent config does not transfer; custom env via a persistent shell file | Explicit `--env` and config, no implicit host inheritance | P1 | none | Not inheriting host env is a security win |

## 8. Agents and integrations

| Feature | Docker behavior | Boks target | Prio | Status | Notes |
|---|---|---|---|---|---|
| Bundled agents | Named agents (e.g. `sbx run claude`) with prepared images | Same names, data-driven registry; only `shell` has an image | P1 | partial | `internal/agent`. An agent is a name, an image, a command and an args mode — never a branch in the CLI. See 2a |
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
   them. `boks run shell /private/tmp/probe/deep/a/b/c/project -- pwd` printed that exact path, and
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
3. ~~**Re-attach identity.**~~ **Answered, then revised 2026-08-11.** The name is the
   identity. A sandbox is called `<agent>-<workspace directory name>` — sbx's rule,
   confirmed against a live `sbx ls` and against `sbx run --help` — and that derived name is
   what a second invocation looks up, so naming and re-attach are one mechanism rather than
   two. The earlier answer, a `boks-<12 hex path digest>` identifier, was collision-free but
   unreadable and impossible to guess; it is gone. `-name` still overrides the derivation,
   which is what lets one workspace hold several sandboxes and lets a sandbox be reached from
   any directory. Section 2a records the three cases the readable rule leaves open —
   characters containerd rejects, two directories sharing a basename, roots and long paths —
   and what Boks decided about each.

   Consequences worth knowing: renaming or moving a workspace directory orphans its sandbox
   (it is still listed, still removable, but a new one is created for the new name), and a
   symlinked workspace resolves to its target before naming, so two paths to the same
   directory share one sandbox.
4. **Kit schema.** Deliberately unanswered until real runtime features exist to configure.
5. ~~**TSI vs external network provider.**~~ **Answered 2026-08-11.** The external provider
   displaces TSI: with it attached the guest has `eth0` and a host loopback probe that TSI
   answered is refused. Two annotations are needed (`io.containerd.nerdbox.network.N` for
   the VM, `io.containerd.nerdbox.ctr.network.N` keyed by `vmmac` for the container), `addr`
   must be CIDR whatever nerdbox's docs show, and one stack serves exactly one VM. What
   remains open is the enforcement built on top: no policy has been applied to a real guest.
