# CLI reference

Every command, flag and default below is read out of the command tree at build time, so this
page describes the `boks` you have rather than the one somebody wrote about. The same
text is available as `boks <command> --help`.

**This file is generated. Do not edit it.** It is produced by `make docs` from
[`internal/cli/reference.go`](https://github.com/dagsommer/boks/blob/main/internal/cli/reference.go),
and `go test ./internal/cli/` fails when the checked-in copy is out of date. To change
something here, change the command that produces it.

Four flags for developing Boks itself are accepted by every command and hidden from `--help`:
`--runtime`, `--snapshotter`, `--containerd-address` and
`--i-know-this-is-not-isolated`. They are omitted below for the same reason they are
hidden. The last one turns off the refusal to present a runtime with no VM boundary as a
sandbox, and must never be used to run anything untrusted.

## boks bundle

Write a clone-mode sandbox's commits to a git bundle on the host

```
boks bundle [flags] SANDBOX
```

Writes the commits from a clone-mode sandbox to a git bundle on the host, then prints the
'git fetch' that reads them into your repository.

In clone mode the guest works on its own clone, so its commits are inside the VM and nothing
on the host has seen them. Docker Sandboxes solves this by serving a git daemon from the
sandbox and fetching over the network. Boks does not: a sandbox has no inbound network, and
opening one so that work can leave would be a hole cut through the boundary the mode exists
to provide. A bundle is a single file that 'git fetch' reads exactly like a remote, and it
travels out over the same channel 'boks cp' uses, which needs no listener at all.

A bundle carries commits. Whatever is uncommitted inside the sandbox is not in it, and this
command says so rather than leaving it to be discovered later.

The printed 'git fetch' writes the sandbox's branches under refs/sandboxes/&lt;sandbox&gt;/, so it
cannot move any branch of yours.

```
boks bundle claude-myrepo
  boks bundle claude-myrepo -o /tmp/work.bundle
```

| Flag | Default | Meaning |
|---|---|---|
| `-o`, `--output string` |  | where to write the bundle (default: ./&lt;sandbox&gt;.bundle) |

## boks ca

Inspect or replace the local CA used for TLS interception

```
boks ca
```

Boks generates one local certificate authority, on this machine, the first time a run
needs to attach a credential to an HTTPS request. Injecting a header means reading the
request, which means terminating TLS, which means a certificate the guest accepts.

Only hosts named by a credential rule are ever intercepted. Everything else is tunnelled
with the origin's own certificate chain untouched.

The private key never leaves this machine and is never written into a guest. Install the
certificate in a sandbox, never in your host trust store: in a sandbox its reach is that
sandbox, in your login keychain it is every TLS connection you make.

'boks ca env' prints BOKS_CA_CERT_B64=&lt;base64 certificate&gt;, for runtimes with their own
trust store (Node, Python) that ignore the system one.

### boks ca env

Print the certificate as an environment variable, for runtimes with their own trust store

```
boks ca env [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `--dir string` |  | authority directory |
| `--export` |  | prefix with 'export ' so the output can be sourced |

### boks ca export

Write the CA certificate (the public half) somewhere

```
boks ca export [flags]
```

Writes the CA certificate — the public half, safe to copy anywhere — in PEM form. Most
tooling takes it through an environment variable:

```
  SSL_CERT_FILE, CURL_CA_BUNDLE, REQUESTS_CA_BUNDLE, NODE_EXTRA_CA_CERTS
```

| Flag | Default | Meaning |
|---|---|---|
| `--dir string` |  | authority directory |
| `-o`, `--output string` |  | write to this file instead of stdout |

### boks ca regenerate

Replace the authority; anything trusting the old one stops working

```
boks ca regenerate [flags]
```

Generates a new authority and discards the old one. This is what revocation means here:
there is no revocation list for a guest to check, so retiring an authority is deleting
its key. Certificates already issued chain to something nothing trusts any more.

Anything you gave the old certificate to must be given the new one.

| Flag | Default | Meaning |
|---|---|---|
| `--dir string` |  | authority directory |
| `-q`, `--quiet` |  | print only the new fingerprint |
| `-y`, `--yes` |  | do not ask for confirmation |

### boks ca show

Print the authority's fingerprint, validity and location

```
boks ca show [flags]
```

| Flag | Default | Meaning |
|---|---|---|
| `--create` |  | generate the authority if it does not exist yet |
| `--dir string` |  | authority directory |

## boks completion

Generate the autocompletion script for the specified shell

```
boks completion
```

Generate the autocompletion script for boks for the specified shell.
See each sub-command's help for details on how to use the generated script.

### boks completion bash

Generate the autocompletion script for bash

```
boks completion bash
```

Generate the autocompletion script for the bash shell.

This script depends on the 'bash-completion' package.
If it is not installed already, you can install it via your OS's package manager.

To load completions in your current shell session:

```
	source <(boks completion bash)
```

To load completions for every new session, execute once:

\#### Linux:

```
	boks completion bash > /etc/bash_completion.d/boks
```

\#### macOS:

```
	boks completion bash > $(brew --prefix)/etc/bash_completion.d/boks
```

You will need to start a new shell for this setup to take effect.

| Flag | Default | Meaning |
|---|---|---|
| `--no-descriptions` |  | disable completion descriptions |

### boks completion fish

Generate the autocompletion script for fish

```
boks completion fish [flags]
```

Generate the autocompletion script for the fish shell.

To load completions in your current shell session:

```
	boks completion fish | source
```

To load completions for every new session, execute once:

```
	boks completion fish > ~/.config/fish/completions/boks.fish
```

You will need to start a new shell for this setup to take effect.

| Flag | Default | Meaning |
|---|---|---|
| `--no-descriptions` |  | disable completion descriptions |

### boks completion powershell

Generate the autocompletion script for powershell

```
boks completion powershell [flags]
```

Generate the autocompletion script for powershell.

To load completions in your current shell session:

```
	boks completion powershell | Out-String | Invoke-Expression
```

To load completions for every new session, add the output of the above command
to your powershell profile.

| Flag | Default | Meaning |
|---|---|---|
| `--no-descriptions` |  | disable completion descriptions |

### boks completion zsh

Generate the autocompletion script for zsh

```
boks completion zsh [flags]
```

Generate the autocompletion script for the zsh shell.

If shell completion is not already enabled in your environment you will need
to enable it.  You can execute the following once:

```
	echo "autoload -U compinit; compinit" >> ~/.zshrc
```

To load completions in your current shell session:

```
	source <(boks completion zsh)
```

To load completions for every new session, execute once:

\#### Linux:

```
	boks completion zsh > "${fpath[1]}/_boks"
```

\#### macOS:

```
	boks completion zsh > $(brew --prefix)/share/zsh/site-functions/_boks
```

You will need to start a new shell for this setup to take effect.

| Flag | Default | Meaning |
|---|---|---|
| `--no-descriptions` |  | disable completion descriptions |

## boks cp

Copy files between the host and a sandbox

```
boks cp [flags] SRC DST
```

Copies files and directories between the host and a running sandbox. Exactly one of the two
paths carries a SANDBOX: prefix; copying between two sandboxes is not supported.

The sandbox must be running, and its image must contain 'tar'.

```
boks cp ./config.yaml web:/etc/app/config.yaml
  boks cp web:/var/log/app ./logs
```

## boks create

Create a sandbox without starting it

```
boks create [flags] [AGENT] [PATH...] [-- AGENT_ARGS...]
```

Creates a sandbox without starting it, pulling the image if needed. Use this to get the slow
part out of the way; 'boks run' brings it up and attaches, and 'boks exec' runs commands in
it.

The arguments are the same as 'boks run': the agent first, then the workspaces, which
default to the current directory. Anything after '--' is recorded as the agent's arguments,
and is what 'boks run' executes when it is given none of its own.

\--clone belongs here too, and only here: the mode lives in the sandbox's mounts, so it is
fixed when the sandbox is created and cannot be changed afterwards.

```
Agents:
  shell          a plain shell in the Boks base image
  claude         Claude Code
  codex          OpenAI Codex
  copilot        GitHub Copilot CLI
  cursor         Cursor CLI
  docker-agent   Docker Agent
  droid          Factory Droid
  gemini         Google Gemini CLI
  kiro           Kiro (no image yet — needs --template)
  opencode       OpenCode
```

```
boks create shell .
  boks create --name web shell ~/src/site -- npm run dev
```

| Flag | Default | Meaning |
|---|---|---|
| `--allow stringArray` |  | allow a destination, host[:ports] (repeatable) |
| `--annotation stringArray` |  | extra OCI annotation KEY=VALUE passed to the runtime (repeatable) |
| `--clone` |  | keep guest writes off your disk: work on a git clone made inside the guest, with the host repository shared read-only at /run/sandbox/source |
| `--cpus int` | `0` | vCPUs for the guest (0: all host CPUs) |
| `--deny stringArray` |  | deny a destination, host[:ports] (repeatable); deny always wins |
| `--env stringArray` |  | extra environment variable KEY=VALUE (repeatable) |
| `--guest-credential stringArray` |  | what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable) |
| `--inject stringArray` |  | attach a credential: service@host[,host]=bearer\|basic[:user]\|header[:format] (repeatable) |
| `-m`, `--memory string` |  | guest memory, binary units (1024m, 8g) (default: half the host's, max 32g) |
| `--name string` |  | sandbox name (default: &lt;agent&gt;-&lt;workspace directory&gt;) |
| `--net string` |  | network mode: none (no network at all) or nat (default nat) |
| `--no-secrets` |  | do not attach credentials from the store; only what --inject names |
| `--oauth stringArray` |  | name a stored OAuth credential; stored ones apply anyway, this pins one (repeatable) |
| `--policy string` |  | network policy preset: open, standard, locked (default standard) |
| `--profile string` |  | stored policy profile to apply ('boks policy profile ls') |
| `-p`, `--publish stringArray` |  | publish a sandbox port on the host, bound to loopback (repeatable): [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL] |
| `-q`, `--quiet` |  | suppress the network summary (a new TLS-interception host is still announced) |
| `-t`, `--template string` |  | OCI image for the guest root filesystem (default: the agent's image) |

## boks daemon

Run the containerd that Boks drives

```
boks daemon
```

Starts, stops and inspects a containerd that Boks manages.

A boks binary on its own cannot start a sandbox: it orchestrates containerd, a VM shim, a
hypervisor library and a filesystem tool. containerd is the piece with the most ways to be
present but wrong, and none of them announce themselves — a diff-service order that omits the
erofs differ fails during an image unpack naming a differ, and a ttrpc socket containerd tries
to chown to uid 0 fails at startup naming a file. 'boks daemon start' writes a configuration
that has neither problem and runs containerd with it.

You do not have to run it. 'boks run', 'boks create', 'boks exec' and 'boks start' do it for
you when nothing else is serving, because needing a daemon first is Boks' problem rather than
yours. This command is for starting one on purpose, and for the questions the others cannot
answer: what is running, what it said, and what configuration it was given.

The daemon is Boks' own. Its root, its state and its endpoint are under your state directory,
so it cannot disturb — or be disturbed by — a containerd that Docker or your distribution is
running. Nothing is installed as a service and nothing runs at boot: it is started by a
command you ran, it runs in the background, and 'boks daemon stop' ends it.

Once it is running, Boks talks to it by default. An explicit --containerd-address, or
BOKS_CONTAINERD_ADDRESS, still wins — and pins the choice: Boks starts no daemon of its own
when you have named one.

### boks daemon config

Print the containerd configuration Boks would write

```
boks daemon config
```

Prints the containerd configuration for this host, with the reason for every setting.

It is generated rather than shipped, because three of the settings cannot be written down
ahead of time: the uid and gid are yours, the paths are under your state directory, and
whether the erofs differ may be named at all depends on whether mkfs.erofs is installed —
naming it when it is absent takes the whole daemon down.

This prints what 'boks daemon start' would write now, which is not necessarily what a running
daemon was started with. 'boks daemon status' names that file.

### boks daemon logs

Print the managed containerd's log

```
boks daemon logs
```

Prints what the managed containerd has written, which is everything it logs plus anything
the supervisor said on its way up.

The log belongs to the daemon rather than to a run of this command, so it survives the daemon
exiting: a containerd that died is exactly when this is worth reading, and it is truncated
only when a new one starts.

### boks daemon serve

Run the managed containerd in the foreground

```
boks daemon serve
```

Runs the managed containerd in the foreground and exits when it does. This is the process
'boks daemon start' puts in the background.

Run it by hand to watch a daemon that will not start: containerd's own output arrives on
stderr as it happens, rather than in a log file after the fact. Ctrl-C stops both.

stdout carries one line and nothing else — the marker that says containerd is serving — because
'boks daemon start' reads it.

### boks daemon start

Start the containerd Boks manages, if it is not already running

```
boks daemon start
```

Starts a containerd configured for Boks, and waits until it answers its own API before
reporting success — a socket that exists is not a daemon that is serving.

It returns to your prompt: the daemon is a background process with no terminal of its own, and
this command is finished once that process is serving. There is no flag for that because there
is no other mode; 'boks daemon serve' is the one that stays in the foreground, and it exists
for watching a daemon that will not start.

Starting one that is already running is not an error and does not restart it: this command
means "make sure it is up", and a restart would take down every sandbox the daemon is serving.
'boks run' does the same thing by itself, so this is only needed to start one on purpose.

If containerd refuses to start, what it said is printed here rather than left in a log file
for you to find.

### boks daemon status

Report on the containerd Boks manages

```
boks daemon status
```

Reports whether a Boks-managed containerd is running, and asks it for its version.

Those are two questions, and this command asks both on purpose. A supervisor holding its lock
says a process is alive; a version returned over the socket says containerd is actually
serving. They can disagree, and a status that collapsed them would call a daemon that answers
nothing "running".

Exits non-zero when no managed daemon is serving, so it can gate a script.

### boks daemon stop

Stop the containerd Boks manages

```
boks daemon stop
```

Stops the managed containerd. Stopping one that is not running is not an error.

containerd's root is left alone, so the images it has already pulled are still there when it
is started again. Only 'boks daemon' state — the socket and the record of the running process
— is removed. That root is where the disk goes, and 'boks purge' is what removes it.

## boks doctor

Check host prerequisites for running sandboxes

```
boks doctor [flags]
```

Checks the host for everything a sandbox needs, and explains how to fix what is missing.
Exits non-zero if sandboxes cannot start.

Which containerd, runtime and snapshotter are checked follows --containerd-address,
\--runtime and --snapshotter, the developer flags described in 'boks --help'.

## boks exec

Run an additional command inside a sandbox

```
boks exec [flags] SANDBOX COMMAND [ARG...]
```

Runs a command inside a sandbox, alongside whatever is already in it. The command inherits
the sandbox's environment and working directory, and its exit code becomes boks'. A stopped
sandbox is started first.

Flags must come before the sandbox name; everything after the name belongs to the guest, so
'boks exec web ls -l' sends -l to ls rather than to boks.

```
boks exec web ls -l
  boks exec -it web sh
  boks exec -w /tmp web -- git status
```

| Flag | Default | Meaning |
|---|---|---|
| `-e`, `--env stringArray` |  | extra environment variable KEY=VALUE (repeatable) |
| `-i`, `--interactive` |  | keep stdin open |
| `-t`, `--tty` |  | allocate a pseudo-terminal |
| `-u`, `--user string` |  | user to run as, UID or UID:GID |
| `-w`, `--workdir string` |  | working directory inside the sandbox |

## boks inspect

Print sandbox details as JSON

```
boks inspect [flags] SANDBOX...
```

Prints everything Boks knows about a sandbox, as JSON: status, image, runtime, snapshotter,
creation time, workspaces, filesystem mode, default command, environment and process id.

"filesystem" is the one to read first. In "direct" mode the workspace is shared read-write at
its host path and guest writes land on your disk. In "clone" mode the host repository is
shared read-only at "source" and the guest works on a clone at "clone"; the workspace entry
still names the host directory the sandbox was created for, which in that mode is what was
cloned rather than what the guest writes to.

## boks ls

List sandboxes

```
boks ls [flags]
```

Lists sandboxes. A sandbox exists from 'boks create' or 'boks run' until 'boks rm';
stopped ones keep their filesystem and are listed too.

Also spelled: `list`

| Flag | Default | Meaning |
|---|---|---|
| `--json` |  | print the full listing as JSON |
| `-q`, `--quiet` |  | print only sandbox names |

## boks net

Inspect or stop the network stack serving a sandbox

```
boks net
```

A sandbox's network is a host-side stack that terminates the guest's virtual NIC, with a
filtering proxy listening inside the sandbox's own virtual network. It runs in a process of
its own so that it lasts as long as the sandbox's VM does rather than as long as the command
that started it: a build running in a sandbox does not lose the network because you pressed
Ctrl-C in another terminal.

One process per running sandbox, started on demand and never at boot. It exits when the
sandbox's task exits, so 'boks stop' takes it with the sandbox.

'boks net serve' is what the others spawn. It is a normal command rather than a hidden one
so that the background process can be reproduced, watched and debugged.

### boks net ls

List the running sandbox network stacks

```
boks net ls
```

Also spelled: `list`

### boks net serve

Run one stack in the foreground, reading its configuration from stdin

```
boks net serve < spec.json
```

Runs one sandbox's network stack in the foreground: the host-side stack that terminates the
guest's NIC, and the filtering proxy inside the sandbox's virtual network. It exits when the
sandbox's task exits, or on SIGTERM.

The specification arrives on stdin as JSON, because it carries the credential values the
proxy attaches to requests and those must not be visible in the process table.

### boks net stop

End a sandbox's network stack

```
boks net stop SANDBOX...
```

## boks policy

Show network policy rules and recent decisions

```
boks policy
```

A network policy is what a sandbox may reach. It is durable state, not an argument to a
run: rules written here survive the command that wrote them and are what 'boks run',
'boks start' and 'boks exec' all serve a sandbox. A rule applies to every sandbox, or is
scoped to one with --sandbox.

Precedence, in one sentence: a deny in any scope beats an allow in any scope, and only the
base preset — chosen by 'policy init', a profile, or a --policy flag — decides what happens
to a destination no rule mentions.

### boks policy allow

Add an allow rule, globally or scoped to one sandbox

```
boks policy allow [flags] DESTINATION...
```

Stores a rule. It applies to every sandbox unless --sandbox or --profile scopes it, and it
survives the command that wrote it: this is the policy 'boks run', 'boks start' and
'boks exec' all serve a sandbox.

A deny in any scope beats an allow in any scope. A sandbox-scoped rule can add access the
machine's policy already tolerates and can take access away, but it can never widen past a
deny someone wrote down.

```
Destinations:
  github.com              any port
  github.com:443          one port
  api.example.com:80,443  several
  *.example.com:443       any subdomain, not the apex
  10.0.0.0/8              an address range
  [::1]:8080              an IPv6 literal with a port
```

```
boks policy allow github.com:443 --note "git over HTTPS"
  boks policy allow --sandbox claude-myproject api.example.com:443
```

| Flag | Default | Meaning |
|---|---|---|
| `--note string` |  | why this rule exists; shown by 'boks policy ls' |
| `--profile string` |  | scope the rule to a stored policy profile |
| `--sandbox string` |  | scope the rule to one sandbox instead of all of them |

### boks policy check

Report whether a destination would be permitted, without contacting it

```
boks policy check [flags] DESTINATION...
```

Reports whether a destination would be permitted, which rule decides it, and how the flow
would be carried. Nothing is contacted: this reads the stored policy and answers from the
same engine the sandbox's network stack uses.

A destination with no port is checked on 443.

The flow mode assumes a client that uses the proxy, which is what HTTP and HTTPS clients in
a sandbox do by default. A client that ignores HTTP_PROXY is judged in the network stack
instead — mode transparent, on the address in the packet — where hostname rules cannot apply
at all, so a hostname-only policy denies it.

Credential rules are not recorded on a sandbox, so pass --inject to see the mode a
credential-bearing host would get.

```
boks policy check github.com:443
  boks policy check --sandbox web api.example.com:443
  boks policy check --policy locked --allow example.com:443 example.com:443
```

| Flag | Default | Meaning |
|---|---|---|
| `--agent string` |  | include the allowlist this agent's definition carries (shell, claude, codex, copilot, cursor, docker-agent, droid, gemini, kiro, opencode) |
| `--allow stringArray` |  | allow a destination, host[:ports] (repeatable) |
| `--deny stringArray` |  | deny a destination, host[:ports] (repeatable); deny always wins |
| `--guest-credential stringArray` |  | what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable) |
| `--inject stringArray` |  | attach a credential: service@host[,host]=bearer\|basic[:user]\|header[:format] (repeatable) |
| `--net string` |  | network mode: none (no network at all) or nat (default nat) |
| `--no-secrets` |  | do not attach credentials from the store; only what --inject names |
| `--oauth stringArray` |  | name a stored OAuth credential; stored ones apply anyway, this pins one (repeatable) |
| `--policy string` |  | network policy preset: open, standard, locked (default standard) |
| `--profile string` |  | stored policy profile to apply ('boks policy profile ls') |
| `--sandbox string` |  | check as this sandbox, including rules scoped to it |

### boks policy deny

Add a deny rule; deny always wins

```
boks policy deny [flags] DESTINATION...
```

Stores a rule. It applies to every sandbox unless --sandbox or --profile scopes it, and it
survives the command that wrote it: this is the policy 'boks run', 'boks start' and
'boks exec' all serve a sandbox.

A deny in any scope beats an allow in any scope. A sandbox-scoped rule can add access the
machine's policy already tolerates and can take access away, but it can never widen past a
deny someone wrote down.

```
Destinations:
  github.com              any port
  github.com:443          one port
  api.example.com:80,443  several
  *.example.com:443       any subdomain, not the apex
  10.0.0.0/8              an address range
  [::1]:8080              an IPv6 literal with a port
```

```
boks policy deny github.com:443 --note "git over HTTPS"
  boks policy deny --sandbox claude-myproject api.example.com:443
```

| Flag | Default | Meaning |
|---|---|---|
| `--note string` |  | why this rule exists; shown by 'boks policy ls' |
| `--profile string` |  | scope the rule to a stored policy profile |
| `--sandbox string` |  | scope the rule to one sandbox instead of all of them |

### boks policy init

Create the durable policy store

```
boks policy init [flags]
```

Creates the durable policy store. Boks works without one — an uninitialised machine
resolves to the built-in deny-by-default preset — so this exists to choose a base posture
and to give you a file to read.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--force` |  | overwrite an existing policy store, destroying every rule in it |
| `--preset string` | `standard` | base posture: open, standard, locked |

### boks policy inspect

Show a scope or a single rule in detail

```
boks policy inspect [flags] [DESTINATION...]
```

With no arguments, describes the stored policy: where it lives, what version it is, the
base posture and every scope in it.

With a destination, describes the rules bearing on it: every scope that covers it, and what
the engine would decide once the scopes are put together.

```
boks policy inspect
  boks policy inspect --sandbox web
  boks policy inspect github.com:443
```

| Flag | Default | Meaning |
|---|---|---|
| `--profile string` |  | scope the rule to a stored policy profile |
| `--sandbox string` |  | scope the rule to one sandbox instead of all of them |

### boks policy log

Show recent policy decisions

```
boks policy log [flags]
```

Shows recent network policy decisions: what was allowed or denied, how the flow was
carried, and why. Decisions are written by the sandbox's network stack and by the proxy
inside it. The log stays on this machine and is never uploaded anywhere.

Identical decisions are collapsed into one row with a count, because a single dependency
install produces hundreds of them and the one denial that explains a failure should not be
buried. Use --raw for the unaggregated form.

The log is one file for the whole machine, so a run you are debugging is mixed in with every
other sandbox and with this morning. --sandbox and --since narrow it: --since takes a
duration (30m, 2h) or a time (2026-08-13, 2026-08-13T09:30:00Z).

The PROXY column is the part to read when you care about confidentiality:

```
  forward          boks handled this at the HTTP level and could read it — plaintext
                   HTTP, or HTTPS terminated because the host has a credential rule
  forward-bypass   tunnelled untouched; end-to-end TLS, boks saw ciphertext only
  transparent      judged in the network stack, by address and port, without the proxy
                   being involved at all — what a raw socket or a non-HTTP protocol
                   produces. boks saw a destination, and nothing else.
```

| Flag | Default | Meaning |
|---|---|---|
| `--file string` | `~/.local/state/boks/policy-log.jsonl` | decision log file |
| `-n`, `--limit int` | `500` | show at most this many decisions (0 for all) |
| `--raw` |  | one line per decision instead of one per destination |
| `--sandbox string` |  | only decisions from this sandbox |
| `--since string` |  | only decisions this recent: a duration (30m, 2h) or a time |

### boks policy ls

Show the rules a policy resolves to

```
boks policy ls [flags]
```

Two things, because they are two halves of one question. First what is written down —
the stored rules, by scope — and then what they resolve to for a run, deny rules first
because they always win. Nothing here contacts the network or a sandbox.

The --policy, --allow, --deny and --profile flags resolve a hypothetical: they show what a
run given those flags would get, on top of what is stored.

```
Presets:
  open      allow everything except this machine's own loopback and link-local addresses
  standard  deny by default; allow a small set of package registries and source hosts over HTTPS
  locked    deny everything; every destination must be added with --allow
```

| Flag | Default | Meaning |
|---|---|---|
| `--agent string` |  | include the allowlist this agent's definition carries (shell, claude, codex, copilot, cursor, docker-agent, droid, gemini, kiro, opencode) |
| `--allow stringArray` |  | allow a destination, host[:ports] (repeatable) |
| `--deny stringArray` |  | deny a destination, host[:ports] (repeatable); deny always wins |
| `--guest-credential stringArray` |  | what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable) |
| `--inject stringArray` |  | attach a credential: service@host[,host]=bearer\|basic[:user]\|header[:format] (repeatable) |
| `--net string` |  | network mode: none (no network at all) or nat (default nat) |
| `--no-secrets` |  | do not attach credentials from the store; only what --inject names |
| `--oauth stringArray` |  | name a stored OAuth credential; stored ones apply anyway, this pins one (repeatable) |
| `--policy string` |  | network policy preset: open, standard, locked (default standard) |
| `--profile string` |  | stored policy profile to apply ('boks policy profile ls') |
| `--sandbox string` |  | resolve as this sandbox, including rules scoped to it |
| `--stored` |  | print only the stored rules, without resolving them |

