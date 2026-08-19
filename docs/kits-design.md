# Kits in Boks — runtime semantics, gap analysis, and a sliced plan

**What exists today: `internal/kit` parses a kit's `spec.yaml`, both schema versions, and
nothing more.** No `--kit` flag, nothing fetches a kit, nothing applies one. Every "must",
"should" and "would" below describes a design, not behaviour you can run. Section 5 is the
order that design is meant to be built in; section 4 is why the order is that way.

This is the contract document for that work, written so the design is decided in one place
rather than rediscovered per slice. It is deliberately longer than the feature is, because the
expensive mistakes here are the quiet ones — a kit's network rules entering the policy scopes
at the wrong layer, or a fetched kit running `setup.install` as uid 0 without a pin — and each
of those is cheap to get right up front and awkward to unpick afterwards.

Written 2026-08-19 against Docker's product documentation, the `kit-author` skill and the
normative Go spec library in `docker/sbx-kits-contrib`, and the 35 real kits published there.
The parser's own testdata is those 35 kits, so the corpus claims below are checkable.

## 0. Sources and how they are cited

| Tag | File | What it is |
|---|---|---|
| `REF` | `kit-reference.txt` | Docker's rendered **product** page "Kit spec reference". Consumer-facing contract. |
| `KITS` | `kits.txt` | Docker's rendered product page "Kits". Covers `--kit` sources and `kit.allowedSources`. |
| `SPEC` | `contrib/spec/SPEC-v2.md` | Normative v2 grammar in `docker/sbx-kits-contrib`. |
| `LIFE` | `contrib/skills/kit-author/topics/lifecycle.md` | Authoring guide's engine-internals walkthrough. |
| `ANAT` | `.../topics/spec-anatomy.md` | Authoring guide, field by field. |
| `COMP` / `DIST` / `PIT` / `BIND` | `.../topics/{composition,distribution,pitfalls,bindings}.md` | Authoring guide topics. |
| `CODE` | `contrib/spec/*.go` | The **only executable** authority: the Go spec library. |
| `KIT/<name>` | `contrib/<name>/spec.yaml` | One of 35 real production kits. |

Anything marked **[inferred]** is my reasoning, not a read. Anything marked
**[conflict]** is a place two sources disagree; both are quoted.

Corpus facts worth stating up front, because they contradict the emphasis of the prose docs.
All 35 `contrib/*/spec.yaml` are `schemaVersion: "2"` (19 `kind: sandbox`, 16 `kind: mixin`).
Across all 35: **zero** use `extends:`, **zero** use `mixins:`, **zero** use `locked:`,
**zero** use `volumes:`, **zero** use `security.privileged`. Six use `requires:`, six use
`ports:`. Everything real is `sandbox`/`permissions.network`/`credentials`/`setup`/
`agentInstructions`/`files/home`.

`contrib/spec/` contains **no** merge, compose, or extends-resolution function at all
(`grep -rn "func ResolveExtends\|func Compose\|func Merge" spec/*.go` → empty). Every merge
rule in `COMP` and `LIFE` describes the closed-source `sbx` engine or the RFC's intent, not
code anyone can read. `CODE spec/types.go:675` says so directly about `Mixins`:

> Forward-compat: accepted at decode time so kits and the published v2 docs can declare it,
> but mixin composition is **not wired in this release** — the field has no runtime effect
> yet (a load-time warning fires when it is used).

---

## 1. Runtime semantics, field by field

### 1.1 The five moments

Docker's model has five distinct moments. Boks has the same five, spelled differently, so the
mapping is worth fixing before the table.

| Moment | Docker | Boks equivalent |
|---|---|---|
| **M1 kit load** | resolve ref → decode → normalize → validate; host-side, host user | new; nothing today |
| **M2 policy compile** | resolution handed to the proxy, fixed for the sandbox's life | `policy.Request.Resolve()` → `Resolution` → `Resolution.Policy()`, `internal/policy/resolve.go` |
| **M3 container create** | image, entrypoint, volumes, privileged, ports — immutable afterwards | `sandbox.Config` → containerd container + OCI spec, `internal/sandbox/lifecycle.go` |
| **M4 first start only** | `setup.install`, uid 0, synchronous, before the agent | new |
| **M5 every start** | `setup.startup` (uid 1000), `setup.files`, static `files/`, workspace-ready hook | partly exists: `internal/sandbox/clone.go:ensureClone` is exactly an M5 hook |

`SPEC §9.6` is the normative statement of M4/M5:

> - `setup.install` runs once, before the sandbox's entrypoint first runs.
> - `setup.startup` MAY run on every sandbox start (create, stop and start, daemon restart,
>   host reboot). Entries MUST be idempotent.

### 1.2 Field table

Privilege column: **host** = evaluated on the host, never in the guest. **uid 0** / **uid
1000** = runs as that user inside the guest.

| Field | Takes effect | Privilege | Ordering guarantee |
|---|---|---|---|
| `schemaVersion` | M1 | host | Loader **forks** on it. `"2"` uses a clean grammar with no v1 shims; a v1 field in a v2 spec is a decode error, not a fold (`REF` "Schema versions"; `LIFE §3`). |
| `kind` | M1 | host | Selects the composition slot. Exactly one `sandbox` per composition, N `mixin` (`SPEC §7`). |
| `name` | M1 | host | `^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`. Must be unique across the whole composition, including against the base sandbox (`COMP` "--kit" step 3). |
| `version`, `displayName`, `description`, `sourceURL`, `licenses` | M1 | host | Metadata. No runtime effect. `licenses` union across the chain (`ANAT`). |
| `locked` | M1 validate only | host | See §3.3 — validated for well-formedness and then **nothing enforces it**. |
| `security.privileged` | **M3 only** | host decision | Immutable after create. `sbx kit add` warns and skips (`PIT §4`). OR-merged across the chain (`COMP`). |
| `args` | **before M1 decode** | host | `${{ kit.args.<name> }}` substituted into the raw YAML *before* decoding, so an arg can parameterise any value including `sandbox.image` (`ANAT` "args"). Every reference must be declared. Values are always strings. |
| `extends` | M1, **opt-in** | host | Loaders return `Extends` as a string and do **not** walk the chain; caller must invoke the resolver (`PIT §6`). Max 5 levels, cycle-detected. Parent must be `kind: sandbox`. |
| `mixins` | M1 decode only | host | Decoded, warned, **no runtime effect** (`CODE types.go:675`, `REF` note). |
| `requires.agent` | composition time | host | `kind: mixin` only; a single agent name; validated for charset by the library, enforced by the consumer (`CODE validate.go:360`, `SPEC §4.3`). |
| `sandbox.image` | **M3** | host | Required for a root `kind: sandbox`; may be inherited via `extends`. |
| `sandbox.build` | never | — | Decodes, emits not-implemented; a kit with `build` **must also** set `image` or load fails (`ANAT`, `REF` note). |
| `sandbox.entrypoint` | M3 (recorded), M5 (exec'd) | uid 1000 | Flat string array. `entrypoint[0]` is the binary. Elements `[1:]` apply in **both** run modes. |
| `sandbox.command.default` / `.interactive` | M5 | uid 1000 | Effective argv = `entrypoint` + (`command.interactive` for a TTY, else `command.default`); `interactive` falls back to `default` when unset. Under `extends`, `command` **replaces** the whole inherited tail *including flags in the parent's `entrypoint`* — a child of `claude` adding `--settings` must re-state `--dangerously-skip-permissions` (`REF`, "Sandbox block"). |
| `sandbox.resources.{cpu,memory,gpu}` | M3 | host | `cpu` non-negative float; `memory` a byte-size string. P3 in `ANAT` — declared, enforcement consumer-defined. |
| `agentInstructions.filename` | M5 (post-start hook) | uid 1000 write | `kind: sandbox` only. A mixin that sets it is **ignored with a warning**, not an error (`REF`, `ANAT`). |
| `agentInstructions.content` | M5 (post-start hook) | uid 1000 write | Sandbox kit: inlined into the profile file, and only when `filename` is set. Mixin: written to `<dir-of-AIfile>/kits-memory/<kit-name>.md`, and the profile gains a sentinel-wrapped `## Kits` pointer section (`<!-- sbx:kits-section start --> … end -->`) rewritten on every run (`LIFE §11.5`). |
| `permissions.network.allow` / `.deny` | **M2** | host, in the proxy | Never enters the guest. Deny wins over allow for the same host, **including across composed kits** (`REF`; `SPEC §5.2`). Overlap is legal and intentional. Lists **append** across the chain. Must be live **before M4** — see §1.4. |
| `ports` | **M3** | host | Host port always ephemeral on `127.0.0.1`; a kit cannot pin one. Two kits asking for the same container port get two host bindings (`COMP`). `sbx kit add` cannot add ports (`REF` "Volumes"/`PIT §4`). |
| `environment.variables` | M4-prologue, then every exec | guest env | Set in the container environment. Union across kits, **last wins silently** (`PIT §9`). Names must be `[A-Za-z_][A-Za-z0-9_]*`. `DASH_*`/`SBX_*`/`DOCKER_*` are a **validation error**; `HOME`/`USER`/`SHELL`/`PATH`/`LD_PRELOAD`/`LD_LIBRARY_PATH` warn. Setting `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` from a kit points traffic away from the forward proxy and thereby disables both policy and credential injection (`KITS`, "Set environment variables", Important box). |
| `credentials[]` | M1 validate; **M2** wire-up; request time | **host only** by default | See §1.5. |
| `setup.install[]` | **M4, once** | **uid 0** default | `command` is a **`string` only** — a list is a validation error. Runs via `sh -c`. Synchronous, in declaration order, concatenated across kits in `--kit` order, and **before the entrypoint first runs**. Re-runs on recreate, so must be idempotent (`PIT §5`). |
| `setup.startup[]` | **M5, every start** | **uid 1000** default | `command` is a `string` **or** `list[string]`; list form is `exec`-style argv with no shell, so `&&` as a bare token is passed literally. `background: false` (default) makes the dispatcher wait before the next entry; `background: true` does not. **Neither gates the agent entrypoint** — "the agent launches once startup commands have been dispatched, regardless of `background`" (`REF`, "startup"). Persisted as per-kit scripts under `/etc/durable-startup.d/` and re-run after every real container start (`PIT §2`). Non-interactive, no TTY: an interactive login hangs. |
| `setup.files[]` | M5 | uid per the write path | Absolute container path; `mode` octal (default `"0644"`); `onlyIfMissing` wraps the write in `test -f`. **Only** `${WORKDIR}` is substituted, and **only in `content`** — any other placeholder fails validation. Cannot target a path at or under the `--clone` target: the write would create a non-empty directory and `git clone` would then refuse (`PIT §18`); the CLI rejects such kits up front. |
| `files/home/**` | M5 | copied, owned by agent | → `/home/agent/<path>`, mode preserved. Parent dirs created, existing files **overwritten** (`REF` "Static files"). |
| `files/workspace/**` | **M5, after the workspace is ready** | copied | → the primary workspace path. Fires in a *second* bucket after every other customizer and after the `--clone` `git clone`, because a hook that ran earlier would write into a directory that does not exist yet and abort the start (`LIFE §8`). |
| `files/<anything else>` | — | — | Ignored **with a warning**. Only `home` and `workspace` are recognised targets (`ANAT`). |
| `volumes[]` | **M3 only** | host | `path` absolute; `type ∈ {"", "tmpfs"}`; `size` a byte-size string; `mode` octal. Union by `path`, last-wins. `sbx kit add` cannot attach them (`REF`). |

### 1.3 Load-time safety (M1), which is a security boundary and not a nicety

`LIFE §2` and `§4`:

- Symlinks in `files/` must resolve inside the artifact root; escapes are rejected.
- Absolute static-file paths (`/etc/passwd`) and `..` traversal are rejected.
- YAML decoding is **strict everywhere**: an unknown field anywhere in the spec is a load
  error, not a silent ignore. `ANAT`: "implementations MUST NOT silently ignore unrecognized
  fields."
- Validation never errors on legacy v1 fields — that is normalization's job — but each fold
  appends one entry to `Artifact.Warnings`, and legacy fields do **not** round-trip.

### 1.4 Execution order — **[conflict]**, and it matters

`REF`, "Execution order", is the consumer-facing contract:

> When a sandbox is created, kit content is applied in this order:
> 1. Network permissions and environment variables.
> 2. Static files under `files/home/`.
> 3. `setup.install` commands, in declaration order.
> 4. `setup.files` entries.
> 5. `setup.startup` commands are registered for each sandbox start.
> 6. Static files under `files/workspace/`, after the workspace is ready. With `--clone`,
>    this means after the repository has been cloned.
>
> For stacked kits, entries in each stage are applied in `--kit` order. An install command
> can consume a bundled file from `files/home/`, but not one from `files/workspace/` or
> `setup.files`, because those files land later.

`LIFE §8` gives a different order for the same steps — its "customizers" bucket is
1 container settings, 2 **install commands**, 3 **environment variables**, 4 **static home
files**, 5 setup files, 6 startup, 7 hooks; then a second bucket with static workspace files.

So `LIFE` puts `setup.install` **before** env vars and before `files/home/`, and `REF` puts it
**after** both. This is not cosmetic: it decides whether an install script can read a
kit-declared env var or `cat` a bundled file.

**Adopt `REF`'s order.** Three reasons. It is the documented consumer contract, so it is what
kits in the wild are written against. `SPEC §9.5` independently corroborates the env-first
half — `SBX_CRED_<SERVICE>_MODE` is "Injected before `setup.install` runs" — and `PIT §5`
repeats it. And `LIFE`'s list is explicitly the engine's internal *append order into a
customizer chain*, which is a different thing from observable execution order. **[inferred]**
that `LIFE` is stale or is describing chain construction rather than execution.

Two orderings are *not* in conflict and are load-bearing for Boks:

- **`permissions.network` is stage 1, before `setup.install`.** Every real kit depends on
  this. `KIT/vale` installs by `curl`-ing `github.com` in `setup.install` and allows
  `github.com`, `objects.githubusercontent.com`, `release-assets.githubusercontent.com` for
  exactly that. `KIT/crush` allows `archive.ubuntu.com`, `security.ubuntu.com`,
  `ports.ubuntu.com`, `download.docker.com` and `repo.charm.sh` purely so its three install
  commands can run. If policy compiles after install, every one of these 35 kits fails.
- **`files/workspace/` is dead last**, after the clone.

### 1.5 Credentials: what happens where

Three separate machines, and conflating them is the main way to get this wrong.

**(a) M1 — declaration.** A kit declares `service`, optionally `apiKey`, optionally `oauth`.
It declares **no discovery source**: "A kit can't read arbitrary host environment variables or
files" (`REF`, "Credentials"). At least one of `apiKey.inject` or `oauth` is required for a
service not in the provider registry (`ANAT`). `provider` is a "forward-compat stub… warns and
has no effect" (`SPEC §5.4`).

**(b) M2 — resolution and wire-up.** Two ordered stages (`BIND`, "Resolution order"):
which binding (CLI `--credential X=variant` → workspace-`remembered` → `bindings[X]` → prompt),
then where the value is (sandbox-scoped secret store → global secret store → the binding's
`discovery[]` in order). The secret-store layers fire **before** discovery.

The gate that matters: **the engine injects only into a domain present in *both* the kit's
`apiKey.inject[].domain` and the user's `bindings[<service>].allowedDomains`** (`PIT §14`,
`LIFE §7`). A domain the kit wants and the user has not approved triggers a domain-expansion
prompt at create time; declining does **not** fail creation — the injection is silently
skipped and the request goes out unauthenticated.

