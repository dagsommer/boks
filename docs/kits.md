# Kits

A kit is a file that declares what a sandbox may do — the network destinations it needs, the
tools to install, the files to drop in — so that the same setup can be applied to any sandbox
without being typed out each time. The format is [Docker Sandboxes'](https://docs.docker.com/ai/sandboxes/customize/kits/),
and a kit written for `sbx` is a kit Boks reads.

> **What Boks does with a kit today:** its network rules, the agent a `kind: sandbox` kit
> defines, and the API-key and OAuth credentials it declares — after you approve them.
>
> `setup.install`, `setup.files` and `setup.startup` run inside the guest after it starts
> (install and files once per sandbox, startup once per boot; a failed install is retried on the
> next run). The `files/` tree is applied too: `files/home/` into `/home/agent` (overwriting, owned by the
> agent) before the install commands, and `files/workspace/` into the primary workspace last,
> **never overwriting a file that is already there** — in a direct mount that is your own disk.
> Symlinks in the tree are skipped. `ports` and `volumes` are applied at creation (see below).
> `--kit` repeats: kits are stacked, and `extends` and `mixins` are followed (see
> [Stacking kits](#stacking-kits)).
> Anything else Boks does not apply is named by the run, and so is every example below that
> shows a field Boks
> ignores. Why, and what comes next, is in [kits-design.md](kits-design.md).

### Credentials need your approval

A kit names a service and the hosts its credential is sent to; the value is whatever you
stored under that name. So before a kit's credentials are used, Boks shows them and asks:

```console
$ boks run team-copilot . --kit git+ssh://git@ghe.example/team/kits.git#dir=copilot

The kit "copilot" wants to use these credentials:
  ghe-token    API key → sent to api.ghe.example, ghe.example   (stored)
  wiki-token   API key → sent to wiki.example                   (not set)
  ghe-login    OAuth login → sent to api.ghe.example, auth.ghe.example   (stored)
[A]pprove all · [R]eview each · [N]o (default):
```

An approval is remembered for that kit and that service, and only for exactly what the kit
declared: the hosts, header and format of a key, or the whole shape of a login (endpoints,
hosts, sentinels, credential file). If the kit later adds a host, you are asked again. A credential you
do not approve is left out and the sandbox still starts.

With no terminal to ask on, nothing is approved. Run once interactively to remember the
answer, or pass `--approve-kit-credentials` to approve for that one run. `--inject` is
unaffected: what you type yourself is never asked about.

### A sandbox remembers its kits

A sandbox is brought back up by commands that take no `--kit` — `boks start`, `boks exec` on a
stopped sandbox, `boks run --name NAME`, or a plain `boks run claude .` in the same directory.
So the sandbox records the kits it was created with, and those commands read them again:

- **Recorded:** the `--kit` references as you typed them, where each is read from on a restart,
  and a digest of what they composed to — in the container's `dev.boks.kits` label, never the
  spec itself. A git reference whose `#ref=` is a branch or tag is recorded **pinned to the
  commit that was fetched**, so a branch that moves cannot change what a restart reads; a
  relative path is recorded as an absolute one, because `boks start` is run from anywhere.
- **Applied again on every start, exec or re-attach:** the credentials, and the
  `setup.startup` commands (once per boot, as before). Install commands and files stay marker-based:
  once per sandbox.
- **Credentials stay behind the approval.** There is no prompt on `start` or `exec`, and nothing is
  approved anew. A credential is attached only if the approval stored in
  `kit-credential-approvals.json` still matches what the kit asks for; otherwise it is left out
  and the way to approve it (`boks run --name NAME` at a terminal) is printed. `boks run` at a
  terminal asks, as it does with `--kit`.
- **A kit that can no longer be read** — a directory that moved, a repository that is gone — is a
  warning that says what is therefore missing, and the sandbox comes up without it. A kit that
  changed on disk since (a local directory is not pinned) is applied as it now is, with a note;
  a changed credential request is asked about again, not attached.
- **`--kit` on a re-attach** has to be the kit the sandbox was created with. A different one is
  refused, in the style of `--cpus` or `--net`, naming both; remove the sandbox to change it.
  Naming no `--kit` is fine and means the recorded ones.
- **Not retrofitted.** A sandbox created before this, or without `--kit`, records nothing; a
  `--kit` on a later run still applies for that run only, as it always did. Recreate the sandbox to
  have it remembered.
- A running sandbox is not re-read by `start` or `exec`: whatever booted it applied its kit.

`boks create --kit` records the kits the same way and, like `boks run`, accepts a `git+…`
reference in the agent's place and registers the agent a `kind: sandbox` kit defines. It also
applies the kit's volumes, ports and environment at creation; its setup runs when the sandbox
first comes up.

**Unverified:** none of this has run against a real guest. What is tested is the record and its
round trip, the reload, the approval gate on a restart, the refusal of a different kit, and the
setup configuration rebuilt from a sandbox's labels.

### OAuth credentials

A kit's `oauth` block describes a login; it carries no token. Boks applies it to the OAuth
credential you stored under the same service name (`boks secret adopt NAME` or
`boks secret login NAME`), and the tokens stay in the store:

```yaml
credentials:
  - service: ghe-login
    oauth:
      tokenEndpoint: {host: auth.ghe.example, path: /oauth/token}
      sentinels: {accessToken: "ghe-access-0123456789", refreshToken: "ghe-refresh-0123456789"}
      resourceHosts: [api.ghe.example]
      credentialFile:
        path: ~/.ghe/credentials.json
        template: '{"token":"{{.AccessToken}}","refresh":"{{.RefreshToken}}"}'
```

- The kit's token endpoint, resource hosts, sentinels and `responseFields` replace those of the
  stored login for this run. The client id, request encoding and headers stay the stored
  login's, because the kit grammar has no spelling for them.
- `credentialFile` is rendered on the host, from sentinels only, and shared read-only into the
  guest. The kit's `path` is **redirected**: the file lands in `/etc/boks/credentials/` as
  `SERVICE-<file name>`, and `BOKS_CREDENTIAL_FILE_<SERVICE>` names it, because Boks shares
  whole directories and cannot mount your tool's own config directory. Put it where the tool
  looks from `setup.startup`. Templates may use `.AccessToken`, `.RefreshToken`, `.ExpiresAt`,
  `.ExpiresAtSeconds`, `.Scopes`, `.ScopesJSON` and `.SubscriptionType`. `structure` is not
  supported (the same is true of sbx's engine); use `template`.
- **`passthrough: true` is refused**, and the run fails naming the credential. It would put the
  real token in the sandbox, which Boks never does.
- **An entry that declares both `apiKey` and `oauth` uses the API key**, as the kit spec says;
  its `oauth` is not applied. This is decided within the entry. A login you adopted separately
  still takes precedence over a key for the same hosts, as it does outside kits.
- If nothing is stored under that name as an OAuth login, the credential is not attached and
  the run says how to store one. `--oauth NAME` on the command line wins over the kit.
- `skipIfEnv` is accepted and ignored.

## Using a kit

Point `--kit` at a directory containing `spec.yaml`, or at the file itself (repeat the flag to stack kits):

```console
$ boks run claude . --kit ./my-kit
$ boks run claude . --kit ./my-kit/spec.yaml
```

The same flag works on `boks create` (which also takes a `git+…` reference in the agent's place),
and on the two commands that answer questions about
policy — which is how you see what a kit does *before* running it:

```console
$ boks policy ls --kit ./my-kit
$ boks policy check --kit ./my-kit https://api.example.com
```

### What a kit may not loosen

A kit's allow rules are added to the sandbox's policy as their own layer, labelled with the
kit's name. They are **additions only**. A deny — from `--deny`, from a global rule, from a
sandbox-scoped rule — beats them, always:

```console
$ boks policy check --kit ./vale --deny api.github.com api.github.com:443
DENY  api.github.com:443
  policy: standard+kit:vale+local (default deny)
  rule:   api.github.com
  scope:  flag --deny
  reason: denied by rule "api.github.com"
```

A destination the kit allows, with nothing denying it, names the kit as the source:

```console
$ boks policy check --kit ./vale objects.githubusercontent.com:443
ALLOW objects.githubusercontent.com:443
  policy: standard+kit:vale (default deny)
  rule:   objects.githubusercontent.com
  scope:  kit vale
```

This is not a special case for kits; it is the rule the whole policy engine is built on, and
it is why a kit can be applied to a locked-down sandbox without being able to widen it. Under
`--policy locked` the kit layer is dropped entirely and listed as dropped, because "locked"
means only what you wrote.

### Where a rule came from

Every layer is labelled, so a destination is always traceable to the thing that asked for it:

```console
$ boks policy ls --kit ./vale
policy standard+kit:vale
  kit vale                  2 rule(s)  what this kit declares it needs; a deny in any scope still wins
  api.github.com                 kit vale         declared by kit vale
  api.github.com:443             preset standard  GitHub REST and GraphQL APIs
  objects.githubusercontent.com  kit vale         declared by kit vale
```

An agent's allowlist is compiled into Boks and auditable by reading the release. A kit is a
file on your disk. When something is reachable, `boks policy ls` names which of the two
permitted it.

## Stacking kits

`--kit` may be given more than once. The kits are applied in the order of the flags, which is
the merge order: it decides which kit's environment variable wins, which kit's file lands, and
which install command runs first.

```console
$ boks run claude . --kit ./vale --kit ./no-telemetry
$ boks kit inspect --resolved ./vale ./no-telemetry   # what that does, without running it
```

| What | Across stacked kits |
|---|---|
| `permissions.network.allow` / `deny` | Appended, duplicates removed. A deny in any kit beats an allow in any other, as every deny does. Each rule is labelled with the kit that declared it in `boks policy ls`; the layer as a whole is named after the stack (`kit:vale,no-telemetry`). |
| `environment.variables` | Union; **the later kit wins**, silently in the run and marked `(overridden by KIT)` in `kit inspect --resolved`. |
| `setup.install`, `setup.files`, `setup.startup` | Concatenated in kit order. |
| `files/home/`, `files/workspace/` | One overlay; a later kit's file replaces an earlier kit's at the same path. |
| `credentials` | Every kit's are asked about, **each kit on its own**: an approval is keyed by kit name and reference, so approving `vale` never approves another kit's credential. The same `service` in two kits is an error, even if they are identical. |
| `ports` | Appended. `volumes` are united by `path`; the later kit wins. |
| `security.privileged` | Any `true` is `true`. |
| `kind: sandbox` | At most one kit may define the agent; a second is an error. It may sit anywhere in the order. A mixin's `agentInstructions` never becomes the agent's. |
| `name` | Must be unique across the whole stack, including kits pulled in by `extends` and `mixins`. |
| `requires.agent` | A mixin that names an agent is refused unless the sandbox runs that agent (`claude-vertex` is not `claude`). A kit-defined agent also counts as the kits it extends. Checked when the agent is known: `run`, `create`, and `policy ls`/`check` with `--agent`. |

The execution order inside the guest is the one in Docker's reference, with each stage applied
in `--kit` order: network and environment, `files/home/`, `setup.install`, `setup.files`,
`setup.startup`, then `files/workspace/`. `kit inspect --resolved` prints it:

```console
$ boks kit inspect --resolved ./agent ./tools ./lint
...
execution plan:
1. network and environment
   [agent] allow agent.test
   [agent] env MODE=base (overridden by tools)
   [tools] allow tools.test
   [tools] deny telemetry.test
   [tools] env MODE=tools
2. files/home
   ...
```

### `extends` and `mixins`

A `kind: sandbox` kit may name a parent with `extends` and kits to layer on with `mixins`.
A reference is resolved like `--kit`: a local path relative to the kit that names it (or
absolute), or a git reference. Resolution order is the parent chain merged into the kit
(parent first), then the mixins the chain declares (an ancestor's before a descendant's), then
the next `--kit`.

- An extends chain climbs at most 5 parents, a cycle is an error, and every parent must be
  `kind: sandbox`. A kit with `extends` carries no `sandbox:` block of its own, so it runs the
  parent's image, entrypoint and command unchanged.
- Under `extends` a repeated credential `service` or volume `path` is an error rather than a
  merge, because Docker's own rule is that the merge cannot decide which shape wins. Lists are
  appended parent first, scalars and variables take the child's, and the child's static files
  replace the parent's.
- Every mixin must be `kind: mixin`. They are kits in their own right: their credentials are
  approved separately and they appear in the plan under their own name.
- **A kit that was fetched may refer only to other `git+` references**, never to a local path:
  a remote file does not get to decide what Boks reads from your disk.
- **`locked`**, which Docker validates and does not enforce, is enforced here for `extends`:
  a descendant that sets a path an ancestor locks, at all, fails. That is stricter than any
  source documents, chosen because a lock that does nothing looks like a control. It does not
  apply to stacked `--kit` flags, which are yours.
- A composition holds at most 32 kits.

`boks kit inspect REF` prints one kit's spec as Boks reads it; `--resolved` takes several
references and prints the members in application order, the merged spec, the credentials that
would be asked about, and the execution plan. It reads nothing from the credential store.

## Reference forms

| Form | Example | Boks |
|---|---|---|
| Local directory | `--kit ./my-kit` | **works** |
| Path to a spec | `--kit ./my-kit/spec.yaml` | **works** |
| Git over HTTPS | `--kit "git+https://github.com/org/repo.git#ref=v1.0&dir=vale"` | **works** |
| Git over SSH | `--kit "git+ssh://git@example.com/org/repo.git#dir=vale"` | **works** |
| OCI artifact | `--kit ghcr.io/org/kit@sha256:…` | not yet |
| ZIP | `--kit ./my-kit-1.0.zip` | not yet |

### Git references

`#ref=` takes a branch, a tag or a commit, and defaults to the repository's default branch —
the same grammar `sbx` uses. `#dir=` names the subdirectory holding `spec.yaml`. **Quote the
whole reference**: `&` backgrounds the command in most shells.

```console
$ boks run claude . --kit "git+https://github.com/docker/sbx-kits-contrib.git#ref=v0.1.0&dir=code-server"
kit …: fetched from https://github.com/docker/sbx-kits-contrib.git at commit d4de09c8…
kit …: v0.1.0 is not a fixed commit, so this kit may differ on the next run;
       use #ref=d4de09c88bef5bcf62aa80543a098c1392506b38 to pin it
```

The fetch is shallow and the clone is discarded once the spec is read. `git` does the
fetching, so an SSH agent, a credential helper, `.netrc` and a proxy all work as they do for
any other repository — and a private repository never prompts: if there is no usable
credential it fails rather than hanging on a password prompt you cannot see.

**Boks always reports the commit it resolved, and warns when the reference was mutable.** A
branch or tag can move, so the kit you ran today is not necessarily the one you run tomorrow;
the warning names the exact SHA to write if you want that guaranteed. Docker's normative
specification goes further and *requires* an immutable reference, but its product
documentation says `#ref=<branch|tag|commit>` and uses a tag in its own example — and real
kits are published and referenced by tag, so refusing them would mean a kit written for `sbx`
does not load in Boks. Reporting the commit keeps what the pinning rule is for — knowing
exactly what ran — without refusing the reference.

`git://` is refused: it is unauthenticated and unencrypted, and nothing that sets a sandbox's
network rules should arrive that way.

## Examples

Each section is one `spec.yaml` doing one thing. They are snippets to lift, not complete
kits — for every field, see Docker's [spec reference](https://docs.docker.com/ai/sandboxes/customize/kit-reference/)
and the field table in [kits-design.md](kits-design.md).

### Allow the hosts a tool needs

The one thing Boks applies today. A kit that adds a linter which fetches its rule set:

```yaml
schemaVersion: "2"
kind: mixin
name: vale
version: "1.0.0"
permissions:
  network:
    allow:
      - objects.githubusercontent.com
      - api.github.com
```

```console
$ boks run claude . --kit ./vale -v
sandbox: claude-myrepo (agent claude, ghcr.io/dagsommer/boks/claude:0.1.5)
command: claude --dangerously-skip-permissions
kit: vale (mixin) from ./vale — 2 allow, 0 deny
workspace: /home/me/myrepo → /home/me/myrepo (rw)
image: ghcr.io/dagsommer/boks/claude:0.1.5 (already present)
network: nat · policy standard+agent:claude+kit:vale · … allow, 0 deny
```

Without `-v` a run prints nothing about the kit unless the policy changed. See
[Output](#output).

### Deny a host the agent would otherwise reach

Deny rules narrow, so they take effect exactly as written and cannot be undone by anything the
kit itself allows:

```yaml
schemaVersion: "2"
kind: mixin
name: no-telemetry
version: "1.0.0"
permissions:
  network:
    deny:
      - telemetry.example.com
```

### A kit that defines a whole agent

`kind: sandbox` names an image and an entrypoint — the same thing a Boks agent is, declared in
a file instead of in Go. Boks runs it as an agent named after the kit, with the kit's image,
entrypoint and command.

```yaml
schemaVersion: "2"
kind: sandbox
name: my-agent
version: "1.0.0"
sandbox:
  image: example/my-agent:1.2.3
  entrypoint: ["my-agent"]
  command:
    default: ["--headless"]
    interactive: ["--tui"]
permissions:
  network:
    allow: [api.my-agent.example.com]
```

### Ports and volumes

`ports` and `volumes` are applied.

```yaml
ports: [{container: 8080, protocol: tcp}]
volumes:
  - path: /var/cache/tool           # block-backed: survives stop/start, removed by `boks rm`
  - path: /tmp/scratch
    type: tmpfs                     # RAM-backed
    size: 64m
    mode: "1777"
```

- **Ports.** Each TCP port is published as if you had typed `--publish PORT`: an ephemeral
  host port on `127.0.0.1`, never one the kit picks. Boks prints each one when it publishes,
  and `boks ports NAME` shows the host port. UDP is skipped with a note (Boks publishes TCP
  only). A kit's ports are applied when the sandbox is created; on a sandbox that exists, use
  `boks ports`.
- **Volumes** are attached only when the sandbox is created, because they are part of the
  container's spec. Running a kit with volumes against an existing sandbox prints a note and
  changes nothing; `boks rm` and run again to get them. A block-backed volume is a directory
  under Boks' state directory (`volumes/NAME/`), shared into the guest read-write, so it has
  no size limit (a `size` is reported as not enforced) and `mode` sets that directory's
  permissions. A tmpfs volume is an OCI `tmpfs` mount with `size` and `mode` applied.
  `boks rm` and an ephemeral run's exit delete the block-backed directories.

### Fields Boks parses and does not apply

Valid in a kit, accepted by `--kit`, and with no effect yet. They are listed so that a kit
which relies on them is not mistaken for one that works:

```yaml
setup:
  install:                              # applied once per sandbox, as uid 0 unless `user:` says otherwise
    - command: "apt-get update && apt-get install -y jq"
  startup:                              # applied once per boot, as the agent user; `background: true` detaches
    - command: ["my-daemon"]
      background: true
  files:                                # applied once per sandbox; ${WORKDIR} expands
    - path: /home/agent/.my-tool/config.json
      content: '{"workspace": "{{.Workspace}}"}'
credentials:                            # applied after approval, see above
  - service: my-service
    apiKey:
      name: MY_SERVICE_TOKEN
      inject:
        - domain: api.my-service.com
          scheme: bearer
agentInstructions:                      # not applied
  filename: AGENT.md
  content: "Do not commit without asking."
```

## Output

A run prints nothing about the network by default. How Boks works — that it terminates TLS
for a credential's hosts, and what the policy allows — is documented and askable at any moment
with `boks policy ls`, so volunteering it on every run only teaches people to skip the output.

One thing still speaks up unprompted: **interception getting wider**. A sandbox that decrypted
one host yesterday and three today is a change to what is being read, and a kit, a profile or
a new credential can make that change without the user doing it deliberately.

The image pull reports itself too, without `-v`: it is the longest thing a run does, and a
silent multi-minute wait is indistinguishable from a hang.

Everything else — the policy table, the standing summary, which kit was applied, the command,
the workspace mounts, the network stack's own progress — waits for `-v`.

An interactive run clears the visible screen immediately before the agent's interface starts,
so it does not draw over your shell. Scrollback is left alone, so everything Boks printed is
one scroll away. `BOKS_NO_CLEAR` turns that off.

`-q`/`--quiet` still parses so that scripts written against 0.1.5 keep working. It does
nothing: quiet is the default now.

## Schema versions

Both `schemaVersion: "1"` and `"2"` load. Write `"2"` for anything new; v1 is what older kits
in the wild use, and Boks translates it forward on read.

The two grammars do not mix. A v1 field inside a `schemaVersion: "2"` spec is **rejected**,
not ignored — the same rule Docker's loader follows — so a spec that half-migrated fails
loudly rather than half-applying:

```console
$ boks policy ls --kit ./half-migrated
boks: ./half-migrated/spec.yaml: line 4: field "network" is v1 and not valid in
      schemaVersion "2" (v2 spells this: permissions.network.allow / .deny …)
```

If you are migrating a kit, the field-by-field mapping is in Docker's spec reference under
"What changed in v2", and every row of it is implemented in `internal/kit`.

## What is verified here

Every `console` block above was produced by running the command, on Linux, on 2026-08-19,
against a kit written for the purpose — except the `boks run` banner, whose allow count is
elided because it depends on the preset and the agent. No sandbox was booted: this machine has
no hypervisor, so what is shown is the policy Boks resolves, not a guest reaching a host.

That distinction matters for the deny example. It shows that the *policy engine* refuses the
destination, which is the layer this feature adds. That the guest is then actually unable to
reach it is the network stack's job, verified separately in
[verification.md](verification.md).

## Troubleshooting

**A git reference fails to fetch** — the error carries git's own message, which names the real
cause (no such ref, authentication failed, host unreachable). For a private repository, check
that the credential git would use outside Boks is available: Boks never prompts, so a missing
credential is a failure rather than a hang.

**`unknown fragment key`** — the grammar is `#ref=…&dir=…` and nothing else. `#branch=` is a
common guess and is not it.

**A destination the kit allows is still denied** — something denies it, and a deny always
wins. `boks policy ls --sandbox <name>` shows every layer; the denying rule is named in the
verdict from `boks policy check`.

**The kit's rules are missing entirely** — check the preset. Under `--policy locked` the kit
layer is dropped by design, and `boks policy ls` lists it as dropped rather than hiding it.

**A field in the kit does nothing** — check the list above. Boks applies `permissions.network`
and nothing else today.