### boks policy profile

Manage named policies a run can select

```
boks policy profile
```

A profile is a named policy: a base preset plus rules. 'boks run --profile NAME' selects
one, so a posture worth reusing is written once instead of retyped as a wall of flags.

```
Rules are added to a profile with the ordinary commands:
  boks policy allow --profile ci proxy.golang.org:443
```

A profile decides the posture a run starts from. It cannot unsay a deny: the global and
per-sandbox rules still apply on top of it, and a deny in any of them wins.

#### boks policy profile create

Create a profile

```
boks policy profile create [flags] NAME
```

Creates a named policy. Rules can be given here or added later with
'boks policy allow --profile NAME'.

```
boks policy profile create ci --preset locked --allow proxy.golang.org:443 \
      --description "dependency fetch only"
```

| Flag | Default | Meaning |
|---|---|---|
| `--allow stringArray` |  | allow a destination in this profile (repeatable) |
| `--deny stringArray` |  | deny a destination in this profile (repeatable) |
| `--description string` |  | what this profile is for |
| `--preset string` |  | base preset: open, standard, locked (default standard) |

#### boks policy profile ls

List the stored profiles

```
boks policy profile ls
```

Also spelled: `list`

#### boks policy profile rm

Delete a profile

```
boks policy profile rm NAME...
```