**(c) request time — the proxy.** `apiKey.proxyManaged: true` means the in-guest value of
`apiKey.name` is the sentinel and the proxy swaps the real value into the outbound request per
`inject[]`. For `oauth`, the proxy intercepts the token endpoint response and replaces the
tokens with `sentinels.*` before the guest sees them, swapping the real token back on outbound
requests to `resourceHosts`. `passthrough: true` opts out and sends the real token into the
guest — a deliberate downgrade.

Two credential-shape **[conflict]**s:

1. `proxyManaged`. `REF` lists it as an `apiKey` field, default `false`, and as the v2 home of
   v1's `environment.proxyManaged`. `SPEC §5.4.1` shows it. All 15 credential entries in
   `KIT/crush` set it. But `ANAT` says: "The block is **P2** because v2 removed its
   `proxyManaged` field as part of the credentials redesign — the proxy-managed semantic now
   lives implicitly on `credentials[].apiKey.name`." `ANAT` is wrong; the field exists, is
   documented in both normative sources, and is used by every shipped kit.
2. `credentialFile.structure` vs `.template`. `ANAT`: "`credentialFile.structure` is a
   declarative JSON map … (preferred)". `REF`: "`credentialFile.structure` — Declarative JSON
   shape defined by schema v2 but **not supported by the sbx engine. A structure-only kit
   fails validation. Use `credentialFile.template`.**" `REF` wins — it describes the shipping
   engine. Boks should read both and render from `template`.

`oauth.resourceHosts` appears in `REF` and in `docs/docker-sandbox-parity.md §2d` but **not**
in `ANAT` or `SPEC §5.4.2`. Treat `REF` as authoritative. `oauth.skipIfEnv` is "Accepted for
compatibility, but ignored for schema v2."

### 1.6 `sbx kit add` — **[conflict]**, and the newer story is simpler

`LIFE §11` and `PIT §3`/`§4` describe applying a kit to a *running* container by replaying
install/env/files/setup-files/startup via exec and `docker cp`, warning-and-skipping the
immutable parts, and recording an audit trail in `~/.sandbox-plugins.json`.

`REF` and `KITS` describe something different and narrower:

> `sbx kit add` **recreates the sandbox** rather than modifying it in place. It supports mixin
> kits limited to `environment.variables`, `setup.install`, and `permissions.network.allow`,
> which follow the same order as sandbox creation. It **rejects** a kit that declares static
> files, `setup.startup`, or `setup.files`. (`REF`, "Execution order")

`KITS` adds that the restart preserves VM state — packages, images, volumes, agent history —
and that kits cannot be *removed* from a running sandbox. Take `REF`/`KITS`: same-URL later
docs, and the restrictive shape is the one to copy. It also happens to be the only shape Boks
can implement cheaply, since Boks fixes a sandbox's policy at start (`resolve.go`,
`Resolution` doc comment: "the rules a sandbox is running under are fixed at the moment it
starts").

### 1.7 The image contract a kit assumes

`REF` ("Sandbox block") and `SPEC §9.1`–`§9.4`, normative for any runtime that consumes kits:

- non-root default user named `agent`, **uid 1000**, home `/home/agent`, with **passwordless
  sudo**;
- `HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` **preserved across sudo**;
- `sh` and `curl` present. `git`, `jq`, `node`, `python3` are explicitly **not** in the floor;
- a root install step that writes under `/home/agent` **must** restore ownership
  (`chown -R agent:agent …`) or later agent-user writes fail;
- amd64 and arm64 both;
- the runtime injects `SBX_CRED_<SERVICE>_MODE` ∈ {`apikey`,`oauth`,`none`} before install,
  plus `MCP_GATEWAY_URL`, `MCP_SENTINEL_TOKEN_NAME`, `PROXY_CA_CERT_B64`,
  `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`, `REQUESTS_CA_BUNDLE`, `PIP_CERT`,
  `JAVA_TOOL_OPTIONS`. Kit content may read these and **must not** overwrite them or copy an
  observed value anywhere persistent.
- a long-running background process from kit content must be started as a process-group
  leader (`setsid`) with stdio redirected, so it outlives its setup command (`SPEC §9.6`).

Boks' agent images already number the agent user 1000 (commit `ec19e20`, "number the agent
user, so a sandbox is not root off Linux"), so the uid half is satisfied. Passwordless sudo
and proxy-env-across-sudo are **unverified** here.

---

## 2. How a kit is obtained

### 2.1 The accepted reference forms — **[conflict]**, and this one is a security decision

`docs/docker-sandbox-parity.md §2e` currently records three forms: "a **local filepath**…, an
**HTTPS URL**, a **git URL**." **That is wrong** and should be corrected. There is no plain
HTTPS-URL form in either source. The actual set:

| Form | `KITS` (product) | `DIST`/`LIFE` (authoring guide) |
|---|---|---|
| Built-in name | `claude` — embedded in the binary | same |
| Local directory | `./my-kit/`, or `file://./my-kit` | same |
| **Local ZIP** | `./my-kit-1.0.zip` — `sbx run claude --kit ./my-kit-1.0.zip` | **not mentioned at all** |
| Git | `git+https://…/repo.git#ref=<branch\|tag\|commit>&dir=<path>`, "**Defaults to the repository's default branch**". `git+ssh://` too, using the local SSH agent, credential helpers and `.netrc`. | `git+https://` / `git+ssh://` with `#ref=` that **MUST** be a full 40-hex commit SHA; "Branch names and tags (including semver tags like `v1.2.3`) are **rejected**." |
| OCI | `ghcr.io/myorg/my-kit:1.0` — **bare registry ref, tag allowed, no scheme**. Docker Hub needs an explicit `docker.io/` prefix. | `oci://ghcr.io/org/kit@sha256:<digest>` — scheme required, "**MUST** be a digest; `:latest` and any tag is rejected." |

The fragment grammar is `#key=value` pairs after `#`, URL-encoded: `ref` (revision) and `dir`
(subdirectory containing `spec.yaml`, defaulting to the repo root). Quote the URL in shells
where `&` backgrounds.

