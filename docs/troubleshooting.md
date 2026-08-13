# Troubleshooting

Start with `boks doctor`. It checks every prerequisite and, for each gap, prints what to do
about it rather than what is wrong; it exits non-zero when sandboxes cannot start. Most of
this page is the same advice, findable when you are reading rather than running.

```bash
boks doctor
```

Checks run in this order, and the first failure is usually the only real one: `platform`,
`virtualization`, `containerd`, `snapshotter`, `snapshotter tools`, `vm runtime`,
`hypervisor library`, `guest image`, and on macOS `runtime entitlement`.

`ok`, `warn`, `fail`, `skip` mean what they say: **warn** is "Boks can run but something is
degraded or unverified", **fail** is "sandboxes cannot start until this is fixed".

---

## The host

### `virtualization` warns on macOS and always will

Expected. Hypervisor.framework cannot be probed without booting a VM, so on Apple silicon the
check reports architecture support and says as much. On Linux the same check opens `/dev/kvm`
and issues an ioctl, so there it is `ok` or `fail` and means something.

### A sandbox dies with `krun_start_enter failed: -22`

The nerdbox shim is not codesigned with the `com.apple.security.hypervisor` entitlement, so
libkrun cannot use Hypervisor.framework. The error names nothing, which is why `doctor` has a
check for it.

```bash
codesign -d --entitlements - _output/containerd-shim-nerdbox-v1
```

nerdbox's `task build:shim` signs it. A plain image build — `task build`, buildx bake — does
not. This is the single most common macOS setup failure.

### `mkdir /var/run/containerd: permission denied`

```bash
sudo mkdir -p /var/run/containerd && sudo chown "$(id -u):$(id -g)" /var/run/containerd
```