#### boks policy profile show

Print one profile and what it resolves to

```
boks policy profile show NAME
```

### boks policy reset

Restore the defaults, destroying stored rules

```
boks policy reset [flags]
```

Restores the defaults, destroying stored rules. With no scope it clears everything: the
global rules, every sandbox's rules and every profile, and returns the base posture to the
default preset. With --sandbox or --profile it clears only that scope.

It asks first unless -f is given. Sandboxes that are already running keep the policy they
started with; this changes what the next run resolves to.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--force` |  | do not ask for confirmation |
| `--profile string` |  | scope the rule to a stored policy profile |
| `--sandbox string` |  | scope the rule to one sandbox instead of all of them |

### boks policy rm

Remove a stored rule

```
boks policy rm [flags] DESTINATION...
```

Removes stored rules. Without --action, both dispositions for the destination go; with it,
only the one named. A destination is matched as the engine sees it, so "GitHub.com:443"
removes the rule stored as "github.com:443".

```
boks policy rm github.com:443
  boks policy rm --sandbox web --action allow api.example.com:443
```

| Flag | Default | Meaning |
|---|---|---|
| `--action string` |  | remove only the allow or only the deny for this destination |
| `--profile string` |  | scope the rule to a stored policy profile |
| `--sandbox string` |  | scope the rule to one sandbox instead of all of them |

## boks ports

List, publish and unpublish a sandbox's ports

```
boks ports SANDBOX [flags]
```

Publishes a port inside a sandbox on the host, and lists what is published.

```
  --publish     [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL]
  --unpublish   [HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL]