`DIST` itself admits the pinning rule is aspirational: "**Until the strict-pin rule is
enforced everywhere by the consumer CLI**, the README's examples show tag-based refs for
ergonomics — but new kits should pin to SHAs." So: **pinning is a documented SHOULD in the
authoring guide and is not enforced by the shipped CLI.** `KITS`' own example is
`#ref=v0.1.0` — a tag.

**What is specified about verification: nothing.** No signature field, no checksum field, no
transparency log. `ANAT` mentions signing only in passing and only as a property of `args`
("a signature covers the declarations and defaults; the values an installer supplies do not"),
with no mechanism named anywhere. OCI-by-digest is the only integrity story, and only when the
consumer chooses a digest. This is the single largest thing Boks must decide for itself, and
`docs/docker-sandbox-parity.md §2e` already says so: everything else Boks fetches is pinned
(`NERDBOX_REV` pins a commit, tarballs pin SHA-256, images pull by digest), so an unpinned kit
would be the one exception and the most powerful one.

### 2.2 `sbx kit` command surface

`sbx kit validate <path>` · `inspect <path> [--json]` · **`pack <path> -o <file.zip>`** ·
`push <path> <ref>` · `pull <ref>` · `add <sandbox> <ref>` · `delete <oci-ref>` (`KITS`,
`DIST`). `pack` is in `KITS` only — another sign the ZIP form is newer than the authoring
guide. `kit push` rewrites the spec into a **distribution form** (image pinned by digest,
`sandbox.build` stripped) and never modifies the source `spec.yaml`; it also publishes a
sibling GC-anchor tag `<ref>:_kit_<tag>` so the underlying image is not garbage-collected
(`DIST`). Registry auth: `pull` prefers `sbx secret set --registry`, falling back to the
Docker credential store; `push` uses only the Docker credential store.

### 2.3 "Restrict kit sources"

This is the mechanism that exists *because* pinning is not enforced, and it is quoted here in
full because it is the design Boks should copy first (`KITS`, "Restrict kit sources"):

> `sbx` restricts which sources a kit can install from. A kit's install commands run with root
> privileges inside the sandbox, so limiting where kits come from reduces supply-chain risk.
> **By default, only kits hosted on Docker Hub (`docker.io/`) are allowed.** Loading a kit
> from any other source fails:
>
> ```
> ERROR: resolve kits: kit "git+https://github.com/docker/sbx-kits-contrib.git#dir=vale"
> cannot be installed — its source is not in your allowlist.
> ```

- `sbx settings set kit.allowedSources '["docker.io/","github.com/docker/"]'` — **replaces the
  whole list**, so the user must re-state entries they want to keep.
- Entries match as **prefixes on a path-segment boundary**: `github.com/docker/` allows
  `github.com/docker/sbx-kits-contrib` but **not** `github.com/docker-evil/kit`. That
  boundary rule is the whole security property and is easy to get wrong.
- `["*"]` disables the restriction. "This isn't recommended."
- Local directories and ZIPs are governed **separately** by `kit.allowLocalKits`, default
  `true`; set `false` to require a remote source.
- Non-interactive equivalents: `DOCKER_SANDBOXES_KIT_ALLOWED_SOURCES`,
  `DOCKER_SANDBOXES_KIT_ALLOW_LOCAL`.

Note what this does **not** do: it constrains *where* a kit came from, not *what it is*. An
allowlisted publisher's kit still runs arbitrary root commands, and the allowlist is a
publisher-identity check with no version identity in it at all.

---

## 3. Composition

### 3.1 The three mechanisms

| | `extends:` | `mixins:` | `--kit` |
|---|---|---|---|
| Kind | author-time inheritance | author-time composition | runtime composition |
| Cardinality | single parent, ≤5 levels, cycle-detected | N | N |
| Declared in | sandbox kit's `spec.yaml` | sandbox kit's `spec.yaml` | CLI flag |
| Resolution | **opt-in** — the caller invokes the resolver; loaders do not walk the chain | automatic on load *per `COMP`* — **but not wired at all per `CODE`** | automatic on `sbx run` |
| Allowed on | `kind: sandbox` only; parent must be `sandbox` | `kind: sandbox` only | — |

Resolution order when all three are present (`COMP`; `ANAT`; `SPEC §7`):
**extends chain (parent → child, recursively) → the kit's own fields → declared `mixins:` in
order → runtime `--kit` in order.** Order of `--kit` flags *is* the merge order, and it
changes install/startup execution order, the `environment.variables` winner, and the `files`
overlay winner (`COMP`, "Order matters").

### 3.2 Precedence when parent and child set the same field

From `COMP`/`LIFE §5`, identical tables in both. This is `extends`; `--kit` differs where noted.

| Field type | Strategy | Same key in both |
|---|---|---|
| Scalars | child wins if set | child overrides parent |
| Maps | recursive merge | child wins per conflicting key |
| **Named arrays** (identity key: `credentials[].service`, `volumes[].path`) | union by identity | **`extends`: matching key is an ERROR.** `--kit`: `credentials[]` same `service` in two kits is an error; `volumes[]` same `path` is **last-wins** |
| Primitive arrays (`permissions.network.allow`/`deny`) | set union, deduplicated, parent order first | always succeeds; deny wins at request time |
| Setup lists (`install`, `startup`, `files`) | concatenate | parent first, then child |
| `files` (static) | overlay keyed `target:relativePath` | child/later kit wins |
| `environment.variables` | union | **last wins, silently** (`PIT §9`) |
| `ports` | append | two kits, same container port → two ephemeral host ports |
| `security.privileged` | **OR** | any `true` → `true` |
| `licenses` | union | — |

The asymmetry to hold onto: `credentials` under `extends` **errors** on a matching `service`
"because the merge can't decide which shape wins" (`LIFE §5`), whereas `environment.variables`
silently takes the later value. One conflict class is fatal, an adjacent one is invisible.

Also note the `extends` + `command` trap from `REF` (§1.2 above): `sandbox.command` *replaces*
the inherited argument tail including flags carried in the parent's `entrypoint`. That is a
scalar-ish "child wins" rule applied to something an author reads as additive, and it silently
drops `--dangerously-skip-permissions`.

### 3.3 What `locked` forbids — and the fact that nothing enforces it

`REF`: "`locked` — Dotted paths child kits may not override." `ANAT` calls it P2 and gives the
example list `sandbox.image`, `credentials[service=anthropic]`.

The code says less. `CODE spec/types.go:683`:

> `Locked` lists dotted YAML paths (e.g. `"agent.image"`) on this artifact that child kits
> must not override during single-parent inheritance. **The spec library only validates
> well-formedness; enforcement lives in the consumer that performs the merge.**

`CODE spec/validate.go:366` `ValidateLocked` checks: non-empty, no duplicates, and a match
against `lockedPathPattern = ^[a-z][a-zA-Z0-9]*(\.[a-z][a-zA-Z0-9]*)*$`.

Two consequences.

1. **`ANAT`'s own example is invalid.** `credentials[service=anthropic]` contains brackets,
   which that regexp rejects — `validate_test.go:397` pins exactly this, rejecting
   `agent.image[0]` with "not a well-formed dotted path". So `locked` cannot name an array
   element at all today; it can only name whole blocks (`credentials`, `sandbox.image`,
   `permissions`). A kit that copied the documented example fails to load.
2. **Nothing in the readable world enforces it.** There is no merge function in
   `contrib/spec/`, no shipped kit uses `locked`, and `REF` never says what happens when a
   child violates it. `docs/docker-sandbox-parity.md §2d` calls `locked` "a governance
   primitive worth remembering: it is how an organisation pins a network rule that a derived
   kit cannot loosen." That is the *intent*; **it is not a capability that exists**, and Boks
   should not plan around inheriting a working implementation. **[inferred]** from the absence
   of any enforcement code or documented failure mode.