The only step that needs root. Full macOS setup notes, including the rootless containerd
configuration where startup dies on `chown …containerd.sock.ttrpc: operation not permitted`,
are in [Verification](verification.md#macos-setup-notes).

### `containerd` is unreachable

- **No socket at the address** — install and start containerd, or point Boks elsewhere with
  `--containerd-address` / `BOKS_CONTAINERD_ADDRESS`. A stale socket file is a common failure
  mode, which is why `doctor` makes a real API call rather than stat-ing the path.
- **Permission denied** — containerd's socket is usually root-owned. Either run Boks with
  sufficient privileges or use a rootless containerd, which is the recommended setup.

### `vm runtime`: `containerd-shim-nerdbox-v1 not found on PATH`

Build it from [nerdbox](https://github.com/containerd/nerdbox) and install the binary where
containerd can find it. **containerd's `PATH` is the daemon's, not your shell's** — this
catches almost everyone once.

That directory must also contain the guest kernel and rootfs: the shim locates
`nerdbox-kernel-<arch>` and `nerdbox-rootfs.erofs` by scanning `PATH` (or `LIBKRUN_PATH`),
not by looking next to itself. That is the `guest image` check below.

### `guest image`: `nerdbox-kernel-<arch> ... not found`

These are the two files a microVM boots, and nothing packages them: nerdbox's releases
carry no assets, and building them is a Linux kernel build driven by `docker buildx bake`.

```bash
scripts/build-nerdbox-guest.sh          # needs Docker with buildx; on Windows, under WSL2
```

Copy both into a directory on containerd's `PATH` or on `LIBKRUN_PATH` —
`$(brew --prefix)/lib` on Apple silicon is already one. `docker` and the machine that runs
the sandbox need not be the same host: these are guest artefacts, so build once per
architecture and copy. [Installation](install.md) has the detail.

The shim accepts `nerdbox-rootfs-<arch>.erofs` as well as the unsuffixed
`nerdbox-rootfs.erofs`; there is no unsuffixed kernel name. `doctor` scans the same places
in the same order, with one caveat it cannot do anything about: it scans *your* `PATH`, and
the one that decides is the containerd daemon's.

### A pull fails partway through with an exec error

`mkfs.erofs` is missing. containerd reports the erofs snapshotter as initialised even when
the tool is absent, so the failure only appears when an image is unpacked.

```bash
apt install erofs-utils      # or: brew install erofs-utils
```

Version matters and `doctor` does not check it: containerd's EROFS snapshotter needs **1.8 or
later**, and Ubuntu 24.04 LTS ships 1.7.1.

### `hypervisor library` warns but everything works

`doctor` stats the usual locations for `libkrun` and does not parse the dynamic loader's
configuration. If libkrun is installed somewhere else on the loader's search path, the
warning is harmless. The check never fails for this reason.

---

## Linux

### `/dev/kvm` is missing

```bash
lsmod | grep kvm
```

On bare metal: enable virtualisation (VT-x / AMD-V / EL2) in firmware and load the kvm
modules. Inside a VM: enable nested virtualisation on the outer hypervisor — some, including
Apple Virtualization Framework guests, do not expose it at all.

### `/dev/kvm` is not accessible

```bash
sudo usermod -aG kvm $USER
```

Then start a new login session. Do **not** `chmod 666 /dev/kvm`, which many guides suggest:
it lets every local account create virtual machines on the host.

### Linux generally

The KVM path is built and designed for, and nobody on this project has exercised it end to
end. If something fails there that this page does not cover, that is the expected state
rather than a surprise — see [Get started](get-started.md#which-platforms-work).

---

## WSL2

Boks inside WSL2 is the only route to Boks on a Windows machine today; a native Windows build
does not exist. **Nobody has run it.** Every ingredient below is traced to WSL's source, its
issue tracker or its kernel config, and none of it has been executed — the full analysis is in
[Windows](windows.md). `doctor` detects WSL through `/bin/wslinfo` and gives WSL-specific
remedies for the KVM failures.

**WSL 2.5.1 is a hard floor**, because it introduced the modules image that makes any
loadable module loadable at all, and clean cgroups v2.

### First, do not start with `nestedVirtualization=true`

Nested virtualisation is **already on by default on Windows 11 x64**, so the advice every
generic guide leads with is usually not the fix. Discriminate first:

```bash
grep -Ec '^flags.*\b(vmx|svm)\b' /proc/cpuinfo
```

- **0** — nested virtualisation really is off at the Windows level, and nothing installed
  inside the distribution can change that. It is genuinely off only on Windows 10, on ARM64,
  on a CPU predating Haswell or Zen, under `safeMode=true`, or under the
  `AllowNestedVirtualization` enterprise policy.
- **1 or more** — nested virtualisation is working and the module is simply not loaded. Go to
  the next section.

Two things that will mislead you: a **malformed `.wslconfig` is ignored silently**, so a
typo'd stanza is indistinguishable from no stanza; and the Windows-side error
`Nested virtualization is not supported on this machine.` goes to `wsl.exe`'s stderr **on the
Windows side**, where nothing inside the distribution can ever see it. Do not grep for it.

`.wslconfig` is global only, `/etc/wsl.conf` has no nested-virtualisation key, and the section
name and key are case-sensitive: `[wsl2]`, `nestedVirtualization`.

### The modules are not loaded

WSL loads exactly three modules at boot — `tun`, `ip_tables`, `br_netfilter` — and both KVM
and EROFS are built as modules.

```bash
sudo modprobe kvm_amd     # or kvm_intel on an Intel CPU
sudo modprobe erofs
```

To persist, in `%UserProfile%\.wslconfig` on the Windows side, then `wsl --shutdown`:

```ini
[wsl2]
loadKernelModules=kvm_amd,erofs
```

`loadKernelModules` is present in WSL's source but undocumented, so treat it as best-effort
and keep `modprobe` as the fallback. `nested=1` on the KVM module is **not** needed — that
governs a third level of nesting and is widely cargo-culted.

### `/dev/kvm` is `root:root 0600`

The node appears through devtmpfs, but **WSL runs no udev**, so the rule that would widen it
on an ordinary distribution never runs.

```bash
getent group kvm || sudo groupadd -r kvm     # it arrives with qemu/libvirt, not the base system
sudo usermod -aG kvm $USER
```

Then fix the node on every boot, in `/etc/wsl.conf` inside the distribution, and
`wsl --shutdown` on the Windows side:

```ini
[boot]
command = /bin/bash -c 'chown root:kvm /dev/kvm && chmod 660 /dev/kvm'
```

With `[boot] systemd=true` udev runs and the stock rules should give `root:kvm 0660` without
the command above — but systemd is off by default in many images.

### Two costs to plan for

- **Keep workspaces in the WSL2 filesystem, not `/mnt/c`.** WSL2 reaches the Windows
  filesystem over 9p; a workspace there makes `git status` on a large repository painful, and
  it is then crossing 9p *and* virtiofs to reach the guest. This is the single most important
  piece of advice for anyone running Boks this way. `/mnt/c` is also case-insensitive by
  default.
- **Do not build a custom WSL kernel.** One without a matching modules image silently loses
  every loadable module, which includes both KVM and EROFS.

Note that this is two nested VM boundaries: Boks' microVM runs inside the WSL2 utility VM. The
sandbox boundary is still a hypervisor boundary, but the threat model now includes WSL2's own.

---

## Running a sandbox

### Git refuses the workspace: "dubious ownership"

```
fatal: detected dubious ownership in repository at '/private/tmp/project'
```

You should not see this — it is handled. A workspace is a live virtiofs mount of a host
directory, so inside the guest it is owned by the host user's uid while the process runs as
root, and Git refuses that combination. It is the first thing a coding agent hits, on every
command it runs, and `git diff` fails worse than the rest by reporting "Not a git repository",
which an agent may believe and act on.

Boks sets `safe.directory` for each workspace through Git's command scope
(`GIT_CONFIG_COUNT` and friends), which is the only protected configuration available from
outside the repository.

If you set `GIT_CONFIG_COUNT` yourself with `--env`, your value wins and you take
responsibility for the whole set — including these entries.

### The agent is denied `api.anthropic.com`

```
no applicable policies for op(action=net:connect:tcp, resource=net:domain:api.anthropic.com:443)
```

Three things produce this, in decreasing order of likelihood:

1. **You stored a credential and no allow rule.** A credential rule says where a value may
   go, not what is reachable. `boks secret set` prints the line you need:
   ```bash
   boks policy allow api.anthropic.com:443
   ```
2. **`--policy locked`.** That drops the agent's own allow layer entirely, because "deny
   everything" has to keep meaning that. Add the destination explicitly.
3. **A deny somewhere.** Deny beats allow in every scope. `boks policy ls` shows which rule
   and which scope, and the CLI says so when an allow you just wrote cannot take effect.

For a subscription login through a sandbox you also need
`boks policy allow platform.claude.com:443`, which is the token endpoint.

Whatever the case, `boks policy log --sandbox <name>` shows the decision and its reason.

### `boks policy allow` did not take effect

```
note: <destination> is still denied — <rule>, written in <scope>. Deny always wins;
      remove that rule if you meant to permit this: boks policy rm <id>
```

That is the whole rule: a deny in any scope beats an allow in any scope.

### A sandbox is running but nothing inside it can reach anything

```
WARNING: sandbox "x" is running, but the process serving its network is gone.
```

The per-sandbox network stack died. A running guest does **not** re-attach to a new link
socket — that was measured on 2026-08-12 — so the sandbox has no network until it is
restarted:

```bash
boks stop <name> && boks start <name>
```

Boks does not do that for you, because it kills whatever is running inside. This is the top
item on the [roadmap](roadmap.md).

### A sandbox ignores `--net none`, or has no policy at all

Network mode is fixed when a sandbox is created, because it is expressed in annotations the
runtime reads at boot. Passing `--net` to an existing sandbox is reported, not obeyed.
Remove it and run again.

A sandbox created before Boks had network annotations runs on the runtime's default transport
(libkrun's TSI), where the guest's `127.0.0.1` is the host's and no policy can be applied to
it at all. Boks warns about this by name. Recreate it.

### Nothing answers on a published port

The service inside the sandbox is bound to the *guest's* own `127.0.0.1`. It has to listen on
the VM's external interface — `0.0.0.0` or `::`. `boks ports` prints exactly this under the
table when a forward has no listener.

`boks ports <name> --publish` also only works on a sandbox that is running: the ports belong
to the network stack, which is started with the VM.

### `--publish` was ignored

It is ignored when re-attaching to an existing sandbox, which keeps the ports it has. Change
them on the running sandbox:

```bash
boks ports <name> --publish 3000
```

### `boks run -it` does not work

There are no `-i` or `-t` terminal flags on `run` or `create`. A run attaches your terminal
exactly when you have one and gets no pty when its output is a pipe, so there is nothing to
switch on. On `run`, `-t` is `--template` — which is why `-ti` does not fail loudly, it
quietly sets `--template` to `"i"`.

```bash
boks run claude .            # a terminal, if you have one
boks exec -it <name> sh      # a terminal in a sandbox that already exists
```

### Ctrl-C exits 1 with an RPC error

Known, and cosmetic: cleanup is complete — no containers, tasks, shim processes or mounts are
left behind — but the exit status is 1 with an RPC error rather than 130 in silence.

---

## Credentials

### I have forgotten the secret store passphrase

There is no recovery. That is what encryption means: the store is one AES-GCM envelope, and a
key that does not open it is indistinguishable from a file that has been damaged. Boks does
not offer to try another passphrase because there is nothing to try one against.

```bash
boks secret reset --force        # deletes the store and every credential in it
```

`reset` is the one subcommand that does not decrypt anything, which is why it works. Every
credential then has to be stored again with `boks secret set NAME`, and until they are, a
sandbox whose policy injects a credential **refuses to start** rather than running without
it.

Without `--force` the command only says what it would do.

### An API key I stored is not being used

```
Note: "x" is an OAuth credential covering the same hosts, and a login takes precedence
over a key — so this key is stored but will not be attached.
```

An OAuth credential is the login you performed, it refreshes itself, and it wins for the
destinations it covers. Remove the login with `boks secret rm <name>` if you meant to use the
key, or store the key under a different name and point an `--inject` rule at it.

### `boks secret set cursor` / `boks secret set droid` refuse

Neither vendor documents the host their CLI sends its key to, and Boks will not guess: a
guessed rule ships your placeholder to the real API instead of your credential. The command
gives you the `--inject` form instead. See [Agents](agents.md#credentials-by-service-name).

### `boks secret set NAME --oauth` is refused

Every flow that could acquire a token starts by identifying the program to the vendor with a
client id issued to a registered application, and Boks holds none. Two things work instead:

```bash
boks secret adopt claude-code    # take over a login you have already performed
boks secret login claude-code    # arm one to be captured from a login inside a sandbox
```

`adopt` covers a machine you have used the agent on, and nothing at all on a fresh one.

### An HTTPS request from the guest fails certificate validation

Only hosts named by a credential rule are intercepted; everything else is tunnelled with the
origin's own chain untouched. For the intercepted ones the guest has to trust the Boks CA:

```bash
boks ca show
boks ca export -o boks-ca.pem      # install it in the guest, never in your host trust store
boks ca env                        # for runtimes with their own trust store (Node, Python)
```

In a sandbox the CA's reach is that sandbox. In your login keychain it is every TLS
connection you make.

---

## Still stuck

- `boks <command> --help`, and the [CLI reference](cli.md), which is generated from the same
  command tree.
- `boks policy log` for anything network-shaped.
- [Verification](verification.md) for what has actually been observed, including the failures
  — several entries there are things that were measured broken.
- [FAQ](faq.md) for the questions that are not failures.