```

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

Unpublishes are applied before publishes, so one invocation can move a port.

```
boks ports web --publish 3000            # an ephemeral host port -> 3000 in the sandbox
  boks ports web --publish 8080:3000       # 127.0.0.1:8080 -> 3000
  boks ports web --publish 127.0.0.1:8080:3000/tcp
  boks ports web                           # what is published now
  boks ports web --json
  boks ports web --unpublish 8080:3000
```

| Flag | Default | Meaning |
|---|---|---|
| `--json` |  | Output in JSON format (for port listing) |
| `--publish stringArray` |  | Publish a port (can be repeated): [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL] |
| `--unpublish stringArray` |  | Unpublish a port (can be repeated): [HOST_IP:]HOST_PORT:SANDBOX_PORT[/PROTOCOL] |

## boks proxy

Run the host forward proxy on its own, outside any sandbox

```
boks proxy [flags]
```

Runs the host forward proxy: HTTP and HTTPS (via CONNECT) are filtered against a network
policy, and credentials are attached to requests for the hosts they name, without the value
ever existing inside a sandbox.

The credentials are the ones in the store — anything stored under a service boks knows is
attached with no flag at all, exactly as it would be in a sandbox — plus whatever --inject
names. --no-secrets leaves the store out.

Hosts a credential names are the only ones whose TLS is terminated: for those, and only
those, the proxy presents a certificate from the local boks CA, verifies the origin itself,
and can read the traffic. Every other destination is tunnelled untouched, with the origin's
own certificate chain intact. 'boks policy log' shows which was which.

Point a client at it with HTTP_PROXY/HTTPS_PROXY. Nothing is wired into 'boks run'.

```
boks proxy --policy standard
  boks proxy --policy locked --allow api.example.com:443 -v
  boks proxy --inject 'my-api@api.example.com=header:x-api-key'