Note also that `locked` is scoped to `extends` only in every source ("during single-parent
inheritance"). It says nothing about `--kit`, which is the direction an untrusted kit actually
arrives from. As a governance primitive it is pointed the wrong way.

### 3.4 When two mixins conflict

Only three error conditions are documented (`LIFE §6`, `COMP`):

- `duplicate kit name X — each kit must have a unique name` — across the base sandbox and all
  mixins, declared and runtime, including a mixin whose name equals the base sandbox's. "No
  partial state is created."
- `credential X defined in both A and B` — same `service` in two kits is a **hard error** at
  compose time, even if the shapes are identical.
- `kit X must be kind mixin, got sandbox` — two `kind: sandbox` in one stack.

Everything else resolves silently by the table in §3.2. `requires.agent` mismatch is a fourth
composition error, enforced by the consumer rather than the library (`SPEC §4.3`): "composing
it onto any other base agent is a composition error (rather than silently producing a
nonsensical-but-valid sandbox)". `requires.agent` is a single name, not a set; family matching
across `claude` / `claude-vertex` / `claude-bedrock` is "left to the consumer's
`extends`-lineage check" — i.e. unspecified.

---

## 4. Gap analysis against Boks

Extends `docs/docker-sandbox-parity.md §2d`/`§2e`; does not repeat them. Corrections to those
sections are flagged **[correction to 2e]** / **[correction to 2d]**.

`docs/roadmap.md` already carries kits as **item 8 of "Next"**: "Kits / declarative
configuration, which is also what would let an agent be defined in a file rather than in
code," with "no `--kit`" and "no kits" recorded under gaps.

### 4a. Network policy — where a kit's rules must enter, and what breaks otherwise

**First, a correction to my own brief and to §2e's framing.** Boks' policy model is *assembled*
in scopes but *decided* flat. `internal/policy/policy.go:73` on `Rule.Scope`:

> Scope says where the rule came from — a preset, a profile, the global store, one sandbox's
> own rules, or a flag on this run. **It carries no weight in the decision: deny beats allow no
> matter which scope either sits in.** It exists so that a verdict can name the thing a user
> would have to edit to change it.

And `Policy.Evaluate` (`policy.go:161-202`) is a deny pass over the whole flattened rule set
followed by an allow pass, with the doc comment: "Precedence is over the whole rule set, not
over the scopes that contributed to it." Precisely: **first-match within each pass, but every
deny is tested before any allow.** `Deny` is also the zero value of `Action` (`policy.go:49`),
so the model fails closed by construction. `RuleSpec.Scope` carries `json:"-"`
(`store.go:97-107`) — it is filled in at resolution and never stored, which is why a scope
label is free to invent.

So there is exactly **one** ordered thing in the whole model, stated in `resolve.go`'s package
comment: **the base layer decides the default action; every other layer only concatenates.**
The six layers, in assembly order, are:

```
1. base      a preset, or a stored profile — the default action and its rules
2. profile   the selected profile's own rules
3. agent     what the agent being run cannot work without, from its own definition
4. global    stored rules that apply to every sandbox on this machine
5. sandbox   stored rules for this sandbox only
6. flags     --allow / --deny on this invocation (a Boks addition, not sbx parity)
```

#### Where a kit's rules go

**A kit's `permissions.network` becomes a seventh layer, `kit <name>`, placed immediately
after the `agent` layer (position 3.5) and before `global`.** The justification is that a kit's
allows are the same *kind* of fact as an agent's allows — "what this thing cannot work without,
from its own definition" — and `resolve.go` already documents that layer's contract precisely:
"a deny in any scope beats an agent's own allow… and there is a test that says so."
`docs/docker-sandbox-parity.md:387` already describes the agent layer as sitting "between the
preset and the global scope", so a kit layer beside it needs no new vocabulary.

Because position carries no decision weight, the placement is almost entirely a *display and
provenance* choice: `boks policy ls` must be able to say `kit vale` next to
`objects.githubusercontent.com` so the user knows which file to edit. What matters far more are
four properties that the layer's position does **not** give you for free.

**(1) The `locked` preset must skip the kit layer, exactly as it skips the agent layer.**
`resolve.go` special-cases `base.Name == PresetLocked` and emits the layer with count 0 and the
reason "not applied: preset locked allows only what you write," on the stated grounds that
"`locked` is documented as 'deny everything; every destination must be added with `--allow`',
and a user who reaches for it is usually trying to stop an agent phoning anywhere at all — an
agent's own API being quietly exempt would defeat exactly the thing they asked for."

If a kit layer does not get the same treatment, `boks run --policy locked --kit ./x` is
strictly weaker than `boks run --policy locked`, and the thing that widened it is a file
fetched off the network. This is the single most important line of code in the whole feature.
The empty-but-visible layer convention should be reused verbatim: "a rule that silently did not
apply is as confusing as one that silently did."

**(2) `Resolution.ToRequest` will silently drop the layer unless it is taught about it.**
`resolve.go`'s `ToRequest` reconstructs a `Request` from a `Resolution` so the supervisor can
hot-reload store-derived rules. It recovers rules by matching `rule.Scope` against
`scopeFlagAllow`, `scopeFlagDeny`, and `"agent "+r.Agent`; **everything else falls into
`default:` and is discarded**, on the assumption that it will be re-derived from the store or
the preset. A kit rule is neither. Add a `kit <name>` case (and a `Resolution.Kits []string`
field to carry the names), or the first hot-reload of the store quietly deletes every allow a
kit contributed and the sandbox loses network to the very hosts the kit needs.

**(3) A kit's rules must be *recorded*, not re-derived, or `boks start` re-fetches remote
input.** `policy.SandboxPolicy` is the record written to a container label at create time and
read back by every later command; its doc comment is explicit that it "records the
*selection*, not the resolved rules: the profile, the preset, and the per-run allow and deny
specs. The stored global and per-sandbox rules are deliberately left out and re-read at start
time, so that a rule added after a sandbox was created still reaches it."

Agent allows sit on the re-derive side of that line, deliberately: "re-derived from the
registry every time rather than recorded on the sandbox — so an entry removed from a later Boks
stops applying to sandboxes that already exist." For a **remote** kit that property inverts
into a hazard: re-deriving means re-reading a kit that the publisher may have changed, so
`boks start` on an existing sandbox could quietly widen its policy, and a `git+https` ref
without a pin could do it from a different machine. The kit's *resolved allow/deny specs* must
therefore be recorded in `SandboxPolicy` (alongside `Allow`/`Deny`), not the kit reference. The
doc comment's other promise — "**Nothing here is a secret.** The fields are destinations and
preset names" — continues to hold, since kit rules are destinations too.

**(4) Pattern semantics must be translated, not copied.** Two independent silent widenings if
kit strings are passed to `policy.ParseRule` verbatim:

- **Wildcard breadth.** The kit spec: `*.example.com` matches **exactly one** label —
  `api.example.com` ✓, `example.com` ✗, `a.b.example.com` ✗ — and the multi-label form is a
  *separate*, not-yet-enforced `**.example.com` (`SPEC §5.2`, `PIT §19`). Boks'
  `internal/policy/pattern.go:113` implements `patternSuffix` as
  `strings.HasSuffix(t.Host, "."+p.host)`, which matches **any** number of labels. The package
  comment states the intent — "The wildcard follows the TLS certificate rule" — but the
  implementation is broader than the TLS rule it names. So `*.example.com` from a kit becomes
  a `**.example.com` in Boks. `docs/docker-sandbox-parity.md §2e` records the reverse gap
  ("Boks' policy engine implements one wildcard form, not two"); **[correction to 2e]** it is
  not merely one-of-two, the one it has is the *wider* one, so importing a kit's rule grants
  more than the author wrote.
- **Port breadth.** A bare host in a kit means "exact host, **default port 443**" (`ANAT`'s
  entry-format table). A bare host in Boks means every port: `ParsePorts("")` returns the empty
  `PortSet` and `PortSet.Any()` is `len(s.ranges) == 0` (`pattern.go:158`, `:198`). So
  `allow: [api.example.com]` from a kit, imported literally, also opens port 80 and everything
  else — and `internal/secret/service.go` already documents why that is not acceptable:
  Boks pins credential hint ports to 443 because "allowing port 80 beside it would add a
  plaintext downgrade path nobody asked for."

The translator must therefore be explicit and lossy in the **narrowing** direction:
`host` → `host:443`; `*.host` → a new single-label pattern kind; `**.host` → today's
`patternSuffix`; `host:port` → as-is; `host:lo-hi`, `host:*`, CIDR → Boks already supports
these and `sbx` does not, so they parse but a kit using them is non-portable and should warn.
Adding a single-label pattern kind to `pattern.go` is a real change to a security-critical
file and is where the risk in this slice lives.

Two confirmations that `:443` is the right default rather than an invention. The `standard`
preset's eleven allows are **all** written `host:443` (`preset.go:122-134`), and
`internal/secret/service.go:158-163` defaults a service's port hint to `"443"`. A kit's bare
host translating to `host:443` therefore matches both existing conventions in the repo.

**(5) There is a fourth place kit rules must be handled: the flag/record merge for an existing
sandbox.** `internal/cli/netflags.go:156-176` merges this run's flags with what the sandbox
recorded, and the merge is deliberately asymmetric: preset and profile are **replaced**,
`--allow` is **replaced**, and `--deny` is a **union** (`unionDenies`, `netflags.go:179`). That
asymmetry is the same principle as everywhere else — a run may narrow but not widen. Kit rules
must land on the *narrowing* side of it: a kit's denies union, a kit's allows do not survive a
re-run that names a different kit set. `netflags.go:162-165` is also where `req.Agent,
req.AgentAllow = f.agent.Name, f.agent.AllowRules()` happens, so it is the natural place for
`req.Kits, req.KitAllow` to be set too.

**(6) Validate kit rules at load, never drop them at start.** `agent.Registry.Add`
(`agent.go:163-167`) runs every `Allow` spec through `policy.ParseRule` **at registration
time** rather than at sandbox start, on the stated grounds that dropping an unparseable rule
would leave "a policy with a hole in it that nothing announced." A kit's `permissions.network`
entries deserve exactly that treatment: a rule that Boks cannot translate must fail the kit
load, not be skipped with a log line. This is the difference between the pattern translator
being a security control and being a best-effort convenience.

Once the layer exists the rules travel to the enforcement point for free: `Resolution` is
embedded as `enforce.Spec.Resolution` (`internal/enforce/enforce.go:110`, JSON tag `"policy"`),
the whole `Spec` is marshalled and piped to a child `boks net serve` **on stdin**
(`internal/enforce/supervisor.go:296-302`), read back by `enforce.ReadSpec`
(`supervisor.go:659-668`), and compiled by `Spec.Policy()` (`enforce.go:171-176`) — which falls
back to the default preset when `Resolution` is nil, the fail-closed direction. No new plumbing
is needed for a new layer.

#### What breaks if the layer enters in the wrong place

- **Into the base (as a preset or profile substitute):** the kit gets to set the *default
  action*, the one thing a layer's position controls. A kit with a broad allow list plus an
  implicit open default is a policy bypass. Never.
- **After `flags`:** harmless for the decision (order does not matter) but it makes
  `policy ls` read as though a kit could override `--deny`, which is the opposite of the
  truth. Provenance is the whole reason layers are ordered at all.
- **Inside the `agent` layer** (i.e. merging kit allows into `Agent.Allow`): loses the
  provenance string, so a verdict says `agent claude` for a rule a third-party kit wrote. It
  also means the rules flow through `Agent.AllowRules()` and inherit the agent layer's
  locked-skip for free — which is right — but at the cost of the one thing `Scope` exists for.
  Keep it a separate layer that *copies* the agent layer's conditional.
- **As store rules (global or sandbox scope):** they become persistent and survive removing
  the kit, and `boks policy rm` becomes the only way to undo a `--kit`. Worse, they would then
  be re-read on hot-reload, mixing kit-derived and user-written rules in one file.

#### Deny is the easy half

A kit's `permissions.network.deny` needs none of this care. Boks' engine tests every deny
before any allow across the flattened set, so a kit's deny is automatically as strong as a
global one, in every preset including `open`. `KITS` describes the same asymmetry for
organization governance — "kit-defined allow rules are ignored… Kit-defined deny rules still
apply, because a deny can only restrict access further" — which is a useful confirmation that
allow and deny from a kit deserve different trust. **A kit's deny can be honoured
unconditionally, including under `locked`; a kit's allow cannot.**

#### Enforcement points already in place

`internal/proxy/proxy.go` calls `Engine.CheckMode` at three stages —
`policy.StageHTTP` (`:245`), `policy.StageConnect` (`:310`), `policy.StageSNI` (`:369`) — and
then re-checks the **resolved address** against deny rules only in `dial` (`:469`), because "an
allow written for a name says nothing about the address behind it" and `allowed.example A
127.0.0.1` would otherwise be a path to the host's own services. A kit's allow inherits that
protection for free. No new enforcement point is needed for `permissions.network`; only
resolution changes.

### 4b. Credentials — what maps 1:1, what is close, what is new

Boks is much further along here than the roadmap's framing suggests. `internal/secret` is a
vendor registry with injection rules, sentinel minting, and OAuth refresh, all host-side.

**1:1, field for field.** `internal/secret/service.go:66` `ServiceInject` against
`credentials[].apiKey.inject[]`:

| Kit | Boks `ServiceInject` |
|---|---|
| `inject[].domain` | `Hosts []string` — a list per rule instead of one entry per domain; "Bare hostnames: an injection domain has no port dimension, because interception is decided per host," which is exactly the kit rule |
| `inject[].header` | `Header string` — "Empty means Authorization" |
| `inject[].format` | `Format string` — "the header value with exactly one `%s`. Empty means the bare secret. Mutually exclusive with Scheme" |
| `inject[].scheme` | `Scheme Scheme` — "the bearer/basic shorthand. Mutually exclusive with Format" |
| `inject[].username` | `Username string` |
| — | `Why string` — a Boks addition, no kit equivalent |

The mutual exclusion of `format` and `scheme` is already implemented with the same semantics.
Translating a kit's `credentials[]` into `[]secret.ServiceInject` is close to mechanical: group
`inject[]` entries by identical (header, format/scheme, username) and collect their domains
into `Hosts`. **[inferred]** — grouping is an optimisation, not required; one `ServiceInject`
per `inject[]` entry also works.

**The validation a kit's `inject[]` needs already exists.** `secret.Inject.Validate()`
(`internal/secret/secret.go:168-201`) rejects an empty domain; rejects `Domain.IsAny()` with
"'*' is not allowed as an injection domain; name the hosts that may receive this secret,
because those are also the hosts whose TLS boks will terminate"; enforces format/scheme
exclusivity; requires **exactly one `%s`** and no other `%` verb; and rejects `\r`/`\n`
(header splitting), with `validHeaderName` restricting the header to `[A-Za-z0-9-_.]`. Every
one of these is a check the kit schema either does not make or leaves to the consumer. Route
kit credentials through it unchanged.

**There is an existing seam that makes kit credentials nearly free.** `secret.Service` renders
itself back into the *same CLI flag strings a user would type* —`InjectSpecs()`
(`service.go:204-213`), `GuestSpec()` (`:258`), `AllowSpecs()` (`:189`) — and `Credential()`
(`:267-282`) parses them back through `ParseCredentials`. `service.go:198-203` states this is
the design point. `ServiceRegistry.Add` (`:358-370`) replaces by name and is documented as "the
user-definition seam". So a kit's `credentials[]` can be translated into flag strings and
admitted through the identical path a hand-typed `-inject` takes, with no new trust path into
the injector.

**Precedence between `apiKey` and `oauth` is inverted between the two systems.** The kit spec:
"When both resolve at runtime, the **API key takes precedence** and OAuth acts as the fallback"
(`REF`, "Credentials"; same in `SPEC §5.4` and `ANAT`). Boks does the opposite:
`secret.PreferOAuth(credentials)` (`service.go:427-459`) **drops an API-key credential whose
destinations are fully covered by an OAuth credential's `ResourceHosts`**, matching on
destinations rather than names. Both are defensible — OAuth-first is the stronger posture, since
an OAuth access token is short-lived and never leaves the host — but they are not the same, and
a kit author who set both expecting the API key to win gets the other one. Decide deliberately
and say which in the honour/ignore report of slice 1.

**Boks overwrites the header; `sbx` substitutes the sentinel.** For API keys Boks does
`h.Set(r.header(), r.headerValue(v))` (`secret.go:694`) — the guest's placeholder is discarded
and the header is replaced wholesale, and the doc at `:659-661` says the overwrite is
deliberate. `LIFE §12` describes `sbx` differently: "the proxy swaps the literal
`proxy-managed` value for the real one per request." Boks only does true string substitution
for OAuth, in `OAuth.substitute` (`oauth.go:387-412`), and **only on a TLS flow**
(`secret.go:700-708`), so an OAuth token is never written onto plaintext. The consequence for
kits: a credential that does not ride in a header — a query parameter, a request body, a
non-standard envelope — is expressible in neither model, but Boks' `Set` model additionally
does nothing useful if the guest put the sentinel somewhere other than the declared header.
That is a narrower contract than the kit docs imply, and it is the right one; just do not
promise sentinel-swap semantics for `apiKey`.

`apiKey.name` ↔ `Service.EnvName`: "the environment variable the guest's own client reads the
credential from. It is what makes `boks secret set <service>` enough on its own."

`oauth.tokenEndpoint` ↔ `Service.TokenEndpoint` + `TokenEncoding`.
`oauth.sentinels.{accessToken,refreshToken}` ↔ `secret.Sentinels{Access,Refresh}`
(`oauth.go:133`). `oauth.responseFields` ↔ Boks' `ResponseFields` (`oauth.go:622` reads
`o.ResponseFields.access()`). `oauth.credentialFile.template` ↔ Boks' rendered
credential file (`CredentialFileData` in `oauth.go:419-426` supplies `AccessToken`,
`RefreshToken`, `ExpiresAt` in unix ms, `ExpiresAtSeconds`, `Scopes`, `SubscriptionType` — no
field a real token can occupy — rendered with `Option("missingkey=error")`).
`oauth.resourceHosts` ↔ `OAuth.ResourceHosts []policy.Pattern`, which like `Inject.Domain`
refuses `IsAny()`. `oauth.sentinels` are validated by `validSentinel` (`oauth.go:320-331`):
≥16 characters "so a substitution cannot fire on coincidence", no newline, access ≠ refresh.

**`oauth.credentialFile.path` cannot be honoured as written, and the reason is structural.**
Boks writes its rendered credential file into `secret.GuestCredentialDir = "/etc/boks/
credentials"` (`import.go:202`), e.g. `/etc/boks/credentials/claude-code.json`
(`import.go:228`), and materialises it as a **host directory mounted read-only** at that path
(`enforce.go:332-372`), telling the guest where it is via a `BOKS_CREDENTIAL_FILE_<SERVICE>`
environment variable (`enforce.go:367`). `import.go:193-202` explains why the agent's own
config directory is not used:

> A dedicated directory, not the agent's own config directory, and the reason is a real
> constraint rather than taste: Boks shares *directories*, read-only (`internal/workspace`
> refuses to share a single file, because the runtime implements a file bind mount by exposing
> its parent). Rendering straight to `~/.claude/.credentials.json` would therefore mount the
> whole of `~/.claude` read-only and break every other thing the agent keeps there.

`~/.claude/.credentials.json` is **exactly** the path `ANAT`'s own OAuth example declares. So a
faithful implementation of `credentialFile.path` either mounts the agent's whole config
directory read-only — breaking the agent — or copies the file in at start, which
`import.go:200-202` says "belongs to the image." Boks must redirect the path into
`GuestCredentialDir` and expose the location by environment variable, and say so rather than
appearing to honour the field.

**Close, but different in a way that matters.**

*Sentinel provenance.* The kit spec has the **author** write the literal sentinel:
`apiKey.proxyManaged: true` sets the in-guest value to the literal string `proxy-managed`, and
`oauth.sentinels.accessToken` is a hand-written string like `sk-ant-oat01-proxy-managed`. Boks
**mints** sentinels from `Service.KeyPrefix` and `Service.KeyLength`, and
`internal/secret/service.go:41` explains why in terms that make the kit approach look like a
bug:

> Every configured row carries the observable prefix and length of a real key for that vendor,
> because the guest holds a *fake* and clients validate credential format locally: `gh` checks
> a token's prefix, SDKs check lengths, Claude Code refuses an OAuth token that does not start
> with `sk-ant-oat01-`. All of that happens inside the guest, before any request reaches the
> proxy that would have replaced the value — so a placeholder spelled "boks-managed" breaks the
> tool invisibly.

`secret.NewSentinel(prefix, seed, length)` (`oauth.go:646-660`) mints one: the vendor's real
prefix, an embedded `boksproxymanaged` marker, the seed, and fixed-alphabet padding to the
vendor's observed key length, deterministic in the seed so a restarted sandbox presents the
same value. Claude Code's shapes are recorded as `AccessPrefix: "sk-ant-oat01-"`,
`RefreshPrefix: "sk-ant-ort01-"`, `SentinelLength: 108` (`import.go:233-235`).

So `proxyManaged: true` must map to **"mint a sentinel for this service"**, not to "write the
literal string `proxy-managed`". A kit-declared `oauth.sentinels.*` should be *validated*
against `validSentinel` and used, since the author picked it to satisfy a specific client's
format check — but a bare `proxyManaged: true` with no vendor row in Boks' registry means Boks
has no `KeyPrefix` to mint from, and the honest outcome is a refusal in the same spirit as the
existing one: `service.go:38` registers an unsourceable service "**with its name and no
rule**, exactly as `kiro` is registered as an agent with no image. Asking for it says what is
missing rather than 'unknown service'."

*Provenance requirement.* `Service.Source` is documented as required for a configured service,
"in the same spirit as the Why on an agent's allow rule: a rule nobody can check is a rule
nobody can correct." A kit-supplied service has no such citation. Either `sourceURL` from the
kit stands in, or kit-supplied services are marked as uncited in `boks secret ls`. This is a
small decision with a real UX consequence and should be made deliberately.

*Scoping.* `docs/roadmap.md` records: "**A credential cannot be scoped to one sandbox.** A
stored credential applies to every sandbox; `--no-secrets` turns all of them off for a run, and
there is nothing in between." The kit model's stage-2 resolution puts **sandbox-scoped secret
store first, global second** (`BIND`). So per-sandbox credential scoping is a prerequisite for
faithful kit credential semantics, and it is an existing known gap rather than something kits
introduce.

**Genuinely new.**

1. **The bindings file and the domain intersection.** Boks has no `~/.config/sbx/
   credentials.yaml` analogue: no `bindings[<service>].allowedDomains`, no named variants
   (`github@work-org-a`), no workspace-`remembered` map, no `discovery[]` with `env:`/`file:`
   + `parser: "json:<dotted.path>"`. The one piece Boks **must** have before honouring any
   third-party kit's `credentials[]` is `allowedDomains`: it is the user-side half of the
   intersection that stops a kit from choosing where the user's key gets sent. Everything else
   (variants, remembered, discovery) is ergonomics Boks can defer, and Boks' encrypted store
   plus `boks secret adopt` already covers the "where does the value come from" question that
   `discovery[]` answers.
2. **A kit-declared service Boks has never heard of.** Boks' registry is compiled in; a kit
   declares a service at load time. This is the one place where kits genuinely change the trust
   model of `internal/secret`, because today every injection rule in Boks was written by a Boks
   author with a citation.
3. **`passthrough: true`** — a kit asking for the real OAuth token to enter the guest. Boks has
   no equivalent knob, and this is **already a decided position**, not an open question:
   `internal/secret/acquire.go:47` quotes the field by name — "Docker's kit format documents
   `passthrough: boolean to skip sentinel masking` on its oauth [block]" — and argues Boks
   should not have such a switch; `docs/security-model.md:664` records the same. Honouring the
   field would reverse a documented decision. See H5.
4. **`credentialFile.template` as a Go template supplied by remote input.** Boks renders its
   own credential documents from code today. A `text/template` from a fetched file is remote
   input reaching a template engine; harmless for injection but it can name any path (`path:`
   with `~` expansion) — a mixin overwriting the parent's OAuth credential file is one of the
   sensitive-path cases `PIT §17` says implementations warn loudly about.

`docs/docker-sandbox-parity.md §2d` already records the injection shape and the OAuth story;
this section adds the sentinel-provenance inversion, the `Source` requirement, and the
intersection gate as the concrete deltas. **[correction to 2d]** — 2d lists `proxyManaged` as
an `apiKey` field without noting that `ANAT` claims it was removed; `REF`, `SPEC` and all 35
kits say 2d is right and `ANAT` is wrong.

### 4c. What a kit must produce to become a Boks agent

`internal/agent/agent.go:45`. An agent is:

```go
type Agent struct {
    Name    string          // how the user asks for it, and the first half of the sandbox name
    Summary string          // one line for help output
    Image   string          // OCI reference for the guest rootfs; "" = known name, no environment
    Init    []string        // prefix put in front of everything else in Argv
    Command []string        // argv the sandbox starts with; empty = the image's default
    Args    ArgsMode        // how arguments after `--` combine with Command
    Env     []string        // KEY=VALUE; nothing is inherited from the host
    Allow   []Destination   // destinations this agent cannot work without
}
```

with `Agent.AllowRules() []policy.RuleSpec` (`:99`), `Runnable()` = `Image != ""` (`:111`),
`Argv(extra []string)` (`:122`), and a `Registry` with `Add`/`Lookup`/`Resolve`/`All`
(`:150`–`:216`). `Builtin()` (`:309`) constructs the compiled-in set.

A `kind: sandbox` kit must therefore produce:

| `Agent` field | From the kit |
|---|---|
| `Name` | `name`, **narrowed**. A kit name is 1–64 chars of lowercase alnum + hyphen; `agent.nameRe` is `^[A-Za-z0-9]+(?:[._-][A-Za-z0-9]+)*$` capped at `maxAgentNameLength = 32` (`agent.go:144`, `:180`), because the name becomes the first segment of the containerd sandbox name. Every kit name is charset-valid, but a 33–64-character one is **not** a valid agent name. Refuse at load with the length named. |
| `Summary` | `displayName` + `description` |
| `Image` | `sandbox.image`. **Boks should require a digest here**, since `internal/*` pulls images by digest elsewhere and a kit's `:latest` is unpinned remote input naming a rootfs |
| `Command` | `sandbox.entrypoint` + `sandbox.command.default` (non-TTY) or `.interactive` (TTY) |
| `Args` | an `ArgsMode` **[inferred]**; the kit format has no equivalent field, so pick one and document it. `command.{default,interactive}` is a *mode* split, which `ArgsMode` is not — the two-mode split is the genuinely new thing here |
| `Env` | `environment.variables` rendered `KEY=VALUE`, minus the reserved prefixes, plus the sentinel for each `credentials[].apiKey.name` |
| `Allow` | `permissions.network.allow` translated per §4a(4). `Destination` carries a reason — use the kit's `name` and any YAML comment is unavailable, so the reason is "declared by kit <name>" |
| `Init` | **nothing in the kit maps to this**, and that is a real gap — see below |

Two structural mismatches.

**`Init` has no kit equivalent, and the existing code path for a custom image already strips
it.** `agent.go:54`:

> `Init` is a prefix put in front of everything else in Argv… It exists because a sandbox does
> not use the image's `ENTRYPOINT`: the OCI spec is built with containerd's `WithProcessArgs`,
> which replaces the whole argv. An image that installs the Boks CA on the way in would
> therefore be bypassed.

Concretely, `initArgv` (`agent.go:280`) is
`["/usr/bin/tini", "-s", "--", "/usr/local/bin/boks-entrypoint"]`, and `boks-entrypoint`
installs the Boks CA when `BOKS_CA_CERT_B64` is set. `Agent.Argv(extra)` (`agent.go:122-131`)
clones `Init` as the prefix; the sole caller is `internal/cli/common.go:350`
(`command := run.Argv(agentArgs)`), and `common.go:343` calls `run = run.Bare()` immediately
before it — and `Bare()` (`agent.go:116-119`) **nils `Init`**, because the prefix names paths
only a Boks image has.

`Bare()` fires when the user supplied their own `-template` image, which is *exactly* the
situation a `kind: sandbox` kit creates. So this is not a mistake someone might make: the
existing path, followed literally, strips the CA installer for every kit-supplied image. The
failure is silent and compound — no CA in the guest means TLS interception cannot work, and
`internal/proxy/proxy.go:315-320` shows the quiet outcome: when a credential rule names a host
but no CA is configured, the flow is carried in `ModeForwardBypass` and the note reads "a
credential rule names this host, but no certificate authority is configured, so the flow is
carried blind and nothing is injected." Slice 3 must decide explicitly whether a kit-supplied
image is *required* to satisfy Boks' `Init` contract (tini plus `boks-entrypoint` present, per
the image contract of §1.7) or whether Boks refuses credential injection into a kit image
altogether. This is the sharpest single mapping hazard in 4c; see H7.

