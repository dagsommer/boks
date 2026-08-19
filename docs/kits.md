# Kits

A kit is a file that declares what a sandbox may do — the network destinations it needs, the
tools to install, the files to drop in — so that the same setup can be applied to any sandbox
without being typed out each time. The format is [Docker Sandboxes'](https://docs.docker.com/ai/sandboxes/customize/kits/),
and a kit written for `sbx` is a kit Boks reads.

> **What Boks does with a kit today: its network rules, and nothing else.**
>
> `--kit` applies `permissions.network` and refuses remote references. Everything else in a
> spec — the image, the entrypoint, `setup`, `files`, `credentials`, `ports`, `volumes` — is
> parsed, validated, and **not applied**. The flag says so, and so does every example below
> that shows a field Boks ignores.
>
> This is a deliberate order of work rather than an unfinished feature. Why, and what comes
> next, is in [kits-design.md](kits-design.md).

## Using a kit

Point `--kit` at a directory containing `spec.yaml`, or at the file itself:

```console
$ boks run claude . --kit ./my-kit
$ boks run claude . --kit ./my-kit/spec.yaml
```

The same flag works on `boks create`, and on the two commands that answer questions about
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
a file instead of in Go. **Boks reads this and applies only the network block**; the image and
command are ignored, and `boks run` uses the agent you named on the command line.

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

### Fields Boks parses and does not apply

Valid in a kit, accepted by `--kit`, and with no effect yet. They are listed so that a kit
which relies on them is not mistaken for one that works:

```yaml
setup:
  install:                              # not applied — would run as uid 0 at creation
    - command: "apt-get update && apt-get install -y jq"
  startup:                              # not applied — would run at each start as uid 1000
    - command: ["my-daemon"]
      background: true
  files:                                # not applied
    - path: /home/agent/.my-tool/config.json
      content: '{"workspace": "{{.Workspace}}"}'
credentials:                            # not applied — see internal/secret for what Boks does today
  - service: my-service
    apiKey:
      name: MY_SERVICE_TOKEN
      inject:
        - domain: api.my-service.com
          scheme: bearer
ports: [{container: 8080, protocol: tcp}]   # not applied — use boks ports
agentInstructions:                      # not applied
  filename: AGENT.md
  content: "Do not commit without asking."
```

## Output

A run is quiet by default. Two things print anyway, because they are news rather than
description:

- **A host whose TLS Boks is about to terminate**, the first time a sandbox sees it. Asking
  for less output is not consent to being decrypted silently.
- **A policy that differs from the one this sandbox last ran under**, including the first run
  of a new sandbox.

Everything else — the standing network summary, which kit was applied, the image, the command,
the workspace mounts, the network stack's own progress — waits for `-v`.

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