```

| Flag | Default | Meaning |
|---|---|---|
| `--allow stringArray` |  | allow a destination, host[:ports] (repeatable) |
| `--ca string` |  | certificate authority directory (default: the one 'boks ca' uses) |
| `--deny stringArray` |  | deny a destination, host[:ports] (repeatable); deny always wins |
| `--guest-credential stringArray` |  | what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable) |
| `--inject stringArray` |  | attach a credential: service@host[,host]=bearer\|basic[:user]\|header[:format] (repeatable) |
| `--listen string` | `127.0.0.1:0` | address to listen on |
| `--log string` | `~/.local/state/boks/policy-log.jsonl` | append decisions to this file |
| `--net string` |  | network mode: none (no network at all) or nat (default nat) |
| `--no-intercept` |  | never terminate TLS; credential rules then apply to plaintext HTTP only |
| `--no-secrets` |  | do not attach credentials from the store; only what --inject names |
| `--oauth stringArray` |  | name a stored OAuth credential; stored ones apply anyway, this pins one (repeatable) |
| `--policy string` |  | network policy preset: open, standard, locked (default standard) |
| `--profile string` |  | stored policy profile to apply ('boks policy profile ls') |
| `--secrets string` |  | encrypted secret store (default: the one 'boks secret' uses) |
| `-v`, `--verbose` |  | print every decision as it is made |

## boks purge

Remove the host-side state Boks has written, and report what it frees

```
boks purge [flags]
```

Removes what Boks has written outside its own installation, and prints what that is and
how much it frees before removing anything.

Almost all of it is containerd's: compressed image blobs in its content store, and those same
layers unpacked by the snapshotter. One 'boks create' of the base image costs about a
gigabyte, and nothing ever collects it. Uninstalling Boks does not, either — a package
manager owns the files it installed, not the ones a program later wrote — so this is the
command that does.

By default it takes containerd's root and the per-sandbox state, and keeps the four things
you would be upset to lose without being asked: the local certificate authority, credentials
stored with 'boks secret set', the rules added with 'boks policy allow', and the decision log.
\--all removes those too and leaves nothing of Boks on this machine.

It destroys sandboxes even without --all. containerd keeps image layers and each sandbox's
filesystem in one root, so there is no way to drop the images and keep the sandboxes. Run
'boks ls' first; 'boks rm' is the command for one sandbox.

Removal is refused while the managed containerd is running or a sandbox's network is up, so
that nothing is deleted from under a process still using it. Stop them, or pass --force.

Nothing outside the state directory is ever touched, and neither is anything inside it that
Boks did not write: the entries removed come from a fixed list of names, not from walking the
directory, and everything else is reported as left alone.

```
boks purge --dry-run     # what is there, and what it would free
  boks purge               # give the disk back, keep the CA and your rules
  boks purge --all         # leave nothing behind, before uninstalling