**`Env` is closed in Boks and open in a kit.** `agent.go:67`: "Nothing is inherited from the
host: an agent gets exactly what its definition asks for." Good news — a kit's
`environment.variables` fits that model exactly. But the kit contract also expects the
*runtime* to inject `SBX_CRED_<SERVICE>_MODE`, `PROXY_CA_CERT_B64`, `NODE_EXTRA_CA_CERTS`,
`SSL_CERT_FILE`, and friends (`SPEC §9.5`). Boks will need its own spelling of those (Boks
already sets proxy/CA vars **[inferred]** from the `Init`/CA machinery). Since a kit is
forbidden from setting `SBX_*`, a Boks-native prefix decision is needed: honour `SBX_CRED_*`
for kit compatibility, or use `BOKS_CRED_*` and break every kit that reads the documented
name. Recommend honouring `SBX_CRED_<SERVICE>_MODE` verbatim — it is a documented contract
kits already branch on (`KIT/*` install scripts, `PIT §5`), and Boks exists for parity.

**Registration lifetime.** `Registry.Add` suggests a kit-derived agent can simply be added at
startup. But `resolve.go` documents that agent allows are "re-derived from the registry every
time rather than recorded on the sandbox — so an entry removed from a later Boks stops applying
to sandboxes that already exist." For a compiled-in agent that is a feature. For a kit-derived
agent it means a sandbox created from `--kit ./x` becomes unstartable, or silently changes, if
the kit file moves or changes. Either record enough of the resolved agent on the container (as
§4a(3) argues for the policy half) or refuse to start a sandbox whose kit is gone, with a clear
message. Do not fall back to a default.