```

| Flag | Default | Meaning |
|---|---|---|
| `--all` |  | also remove the CA, stored credentials, policy rules and the decision log |
| `--dry-run` |  | print what would be removed and stop |
| `--force` |  | remove even though the daemon or a sandbox network is running |
| `-y`, `--yes` |  | do not ask for confirmation |

## boks rm

Delete a sandbox and its filesystem

```
boks rm [flags] SANDBOX...
```

Deletes sandboxes and their filesystems. This is not reversible: everything written inside
a sandbox that is not in a shared workspace is gone.

| Flag | Default | Meaning |
|---|---|---|
| `-f`, `--force` |  | remove a running sandbox, killing whatever is inside it |

## boks run

Run an agent in a sandbox, creating or re-attaching to it

```
boks run [flags] [AGENT] [PATH...] [-- AGENT_ARGS...]
```

Runs an agent inside an isolated microVM. The agent comes first and decides what the
sandbox contains; the workspaces follow and default to the current directory. Each
workspace is shared into the guest at the same absolute path it has on the host, and the
first one is the process's working directory. Nothing above them is exposed. A workspace
may carry a ':ro' suffix for a read-only share.

By default the guest writes straight to those directories. --clone changes that: the
workspace must be a git repository, it is shared read-only at /run/sandbox/source, and the
agent works on a clone made inside the guest, so nothing it writes reaches your disk. The
clone carries committed history only, the mode is fixed when the sandbox is created, and
'boks bundle' is how commits come back out.

The sandbox is named &lt;agent&gt;-&lt;workspace directory&gt; and persists. Running the same agent in
the same directory re-attaches to it, so packages installed and files written inside it are
still there; remove it with 'boks rm'. Pass --rm for a sandbox destroyed when the command
exits, or --name to reach a sandbox from anywhere.

What a sandbox is made of is fixed when it is created — the agent, the image, the vCPUs, the
memory, the environment and the network mode all live in the container the runtime builds the
VM from. Passing one of those to a sandbox that already exists is refused rather than quietly
dropped, and the refusal names the value the sandbox has. Remove it, or name a new one.

Arguments after '--' are passed to the agent. For the shell agent they are the command to
run, since that is what arguments to a shell are.

```
Agents:
  shell          a plain shell in the Boks base image
  claude         Claude Code
  codex          OpenAI Codex
  copilot        GitHub Copilot CLI
  cursor         Cursor CLI
  docker-agent   Docker Agent
  droid          Factory Droid
  gemini         Google Gemini CLI
  kiro           Kiro (no image yet — needs --template)
  opencode       OpenCode
```

```
boks run                              # a shell in the current directory
  boks run shell . -- uname -a
  boks run shell ~/src/foo ~/src/lib:ro
  boks run --clone claude ~/src/foo     # the agent works on a clone; your files are read-only
  boks run --name claude-boks           # re-attach by name, from anywhere
```

| Flag | Default | Meaning |
|---|---|---|
| `--allow stringArray` |  | allow a destination, host[:ports] (repeatable) |
| `--annotation stringArray` |  | extra OCI annotation KEY=VALUE passed to the runtime (repeatable) |
| `--clone` |  | keep guest writes off your disk: work on a git clone made inside the guest, with the host repository shared read-only at /run/sandbox/source |
| `--cpus int` | `0` | vCPUs for the guest (0: all host CPUs) |
| `--deny stringArray` |  | deny a destination, host[:ports] (repeatable); deny always wins |
| `-d`, `--detached` |  | print the sandbox name and exit instead of attaching |
| `--env stringArray` |  | extra environment variable KEY=VALUE (repeatable) |
| `--guest-credential stringArray` |  | what the guest holds instead: service=[ENV_NAME=]placeholder (repeatable) |
| `--inject stringArray` |  | attach a credential: service@host[,host]=bearer\|basic[:user]\|header[:format] (repeatable) |
| `-m`, `--memory string` |  | guest memory, binary units (1024m, 8g) (default: half the host's, max 32g) |
| `--name string` |  | sandbox name (default: &lt;agent&gt;-&lt;workspace directory&gt;) |
| `--net string` |  | network mode: none (no network at all) or nat (default nat) |
| `--no-secrets` |  | do not attach credentials from the store; only what --inject names |
| `--oauth stringArray` |  | name a stored OAuth credential; stored ones apply anyway, this pins one (repeatable) |
| `--policy string` |  | network policy preset: open, standard, locked (default standard) |
| `--profile string` |  | stored policy profile to apply ('boks policy profile ls') |
| `-p`, `--publish stringArray` |  | publish a sandbox port on the host, bound to loopback (repeatable): [[HOST_IP:]HOST_PORT:]SANDBOX_PORT[/PROTOCOL] |
| `-q`, `--quiet` |  | suppress the network summary (a new TLS-interception host is still announced) |
| `--rm` |  | destroy the sandbox when the command exits |
| `-t`, `--template string` |  | OCI image for the guest root filesystem (default: the agent's image) |

## boks secret

Manage host-side credentials the guest never receives

```
boks secret
```

Credentials live in an encrypted file on this machine and are never written into a
sandbox. The host proxy attaches them to requests for the hosts the credential names; the
guest holds a placeholder shaped like the real thing.

A credential stored under a service boks knows — anthropic, cursor, droid, github, google, groq, mistral, nebius, openai, openrouter, xai —
needs no further configuration: boks already has that vendor's hosts, header, environment
variable and key shape, and every sandbox you run attaches it. 'boks secret services'
prints the list. Anything else is stored under a name of your own and attached by a
'boks run --inject' rule.

The file is encrypted with a passphrase taken from BOKS_SECRETS_PASSPHRASE. Without an OS
keychain that is exactly as strong as the passphrase, and no stronger.

There is no recovery for a forgotten passphrase — that is what encryption means — so
'boks secret reset' deletes the file and everything in it, which is the only way out and
is spelled out wherever the store fails to decrypt.

### boks secret adopt

Adopt an OAuth credential an agent on this machine already has

```
boks secret adopt [flags] [NAME]
```

Adopts an existing OAuth credential — an access token, a refresh token and an expiry —
into the encrypted store, and mints the sentinels the guest will hold in its place.

Where it reads from, unless --from says otherwise:

```
  macOS      the Keychain item "Claude Code-credentials", via the 'security' CLI
  elsewhere  ~/.claude/.credentials.json
```

\--from - reads the credential document on standard input, which is the portable path and
works anywhere.

The value never leaves this machine. The sandbox receives sentinels shaped like real tokens;
the proxy substitutes the real access token on requests to the resource hosts, and refreshes
the pair on the host when it expires. Naming a host here means boks will terminate TLS for
it — see 'boks proxy --help'.

Once adopted it is used by every sandbox you run, and it takes precedence over an API key
covering the same hosts.

```
boks secret adopt claude-code
  boks secret adopt --from ~/.claude/.credentials.json claude-code
  cat creds.json | boks secret adopt --from - claude-code
```

| Flag | Default | Meaning |
|---|---|---|
| `--account string` |  | keychain account, when the item is not stored under your user |
| `--client-id string` |  | override the OAuth client id |
| `--env string` |  | guest environment variable holding the access-token sentinel |
| `--file string` |  | guest path for the rendered credential file |
| `--format string` | `claude-code` | credential format: claude-code |
| `--from string` |  | where to read it: 'keychain', '-' for stdin, or a file path |
| `--no-file` |  | do not render a credential file into the guest |
| `--resource-host stringArray` |  | override the hosts where the token is used (repeatable) |
| `--store string` |  | encrypted store file |
| `--token-url string` |  | override the token endpoint URL |

### boks secret import

Store credentials found in this shell's environment, with a prompt for each

```
boks secret import [flags] [SERVICE...]
```

Looks at the environment variables this shell already has — ANTHROPIC_API_KEY,
GITHUB_TOKEN, OPENAI_API_KEY and the rest — and offers to store the ones boks knows a
service for. Name services to consider only those.

Each is shown with the last four characters of its value, which is enough to tell two keys
for the same vendor apart and is the only fragment of any credential boks ever prints.

Nothing is stored without a "yes", unless --all is given. A service that already has a
credential is skipped unless --force is given, and one that already has an OAuth credential
is skipped whatever is given: a login takes precedence over a key, so storing one over it
would leave a credential that is never used.

```
boks secret import                 # walk everything found, with a prompt each
  boks secret import anthropic github
  boks secret import --all --dry-run