### 4d. Files, volumes, ports

**Static files.** Boks has exactly one host→guest file channel: `sandbox.Copy`
(`internal/sandbox/copy.go:36`), a tar stream through an exec'd `tar` in the guest. Its own doc
comment names the two constraints: it "needs `tar` in the guest image — every usual base image,
including busybox-based ones, has it — and **a running sandbox**." Host-side extraction "treats
the archive as hostile: entries that would land outside the destination are refused rather than
written" — which is the right posture and is the direction that matters *less*; the
guest-bound direction is where a kit's `files/` tree needs the same treatment, and `LIFE §2`'s
symlink/absolute/`..` rules are the load-time half of it.

"Requires a running sandbox" matches the kit model (static files land at M5, post-start), so
`Copy` is the right primitive for both `files/home/` and `files/workspace/`. Host→guest runs
`/bin/sh -c 'mkdir -p "$1" && exec tar -xmf - -C "$1"'` with the target passed as **argv**, not
interpolated, so a path with spaces cannot become shell syntax (`copy.go:72`) — worth preserving
when the paths start coming from a fetched file.

The alternative seam is the **host-directory-mounted-read-only** pattern, used twice already:
the CA (`enforce.go:459-473`) and the credential files (`enforce.go:332-372`, whose doc says "It
is the CA mechanism, reused deliberately: a host-side directory mounted read-only, not a file
baked into an image"). It regenerates on every start and the guest cannot modify it — which is
right for `/etc`-shaped content and wrong for `files/home/`, where the agent must be able to
write afterwards. The hard constraint that decides between them: **`workspace.Parse` refuses to
share a single file** (`workspace.go:72-76`) because the runtime implements a file bind mount by
exposing its parent. Per-file placement therefore needs `Copy`, or a dedicated directory of its
own. Use `Copy` for both `files/` targets.

**The workspace path is not a constant in Boks, and this changes `files/workspace/` materially.**
`internal/workspace/guestpath.go:56` `guestPath`: on a POSIX host it is the **identity** — "the
host path *is* a Linux path, so the workspace is mounted at it verbatim and every absolute path
keeps working on both sides." On Windows, `C:\Users\dag\src\foo` → `/c/Users/dag/src/foo`.

Consequences: (a) `${WORKDIR}` substitution in `setup.files[].content` must use the computed
guest path per sandbox, which on POSIX equals the host path — so a kit config file baked at
startup contains a host-specific absolute path, which is fine but means the file is not
portable between sandboxes; (b) `files/workspace/<path>` in direct-mount mode writes **straight
into the user's real directory on the host**, and Boks' default is direct mount. `PIT §17`
already flags this for `sbx` ("the kit's write modifies the **host** file. Anything the user
edited between restarts gets clobbered the next time the sandbox starts") and says the CLI
emits a banner-style warning rather than refusing. In Boks the same hazard applies with no
`--clone` escape hatch by default.

`internal/sandbox/clone.go` already has the exact hook shape needed:
`ensureClone(ctx, container, task, out)` (`:186`) plus `cloneCommand(dst, owner)` (`:171`) is a
post-start, before-agent step gated on `Filesystem.IsClone()`. `files/workspace/` is the same
shape, sequenced after it.

**Volumes.** No kit-shaped mechanism exists — Boks has **no volume abstraction at all**: no
tmpfs, no block-backed volume, no size. Everything is an OCI bind mount of a host directory,
split between `Config.Workspaces` (part of the sandbox's identity, in labels, shown by
`boks ls`) and `Config.Mounts` ("further host directories… that are not the user's
workspaces — the public half of the interception CA, today", `sandbox.go:103-107`), combined by
`guestShares` (`sandbox.go:593-599`) and applied via `oci.WithMounts` (`sandbox.go:476-478`).
Resources travel as **annotations**, not mounts (`resourceAnnotations`, `sandbox.go:627-638`),
which is also where `sandbox.resources.{cpu,memory}` would land. `volumes[]` is `type: ""`
(block-backed) or `tmpfs`, both M3-only. No shipped kit uses it. `docs/docker-sandbox-parity.md §2d` notes it is
"how the nested Docker data disk is expressed rather than being a special case," and nested
Docker is roadmap item 6 — so `volumes[]` should follow that work rather than lead it. Defer.

**Ports.** `internal/ports/spec.go` already implements sbx's grammar verbatim
(`[[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]`, six protocol spellings, loopback-only
default, "Nothing is opened implicitly"). A kit's `ports[]` entry — `{container, protocol,
name}` with an always-ephemeral loopback host port — is a strict subset: it maps to a `ports`
spec with HOST_IP and HOST_PORT both omitted. Two deltas: the kit's `name` label has no home
in Boks' spec type, and the package's stated principle — "Publishing is per-port,
per-sandbox and **requested by a human**, and it is built as one hole at a time rather than by
turning on a mechanism" — is in direct tension with a fetched file requesting holes. A kit's
`ports[]` should be surfaced and confirmed, not silently opened; loopback-only keeps the blast
radius small either way.

---

## 5. Sliced implementation plan

Six slices. Each is independently shippable and independently useful. Verification is stated in
terms of what can run on a machine with no hypervisor.

**What can be verified here, generally.** Everything host-side: parsing, normalization,
validation, policy resolution, pattern matching, credential-rule construction, and the proxy
itself (`internal/proxy` has host-side tests; `internal/enforce` too). Boks' integration suite
(`internal/sandbox/*_integration_test.go`) drives a **real containerd** behind
`BOKS_INTEGRATION=1` and logs a warning rather than failing when the runtime is not the
isolating one — so guest-side behaviour is testable against containerd+runc *if one is present*,
and only the VM boundary itself needs the hypervisor. Neither is present on this machine, so
everything below marked "guest" is unverifiable here.

### Slice 1 — Read a kit and say what Boks would do with it

**Delivers** `internal/kit`: reference classification, directory + ZIP loading, strict YAML
decode forked on `schemaVersion`, v1→canonical normalization with a warnings channel, and
`ValidateArtifact`. Plus `boks kit inspect <ref>` printing the canonical form and — the part
that makes it useful on day one — an **honour/ignore report**: for every field, whether this
Boks would apply it, ignore it, or refuse to load.

**Why first, given "riskiest unknowns first."** The riskiest *design* unknown is §4a, and
slice 2 is where it is settled. But §0 shows the docs disagree with each other and with the
shipped library on `mixins`, `locked`, `proxyManaged`, `credentialFile.structure`, `kit add`,
reference pinning, and execution order. Slice 1 turns 35 real kits into a test corpus, which is
what makes every later slice decidable against reality instead of against prose. It is also the
cheapest slice, so paying for it first costs little.

**Touches** new `internal/kit/` (loader, normalize, validate, report); `internal/cli/` for the
`kit` command group.

**Verification, no hypervisor.** Table tests over all 35 `contrib/*/spec.yaml`: every one loads,
none produce warnings (all are v2), and the honour/ignore report is golden-filed. Negative
tests for each documented refusal: unknown field, `schemaVersion: "3"`, `kind: mixin` with a
`sandbox:` block, `build` without `image`, `locked` containing brackets (matching
`validate_test.go:397`), a `files/` symlink escaping the root, an absolute static-file path, a
`..` traversal, a `setup.install[].command` given as a list, a `setup.files[].content` with a
placeholder other than `${WORKDIR}`, a reserved `SBX_*` env name. Hand-written v1 fixtures for
each fold in `REF`'s v1→v2 table.

**Cannot be verified here.** Nothing. This slice is pure host-side Go.

### Slice 2 — The kit policy layer (the risky one)

**Delivers** a `kit <name>` layer in `policy.Request`/`Resolution`, placed after `agent` and
before `global`; the `PresetLocked` skip with the empty-but-visible layer; the `ToRequest`
round-trip case; `SandboxPolicy` carrying the resolved kit specs; and the pattern translator of
§4a(4), including a **new single-label wildcard pattern kind** in `internal/policy/pattern.go`.
Surfaced as `boks policy ls --kit <ref>` — read a kit, show exactly what it would add and what
it could never add, before running anything.

**Touches** `internal/policy/resolve.go` (new layer, `ToRequest`, `SandboxPolicy`),
`internal/policy/pattern.go` (single-label kind), `internal/kit/` (translator),
`internal/cli/` (`policy ls --kit`).

**Verification, no hypervisor.** All of it is unit-testable, and the existing suite already has
the shape: `resolve_test.go` and `policy_test.go` exist. Required assertions:
a kit allow is beaten by a global deny, a sandbox deny, and a `--deny` flag; the kit layer
contributes **zero** rules under `--policy locked` and still appears in `Describe()` with its
reason; `ToRequest(store)` round-trips a resolution containing kit rules without losing them;
`*.example.com` from a kit does **not** match `a.b.example.com`; a bare `api.example.com` from
a kit does **not** match port 80; `**.example.com` from a kit is refused or warned; a kit deny
is honoured under every preset including `open`. Then run the translator over the union of all
`permissions.network` entries in the 35-kit corpus and assert every one either translates or
produces a named, actionable diagnostic — `KIT/crush` alone has ~30 entries including
`*.openai.azure.com`, which is the single-label case.

**Cannot be verified here.** That the proxy actually denies at the wire — but
`internal/proxy/proxy_test.go` is host-side and covers the three `CheckMode` stages, so this is
verifiable in CI without a guest. Genuinely unverifiable: that the *guest's* resolver and the
proxy agree, i.e. `internal/sandbox/policy_integration_test.go`, which needs containerd.

### Slice 3 — `kind: sandbox` → an agent, image and argv only

**Delivers** `boks run --kit ./x .` for a kit that only names an image, an entrypoint, a
command, and env vars. No `setup`, no `files/`, no credentials. This is the roadmap's "let an
agent be defined in a file rather than in code" reduced to its smallest honest form.

**Touches** `internal/agent/agent.go` (construct an `Agent` from a kit; keep `Init`),
`internal/cli/run.go` + `create.go` (the `--kit` flag, currently absent per
`docs/roadmap.md`), `internal/sandbox/lifecycle.go` (record the kit on the container).

**Verification, no hypervisor.** Unit tests that a kit produces the expected `Agent`, and
specifically that `Argv(extra)` still begins with Boks' `Init` prefix — assert the CA-install
prefix is present, since dropping it is the §4c hazard. Test the TTY/non-TTY `command` split
against `ArgsMode`. Test that a kit whose `sandbox.image` carries no digest is refused (or
warned, if that is the chosen policy). Test that a sandbox recorded with a kit refuses to start
when the kit is gone rather than falling back.

**Cannot be verified here.** That the image actually boots, that the entrypoint runs, that
uid 1000 and passwordless sudo hold, that `HTTP_PROXY` survives `sudo`. All need a guest;
the last three need a guest running a *kit-supplied* image, which Boks has never done — note
that `docs/roadmap.md` already records "Only the base image has run in a microVM."

### Slice 4 — `setup` phases and static files

**Delivers** `setup.install` (once, uid 0, `sh -c`, synchronous, before the entrypoint),
`setup.startup` (every start, uid 1000, `background`, dispatched but not gating the
entrypoint), `setup.files` (`${WORKDIR}`, `mode`, `onlyIfMissing`), `files/home/` and
`files/workspace/`, in `REF`'s execution order (§1.4) with `permissions.network` live first.

**Touches** `internal/sandbox/lifecycle.go` (an M4 phase and an M5 phase),
`internal/sandbox/clone.go` (sequence `files/workspace/` after `ensureClone`),
`internal/sandbox/copy.go` (reuse for static trees), `internal/workspace/guestpath.go`
(`${WORKDIR}` source of truth).

**Verification, no hypervisor.** The *plan* is testable even when execution is not: assert the
ordered list of operations Boks would perform for a given kit — a golden "execution plan" —
against `REF`'s six stages, for single kits and for `--kit a --kit b` (concatenation in flag
order). Assert `setup.files` targeting at or under the clone target is refused at create time
(`PIT §18`). Assert `${WORKDIR}` resolves to `guestPath(hostPath, style)` for both styles,
reusing the existing Windows-on-Linux test trick in `guestpath_test.go`. Assert a
`files/workspace/` path that collides with an existing host file produces the §4d warning.

**Cannot be verified here.** Everything about actual execution: uid 0 vs 1000, `sh -c`
semantics, idempotency on restart, `background` not gating the entrypoint, the
`/etc/durable-startup.d/`-equivalent surviving a stop/start, `tar` present in a kit-supplied
image, ownership restoration after a root install writes into `/home/agent`. All need
containerd at minimum.

### Slice 5 — Credentials

**Delivers** `credentials[]` → `secret.Service` + `[]secret.ServiceInject`; sentinel minting
rather than literal `proxy-managed`; `oauth` mapped onto Boks' existing endpoint/sentinel/
response-field machinery and `GuestCredentialDir`; a user-side `allowedDomains` gate; and
per-service refusal when Boks cannot mint a credible sentinel.

**Touches** `internal/secret/service.go` (accept runtime-registered services),
`internal/secret/oauth.go` (`credentialFile.template` rendering),
`internal/secret/import.go` (guest file placement), `internal/proxy/proxy.go` (nothing, if the
`Injector` interface is unchanged — verify), plus a new bindings store for `allowedDomains`.

**Verification, no hypervisor.** This is the slice that verifies *best* without a guest,
because the proxy is host-side. Drive `internal/proxy` with a kit-derived `Injector` and assert:
the header and format land exactly as `inject[]` declared; injection happens **only** for the
intersection of `inject[].domain` and `allowedDomains`, with a non-fatal skip otherwise
(`PIT §14`); `Pattern.IsAny()` refuses a catch-all injection domain (`pattern.go:127`
documents this as the case where the check matters most); a minted sentinel satisfies
`validSentinel` and carries the vendor's documented prefix; a kit declaring a service with no
Boks registry row and no derivable prefix is registered name-only rather than guessed at;
`passthrough: true` is refused or requires an explicit user opt-in. Golden-test the
`credentialFile.template` render against `KIT/*` OAuth kits.

**Cannot be verified here.** That the guest's client accepts the minted sentinel — the whole
point of `KeyPrefix`/`KeyLength` is a check that runs *inside* the guest, before any request
reaches the proxy. That needs a real agent binary in a real guest and is the highest-value
guest test in the whole feature.

### Slice 6 — Composition, last and deliberately

**Delivers** `--kit` stacking with the merge rules of §3.2, the three composition errors of
§3.4, `requires.agent` enforcement, and `extends` resolution.

**Why last.** Upstream does not implement `mixins` (`CODE types.go:675`), no shipped kit uses
`extends`, `mixins`, or `locked`, and there is no readable merge implementation anywhere to
copy or check against. Building it early means building against prose alone. `--kit` stacking
of *independent* mixins is the useful 80% and needs only list concatenation plus the duplicate
-name and duplicate-credential errors; `extends` and `locked` can wait until a real kit needs
them.

**Touches** `internal/kit/` (compose), `internal/cli/run.go` (repeated `--kit`).

**Verification, no hypervisor.** Synthetic fixtures for each row of §3.2 and each error of
§3.4, plus order-sensitivity tests (`--kit a --kit b` vs `b a` differing in
`environment.variables` winner, `files` overlay winner, and install order). Assert `locked` is
either enforced or explicitly reported as unenforced — do not ship a field that looks like a
governance control and is not one.

**Cannot be verified here.** Nothing new; composition is host-side.

### Not in any slice

`volumes[]` (follow nested Docker, roadmap item 6). `sandbox.build` (not implemented upstream
either). `args` / `${{ kit.args.* }}` (pre-decode substitution; no shipped kit uses it). MCP
gateway variables. `sbx kit push`/`pull`/`pack` as a publishing story.

---

## 6. Hazards — things that would be silently wrong

**H1. A kit is remote input that runs commands as uid 0.** `setup.install[].command` is a shell
string run via `sh -c` as root, and `KITS` says so as the stated rationale for the source
allowlist. So the whole `kit.allowedSources` / `kit.allowLocalKits` mechanism is not a
convenience — it is the only thing between a URL and root in the guest, and Boks needs its
equivalent **in slice 1, not later**. Two specific traps in copying it: the allowlist must
match on **path-segment boundaries** (`github.com/docker/` must not admit
`github.com/docker-evil/`), and `settings set` **replaces the whole list**, so a user adding one
publisher silently drops `docker.io/` unless the UI says so. Boks' own default should be
narrower than Docker's, because Boks has no Docker Hub relationship to lean on:
**[inferred]** default to local-only, with every remote source opt-in.

**H2. Unpinned remote references, and the pinning rule that does not exist.** `DIST` says Git
refs MUST be 40-hex SHAs and OCI refs MUST be digests; `KITS` accepts `#ref=v0.1.0` and
`ghcr.io/myorg/my-kit:1.0`, and `DIST` itself concedes the rule is not "enforced everywhere by
the consumer CLI." There is **no** signature or checksum field anywhere in the schema. So the
sbx behaviour Boks would inherit by copying the product docs is: fetch a mutable tag, run its
install commands as root. `docs/docker-sandbox-parity.md §2e` already draws the right
conclusion from Boks' own habits (`NERDBOX_REV` pins a commit, tarballs pin SHA-256, images
pull by digest) — carry it through: require a pin, or require an explicit per-invocation
acknowledgement, and never resolve a mutable ref silently.

**H3. A kit's allow widening a policy the user pinned.** Three distinct ways, all silent:

- *`--policy locked` defeated.* If the kit layer does not copy the agent layer's
  `PresetLocked` skip, `boks run --policy locked --kit ./x` allows whatever `./x` lists.
  `resolve.go` states the principle for agents — "an agent's own API being quietly exempt would
  defeat exactly the thing they asked for" — and a third-party file deserves *less* trust than
  a compiled-in agent, not more.
- *Wildcard breadth.* A kit's `*.example.com` means one label; Boks' `patternSuffix` means any
  number (`pattern.go:113`). Import it verbatim and `a.b.evil.example.com` is reachable when
  the author allowed only `api.example.com`. The kit's own `**.example.com` — the form that
  *does* mean this — is documented as **not enforced** upstream, so no kit author has ever had
  to think about the difference.
- *Port breadth.* A kit's bare host means port 443; Boks' bare host means every port. Import
  it verbatim and you have added a plaintext downgrade path, the exact thing
  `internal/secret/service.go` says it pins ports to 443 to avoid.

Each of these turns a *narrowing* intent into a *widening* effect, which is the worst direction
for a security control to fail in.

**H4. Kit rules vanishing, or reappearing changed, at the wrong moment.** Two halves of the
same mistake. `Resolution.ToRequest` discards any rule whose `Scope` it does not recognise, so
an unhandled kit layer disappears on the first supervisor hot-reload and the sandbox loses
network to the hosts the kit needs — an availability failure that will look like a network bug.
Conversely, if kit rules are *re-derived* from the kit at every start (which is what
`resolve.go` deliberately does for agent allows), then editing or re-publishing a kit changes
the policy of a sandbox that already exists, and `boks start` becomes a moment when remote
input can widen containment. Agent allows can be re-derived because they ship inside the
binary. Kit allows cannot. Record the resolved specs.

**H5. Credential injection putting a secret in the guest.** Boks' design keeps credentials on
the host; three kit fields push the other way and each needs an explicit decision rather than a
faithful implementation.

- **`passthrough: true`** does exactly this on purpose: "the proxy returns the real OAuth
  response to the container instead of swapping in sentinels" (`ANAT`). A kit can ask for it and
  the schema has no `passthroughReason` field, so there is nothing to display. Boks has
  **already decided against it** — `internal/secret/acquire.go:47` names the field and argues
  Boks should not have the switch, and `docs/security-model.md:664` records the same. The hazard
  is therefore not "should we add it" but a kit loader honouring it by default and reversing a
  written decision. Refuse the field; do not silently ignore it either, because a kit that needs
  it is a kit that will not work.
- **`credentialFile`** writes a document into the guest. With sentinels that is fine and is what
  Boks already does at `/etc/boks/credentials`. But the path is kit-chosen with `~` expansion,
  and `PIT §17` lists overwriting a parent kit's OAuth credential file among the cases
  implementations "warn (loudly)" about, alongside `~/.ssh/**` and shell rc files. Boks should
  refuse those paths outright rather than warn — a warning in a create-time log is not a
  control.
- **The container-resident model.** `PIT §13` documents that AWS SigV4 forces the real
  credential into the container, "restricted by `permissions.network.allow`." So a kit can
  legitimately require a real secret in the guest, bounded only by an allow list that H3 shows
  can be misimported. `KIT/crush` is the concrete artefact: its `aws` credential declares
  `format: AWS4-HMAC-SHA256 Credential=%s/` across eight Bedrock hosts, which cannot produce a
  valid SigV4 signature at a proxy that never sees the canonical request. It is a header-shaped
  placeholder for something the model cannot do. Do not read a shipped kit's `format` as
  evidence that the mechanism works.

**H6. The sentinel written literally.** `proxyManaged: true` is documented as setting the guest
variable to "the proxy-managed sentinel," and the kit examples show the literal string
`proxy-managed`. `internal/secret/service.go:41` explains at length why a literal breaks the
guest's client *before* any request reaches the proxy — `gh` checks prefixes, Claude Code
refuses a token not starting with `sk-ant-oat01-`. A faithful implementation of the kit field
produces tools that fail locally with no proxy log entry to explain it. Map `proxyManaged` to
Boks' minting, and refuse when there is no vendor row to mint from.

**H7. A kit-supplied image silently loses the Boks CA, on the existing code path.** This is the
one hazard where the naive implementation is *already written*. `Agent.Bare()`
(`agent.go:116-119`) nils `Init`, and `internal/cli/common.go:343` calls it whenever the user
supplied their own `-template` image — which is exactly what a `kind: sandbox` kit is. `Init` is
`["/usr/bin/tini","-s","--","/usr/local/bin/boks-entrypoint"]` (`agent.go:280`) and
`boks-entrypoint` is what installs the CA from `BOKS_CA_CERT_B64`. Without it there is no CA in
the guest, so TLS cannot be terminated, so nothing is injected — and the symptom is not a crash
but `proxy.go:315-320`'s `ModeForwardBypass` note, "a credential rule names this host, but no
certificate authority is configured, so the flow is carried blind and nothing is injected."
Every credential a kit declares then does nothing, quietly. Either require kit images to carry
the `Init` binaries (which the §1.7 contract nearly already demands of any kit image) and stop
calling `Bare()` for kit-derived agents, or refuse credential injection for kit images and say
so at create time.

**H8. `files/workspace/` writing to the host.** Boks' default is direct mount and its guest
workspace path is the host path verbatim on POSIX (`guestpath.go:56`). So a kit's
`files/workspace/.editorconfig` overwrites the user's real `.editorconfig`, on **every** start,
in their real repository. `PIT §17` documents that `sbx` warns rather than refuses. A warning is
the wrong control for silent data loss in a user's git working copy: refuse when the target
exists and is tracked, and require an explicit opt-in.

**H9. A kit setting the proxy variables.** `KITS` flags this as an Important box: setting
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` from a kit "points traffic away from the forward proxy,
so it can no longer apply network policy or inject credentials." The schema *warns* rather than
rejecting for these names (unlike `SBX_*`/`DASH_*`/`DOCKER_*`, which are errors). In Boks the
forward proxy is the entire enforcement point, so this must be a **refusal**, not a warning.
The same applies to `PROXY_CA_CERT_B64`, `NODE_EXTRA_CA_CERTS`, `SSL_CERT_FILE`,
`REQUESTS_CA_BUNDLE`, and `PIP_CERT`: `SPEC §9.5` says kit content "MAY read them and MUST NOT
overwrite them," and a MUST NOT with no enforcement is a hazard.

**H10. `locked` read as a governance control.** `docs/docker-sandbox-parity.md §2d` calls it
"how an organisation pins a network rule that a derived kit cannot loosen." Today it is a
validated-but-unenforced string list (§3.3) that cannot even name an array element, and it
applies only to `extends`, not to `--kit` — the direction untrusted kits actually arrive from.
Anyone reasoning "kits are safe because an org can lock the network rules" is reasoning about a
feature that does not exist in any readable implementation. Boks' equivalent already exists and
is better: a deny in any scope beats every allow, and a global deny is stored on the machine
rather than declared by a kit.

**H12. `credentialFile.path` honoured literally breaks the agent it was written for.** The
field's own canonical example is `~/.claude/.credentials.json`. Boks cannot mount a single file
(`workspace.go:72-76` — a file bind mount exposes its parent), so honouring the path means
mounting the whole of `~/.claude` read-only, which breaks every other thing Claude Code keeps
there. `import.go:193-202` already worked this out and chose a dedicated
`/etc/boks/credentials` directory plus a `BOKS_CREDENTIAL_FILE_<SERVICE>` pointer. The hazard is
a loader that reads `path:` and does what it says. Redirect and report; do not honour.

**H13. Two silent precedence inversions inside credentials.** First, `apiKey` vs `oauth`: the
kit spec says the API key wins with OAuth as fallback, and `secret.PreferOAuth`
(`service.go:427-459`) drops the API-key credential whose destinations OAuth already covers.
A kit author who declared both gets the opposite of what the schema promised, with no
diagnostic — and because `PreferOAuth` matches on **destinations rather than names**, the drop
can happen for a credential the author never connected to the OAuth entry. Second,
`environment.variables` last-wins across `--kit` order (H15) is the same class of silence.
Neither is wrong as a policy; both need to be *reported*, because a credential that resolved
differently than declared is indistinguishable from one that failed.

**H14. `permissions.network` compiled after `setup.install`.** Stated in §1.4 but repeated here
because it is a whole-feature failure rather than a subtle one: `KIT/vale`, `KIT/crush` and
essentially every other install-bearing kit reaches the network from a root shell during
`setup.install`, and the kit contract puts network permissions at stage 1 for exactly that
reason. In Boks the policy is compiled once at start and handed to the supervisor over a pipe
(`enforce.Spec` on stdin, `supervisor.go:296-302`), so "compile the policy, then run install" is
the natural order — but any design that defers the kit layer until after the container is up
inverts it, and the symptom is every kit failing to install with an opaque proxy denial.

**H15. `--kit` order as a silent behaviour switch.** `COMP` is explicit that flag order changes
the `environment.variables` winner, the `files` overlay winner, and install/startup order — and
that `environment.variables` last-wins is silent (`PIT §9`). Two invocations differing only in
flag order produce different sandboxes with no diagnostic. Whatever Boks builds in slice 6
should report the winner whenever two kits set the same variable or the same file path, rather
than inheriting the silence.