```

| Flag | Default | Meaning |
|---|---|---|
| `--all` |  | store everything found without prompting |
| `--dry-run` |  | say what would be stored and store nothing |
| `--force` |  | replace a credential that is already stored |
| `--store string` |  | encrypted store file |

### boks secret login

Arm a credential to be acquired by the agent's own login inside a sandbox

```
boks secret login [flags] [NAME]
```

Prepares boks to keep the credential an agent's own login produces, without the token ever
entering the sandbox.

Nothing is contacted and nothing is stored yet: this writes the shape of the credential —
the vendor's token endpoint, the hosts it is used on, and the sentinels the guest will hold
— and marks it as awaiting a login. Then you log in the way that agent normally does, inside
a sandbox:

```
  boks run claude -- auth login
```

The agent performs its own OAuth, with its own client id, because it is that program. Boks
already terminates TLS for the token endpoint, so it relays that one exchange, keeps the
tokens it returns, and rewrites the answer so what reaches the sandbox is sentinels. The
agent writes those sentinels into its own credential file; there is no moment at which a
real token exists inside the guest.

This is not boks running an OAuth flow. It has no client id for any vendor and will not
borrow another product's — see 'boks secret set --oauth'. It is boks keeping the result of a
login the agent performed for itself.

The login itself is a paste-a-code flow: the agent prints a URL, you authorise it in a
browser on this machine, and paste the code back into the sandbox's terminal. Nothing has to
listen on a port inside the sandbox, which is why this works with no port publishing.

```
boks secret login claude-code
  boks run claude -- auth login
```

| Flag | Default | Meaning |
|---|---|---|
| `--client-id string` |  | override the OAuth client id |
| `--env string` |  | guest environment variable holding the access-token sentinel |
| `--file string` |  | guest path for the rendered credential file |
| `--force` |  | replace a credential that is already stored under this name |
| `--format string` | `claude-code` | credential format: claude-code |
| `--no-file` |  | do not render a credential file into the guest |
| `--resource-host stringArray` |  | override the hosts where the token is used (repeatable) |
| `--store string` |  | encrypted store file |
| `--token-url string` |  | override the token endpoint URL |

### boks secret ls

List the stored credentials and where each one goes

```
boks secret ls [flags]
```

Lists the stored credentials, never their values: the name, whether it is an API key or
an OAuth login, and the hosts it is attached to.

This needs the passphrase, because the names are inside the encrypted envelope with the
values. That is deliberate: a plaintext index of which services you hold credentials for is
useful to somebody who cannot read the credentials themselves.

| Flag | Default | Meaning |
|---|---|---|
| `--store string` |  | encrypted store file |

### boks secret reset

Delete the credential store, for a passphrase that is lost

```
boks secret reset [flags]
```

Deletes the encrypted credential store and everything in it.

This exists because a forgotten passphrase is otherwise a dead end: every other subcommand
has to decrypt the store to do its work, 'rm' included, so there is no way to remove a
credential you can no longer read. This one does not decrypt anything and does not need the
passphrase — it removes the file.

Nothing is recoverable afterwards. Every credential has to be stored again with
'boks secret set', and sandboxes configured to inject one will refuse to start until it is.

| Flag | Default | Meaning |
|---|---|---|
| `--force` |  | actually delete it; without this the command only says what it would do |
| `--store string` |  | encrypted store file |

### boks secret rm

Remove a credential

```
boks secret rm [flags] NAME
```

| Flag | Default | Meaning |
|---|---|---|
| `--store string` |  | encrypted store file |

### boks secret services

List the services boks knows, and what it knows about each

```
boks secret services
```

Lists the services a credential can be stored under by name alone.

A service with no rule is one whose vendor documentation did not name both the host its
credential is sent to and the header that carries it. Boks does not guess at either: a
guessed rule sends the wrong header, or the right header to the wrong host, and either way
the placeholder in the guest reaches the real API instead of your credential. Store such a
credential under a name of your own and attach it with 'boks run --inject'.

### boks secret set

Store a credential for a service, read from stdin or --value

```
boks secret set [flags] SERVICE
```

Stores a credential under a name. Prefer stdin over --value: an argument is visible in the
process list and in your shell history.

If the name is a service boks knows, that is the whole configuration. Boks already has the
vendor's hosts, the header the key rides in, the environment variable the guest's own client
reads it from, and the shape a convincing placeholder has — so every sandbox you run
attaches it and no --inject is needed. 'boks secret services' lists them.

Any other name is stored just the same and attached by nothing until a 'boks run --inject'
rule says where it goes.

```
echo -n "$ANTHROPIC_API_KEY" | boks secret set anthropic
  echo -n "$GITHUB_TOKEN"     | boks secret set github
  echo -n "$KEY"              | boks secret set my-internal-api
```

| Flag | Default | Meaning |
|---|---|---|
| `--oauth` |  | acquire the credential by logging in (see 'boks secret set --help') |
| `--store string` |  | encrypted store file |
| `--value string` |  | the credential; omit to read it from stdin |

## boks start

Start a stopped sandbox

```
boks start [flags] SANDBOX...
```

Brings stopped sandboxes up, with the filesystem they had when they were stopped. Starting
a sandbox that is already running does nothing.

## boks stop

Stop a sandbox without deleting it

```
boks stop [flags] SANDBOX...
```

Shuts sandboxes down without destroying them. Anything written inside a sandbox is still
there when it starts again; 'boks rm' is what deletes it.

## boks version

Print the boks version

```
boks version [flags]
```

Prints the version of boks that is running.

With --check, also asks GitHub which release is newest and says whether this one is behind.
That request is made every time --check is passed: it is an explicit instruction, so it
neither reads the daily check's cached answer nor writes one, and the environment variables
that turn the daily check off do not apply to it.

'boks run' reports a new release by itself, once a day, from a cached answer. This command
is for asking now.

| Flag | Default | Meaning |
|---|---|---|
| `--check` |  | ask whether a newer release exists |
