# Verifying the VM boundary

A sandbox is only worth the name if the command really ran behind a hypervisor. "It printed
`running in sandbox`" proves nothing. This document defines what counts as evidence, and the
procedure for collecting it.

## Current status

**Verified on 2026-08-11.** `boks run` boots a genuine microVM through
`containerd-shim-nerdbox-v1` and libkrun, and the guest's kernel identity, uptime and
hardware topology are its own.

The host was macOS, which makes the result unusually clear-cut: the guest runs **Linux**
while the host runs **Darwin**, so a shared-kernel container is not a possible explanation
for any of it.

### Environment

| | |
|---|---|
| Host | Apple M5 Pro (T6050), 18 cores, 48 GiB |
| Host OS | macOS 26.5.2 (build 25F84), Darwin 25.5.0, `xnu-12377.121.10~1`, arm64 |
| containerd | 2.3.3 (Homebrew), running rootless as uid 502 |
| nerdbox | `cd2c23f`, shim codesigned with `com.apple.security.hypervisor` |
| libkrun / libkrunfw | 1.19.4 / 5.5.0 |
| erofs-utils / e2fsprogs | 1.9.3 / 1.47.4 |

### Host versus guest

Collected from the same machine, minutes apart. Guest values are from
`boks run shell . -- …` with `-cpus 2 -memory 2048`. (Those were the defaults at the time;
the defaults are now all host CPUs and half the host's memory, so a repeat run reports the
values you pass rather than these.)

| Fact | Host | Guest |
|---|---|---|
| `uname` | `Darwin 25.5.0 … arm64` | `Linux 6.12.44 #1 SMP aarch64` |
| kernel build | `xnu-12377.121.10~1/RELEASE_ARM64_T6050` | `gcc (Debian 12.2.0) … #1 SMP Tue Aug 11 17:34:15 UTC 2026` |
| `boot_id` | no procfs; macOS has none | `39e1d653-fa1d-420c-945f-97467b87c3b8`, **new on every run** |
| uptime | 28 days, 7:21 | `0.03` seconds |
| CPUs | 18 | 2 (tracks `-cpus`) |
| memory | 48 GiB | `MemTotal: 2044888 kB` (tracks `-memory`) |
| virtio devices | — | `virtio0`…`virtio7` (fs, block ×3, console, rng, balloon, vsock) |
| PID 1 | `launchd` | the sandboxed process itself |

Resource requests reach the VMM rather than being advisory:

| Flags | guest `nproc` | guest `MemTotal` |
|---|---|---|
| `-cpus 1 -memory 512` | 1 | 500532 kB |
| `-cpus 4 -memory 4096` | 4 | 4041812 kB |
| `-cpus 8 -memory 1024` | 8 | 1013764 kB |

A full boot, command and teardown takes **~0.23 s**.

### Behaviour verified behind that boundary

| Behaviour | Result |
|---|---|
| containerd connect, image pull, unpack (erofs) | pass |
| container + task create, start, wait, delete | pass |
| stdout/stderr streaming | pass |
| exit code propagation (0, 7, 42) | pass |
| workspace visible at its exact host path | pass |
| intermediate mount-point directories auto-created in the guest | pass |
| writes reaching the host promptly | pass |
| read-only workspace rejecting writes | pass |
| parent directories not exposed | pass |
| symlink out of the workspace does **not** escape to host files | pass |
| no host Docker or containerd socket in the guest | pass |
| no host processes or host filesystem visible | pass |
| cleanup: no leaked containers, tasks, shims or VM processes | pass |
| cleanup after SIGINT | pass |
| `BOKS_INTEGRATION=1` suite against `io.containerd.nerdbox.v1` | 7/7 pass |

### Not yet verified behind that boundary

The persistent sandbox lifecycle was built and tested against a real containerd on a host
with **no hypervisor**, using the runc dev runtime
(`-runtime io.containerd.runc.v2 -snapshotter native -i-know-this-is-not-isolated`). That
proved the containerd orchestration and nothing about the VM boundary.

`stop`, `start`, `exec`, `rm` and snapshot persistence have since been confirmed behind a
real hypervisor — see *Lifecycle behind a real VM* below. What still needs confirming:

- that an `exec`'d process runs inside the *same* VM as the sandbox's own process (compare
  `boot_id`, and check the exec'd process sees the sandbox's PID namespace);
- that `cp` works when the guest is reached through vsock rather than a local FIFO.

Run the suite unmodified — it defaults to the isolating runtime:

```bash
BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v
```

### What was *not* contained, in that configuration

Network. The guest had no virtio-net device — only `lo` — yet it reached the internet, and it
reached **the host's own loopback services**. nerdbox's default is libkrun's TSI, which
rewrites guest `AF_INET` sockets and performs the connection on the host, so guest
`127.0.0.1` is the *host's* `127.0.0.1`. Observed: DNS resolves, outbound HTTP and HTTPS
succeed, raw TCP connects, ICMP fails (`Network unreachable`), and a host service on
`127.0.0.1:11434` answered the guest. See [security-model.md](security-model.md).

`boks run` now wires every sandbox it creates to a host-side stack instead, which is what the
next section is for: that wiring has never been watched from inside a real guest.

## The network enforcement, answered on a host with a hypervisor

**Run 2026-08-12**, macOS 26.5.2 / Apple M5 Pro, containerd 2.3.3, nerdbox `cd2c23f`,
libkrun 1.19.4, against `io.containerd.nerdbox.v1` and the published
`ghcr.io/dagsommer/boks/base:0.1.0`. Eleven of the twelve checks pass. **Check 6 fails**,
and it is the one that decides whether any of this is a boundary.

> [!CAUTION]
> **Check 6 failed: policy in `-net nat` is advisory.** A guest that unsets the proxy
> variables reaches denied destinations. Under `-policy locked -allow example.com`:
>
> ```
> === denied host, unsetting ALL FOUR proxy vars ===
>   http=200
> === raw TCP to denied 1.1.1.1:443, real TLS handshake via python ===
>   TLS to 1.1.1.1 SUCCEEDED, peer cert issuer: {'countryName': 'US', ...,
>     'commonName': 'SSL.com SSL Intermediate CA ECC R2'}
> ```
>
> The certificate is the origin's own, so this is a real end-to-end connection, not the
> proxy. Neither attempt appears in `boks policy log`: they never reached the policy engine.

**A trap worth recording, because it manufactures a false pass.** The guest's proxy
variables are **lowercase** (`http_proxy`, `https_proxy`), while the CLI banner says
`HTTP_PROXY and HTTPS_PROXY`. A probe that unsets only the uppercase pair still goes through
the proxy and is correctly refused — which reads exactly like enforcement working. Several
runs during this session looked like passes for that reason. **Unset all four**, and prefer
a raw socket over `curl` for the decisive check.

The base image also carries no `wget`, `nslookup` or `nc`, and its `/bin/sh` is dash, so
`/dev/tcp` is unavailable there. A `wget`-based probe fails with `command not found`, which
also reads as a pass. Use `curl`, `python3`, and `bash -c` when `/dev/tcp` is wanted.

| # | Question | Result | Observed |
|---|---|---|---|
| 1 | Does the guest get the NIC Boks asked for? | **pass** | `eth0` and `lo` in `/sys/class/net`; a ninth virtio device, id `1` (`VIRTIO_ID_NET`), appears only when the annotations are set |
| 2 | Is the host's loopback gone? | **pass** | host `python3 -m http.server 9999 --bind 127.0.0.1` unreachable: `curl` rc=7, Python `ConnectionRefusedError`. TSI answered this probe; the stack refuses it |
| 3 | Does the guest reach the proxy? | **pass** | `http_proxy=http://192.168.127.1:3128` is reachable and serves every proxied request below |
| 4 | Is an allowed host allowed? | **pass** | `https://example.com` → `200`, logged `allowed by rule "example.com"` |
| 5 | Is a denied host denied? | **pass** | `https://www.google.com` through the proxy → curl rc=56; plain HTTP → `403`; logged `denied by default (policy "locked+local" allows only listed destinations)` |
| 6 | **Is a guest that ignores the proxy still contained?** | **FAIL** | with all four proxy variables unset, the denied host returns `200`, and a raw Python TLS handshake to `1.1.1.1:443` completes against the origin's own certificate. Nothing reaches the policy engine |
| 7 | Is DNS mediated? | **pass** | `nameserver 192.168.127.1` in the guest, not a copy of the host's file. Note it resolves denied names too — mediated, not filtered |
| 8 | Does `-net none` mean none? | **pass** | `lo` only; `curl` → `000`, raw socket → `OSError`. The one posture that contains a hostile guest today |
| 9 | Is the CA usable inside the guest? | **pass** | see *Credential injection* below |
| 10 | Does the network survive the command that started it? | **pass** | `boks run -d` then `boks exec` from a fresh shell works; `boks net ls` shows the same PID |
| 11 | **Does a running VM re-attach to a restarted stack?** | **no** | `kill -9` on the supervisor is detected (`no sandbox network stacks are running`), and a later command starts a fresh one on the same socket — but the running guest never re-attaches: `curl` rc=7, raw socket `OSError`. The VM keeps running; only its network is gone. **A crashed supervisor costs that sandbox its network until the sandbox is restarted** |
| 12 | Does teardown reach the VM? | **pass** | after `rm -f`: no container, no task, no `boks net serve` process, empty state directory |

Check 6 is the one that decides whether any of this is a boundary, and it failed: in
`-net nat` the policy is a cooperation feature, not containment. Checks 11 and 12 answered
the supervisor question — teardown is clean, and there is no repair path, so a crashed
supervisor is a restart.

### Credential injection, verified inside a real guest

*(2026-08-12.)* The property that matters is that the secret never enters the VM, and it
holds. With the secret `REAL-SECRET-VALUE-12345` stored host-side and

```
-inject 'demo@postman-echo.com=x-api-key' -guest-credential 'demo=DEMO_KEY=placeholder-not-the-secret'
```

the guest printed `placeholder-not-the-secret`, a grep of the guest's whole environment for
the real value returned `0`, and the origin received:

```
{"headers":{"host":"postman-echo.com", ... ,"x-api-key":"REAL-SECRET-VALUE-12345", ...}}
```

Interception is scoped to hosts that have a credential rule, confirmed by comparing issuers
through the proxy in one guest:

| Host | Credential rule | Certificate issuer seen by the guest |
|---|---|---|
| `postman-echo.com` | yes | `O=Boks, CN=Boks local CA (CFP2C7WKW7)` |
| `example.com` | no | `C=US, O=SSL Corporation, CN=Cloudflare TLS Issuing ECC CA 3` |

`boks policy log` recorded `forward` for the intercepted host and `forward-bypass` for the
other, as designed.

Note the spelling: the attachment is `header-name[:format]`, so `x-api-key` is right and
`header:x-api-key:%s` sets a header literally named `header`. The flag help reads
`bearer|basic[:user]|header[:format]`, where `header` is a placeholder for the name — easy
to misread as a keyword.

### Lifecycle behind a real VM

*(2026-08-12.)* Verified with the isolating runtime, replacing the runc-based results below:

- **`stop` then `start` preserves the writable snapshot**: a file written to `~` before the
  stop read back as `persisted` after the start.
- **`start` boots a new VM, it does not resume one.** `boot_id` changed across the stop
  (`45be14e1-…` → `82408573-…`) and guest uptime was `2s`. In-guest state survives because
  the snapshot does, not because the VM does.
- **`rm -f` leaves nothing**: no container, no task, no shim, no supervisor, empty state
  directory.
- **Ctrl-C cleans up but reports badly.** SIGINT to a running `boks run` leaves nothing
  behind, but exits **1** and prints
  `boks: sandbox process failed: rpc error: code = Canceled desc = context canceled`
  rather than exiting 130 silently.

## TLS interception, verified on the host

*(verified 2026-08-11, Linux host, no hypervisor involved — this is a host-side path and
needs no guest.)* Two real HTTPS origins, certificates issued by a throwaway "Demo Web CA"
standing in for the public trust store; a real `curl` through `boks proxy`; one host with a
credential injection rule and one without.

| Claim | Evidence |
|---|---|
| the origin receives the real secret | `x_api_key_received: "sk-ant-REAL-SECRET-VALUE-…"` |
| the client only ever sent a placeholder | request carried `x-api-key: sk-ant-api03-placeholder…` |
| a host with no rule gets nothing | the same placeholder arrived unchanged at `plain.localtest.me`, and no `Authorization` |
| the intercepted host presents a Boks certificate | `subject=O=Boks intercepted, CN=api.localtest.me` / `issuer=O=Boks, CN=Boks local CA (…)` |
| the unconfigured host keeps its own chain | `subject=CN=plain.localtest.me` / `issuer=O=Demo Web CA, CN=Demo Web CA` |
| the log separates the two | `PROXY` column: `forward` for `api.localtest.me:9443`, `forward-bypass` for `plain.localtest.me:9444` |
| no secret reaches any log | `grep` for the credential in the decision log and the proxy's stderr: 0 hits, while the stderr does say `injected credential anthropic for api.localtest.me:9443` |
| the origin's certificate is verified by the proxy | the proxy ran with the demo CA in its trust store; a unit test drives the failure path and asserts the request never reaches the origin |

The certificate comparison is the load-bearing part: both hosts were reached through the
same proxy, by the same client, trusting both authorities, so the only thing distinguishing
them is whether Boks substituted a certificate — and it did so for exactly the host with a
credential rule.

**Not verified:** none of this has run against a guest. `boks run` now does start the proxy,
set `HTTP_PROXY` and share the CA into the sandbox — see the checks above — but no VM has
exercised any of it. What is demonstrated here is the host-side mechanism, driven by a real
client over real TLS.

## What counts as evidence

Weak evidence, do not rely on it:

- a message printed by the guest saying it is sandboxed;
- a hostname that differs from the host's;
- absence of a file you expected to see.

Strong evidence — each of these is hard to fake from inside a container sharing the host
kernel:

1. **Different kernel identity.** `uname -r` and, more tellingly,
   `cat /proc/sys/kernel/random/boot_id` differ from the host's. A container shares the
   host's boot_id; a VM has its own.
2. **Different kernel build.** `cat /proc/version` shows the microVM kernel, not the host's.
3. **Independent uptime.** `cat /proc/uptime` in the guest is bounded by the sandbox's age,
   not the host's. A container reports host uptime.
4. **PID 1 is the guest init.** In the guest, `ls /proc/1/comm` names the VM's init, and the
   host's process table contains no such process tree.
5. **Distinct device topology.** The guest's `/proc/cpuinfo`, `/sys/class/block` and
   `/proc/meminfo` reflect the vCPU and memory the sandbox was given
   (`-cpus`, `-memory`), not the host's hardware.
6. **virtio devices present.** `ls /sys/bus/virtio/devices` is non-empty in the guest and
   lists the virtiofs and, if configured, virtio-net devices.
7. **Kernel modification is contained.** Something that would change host kernel state —
   writing a `/proc/sys` value — is visible only inside the guest.
8. **No host container-runtime socket.** `/var/run/docker.sock` and containerd's socket are
   absent.

Evidence 1, 3 and 5 together are the practical minimum: a kernel with its own boot identity,
its own uptime, and hardware that matches what Boks asked the VMM for.

## Procedure

On a host with hardware virtualisation (bare metal Linux with KVM, or Apple silicon macOS):

1. **Confirm prerequisites.**

   ```
   boks doctor
   ```

   Every check must be `ok`. In particular `virtualization` and `vm runtime`.

2. **Record the host's identity.**

   ```
   uname -r; cat /proc/sys/kernel/random/boot_id; cat /proc/uptime; nproc
   ```

3. **Collect the same from a sandbox.**

   ```
   boks run shell . -cpus 2 -m 2048m -- sh -c 'uname -r; cat /proc/sys/kernel/random/boot_id; cat /proc/uptime; nproc'
   ```

4. **Compare.** The boot_id must differ, the guest uptime must be far smaller than the
   host's, and `nproc` must equal the `-cpus` value rather than the host's core count.

5. **Check the device topology.**

   ```
   boks run shell . -- sh -c 'ls /sys/bus/virtio/devices; cat /proc/version'
   ```

6. **Confirm the workspace still behaves.** Exact path, contents, and write-back:

   ```
   boks run shell . -- sh -c 'pwd && ls && touch boks-probe'
   ```

7. **Confirm containment of the parent.** The parent directory must contain only the
   workspace:

   ```
   boks run shell /some/dir/project -- ls /some/dir
   ```

8. **Run the integration suite against the real runtime.**

   ```
   BOKS_INTEGRATION=1 go test ./internal/sandbox/ -run Integration -v
   ```

   With no `BOKS_TEST_RUNTIME` override these run against `io.containerd.nerdbox.v1`, so a
   pass means the assertions held behind a VM boundary. The suite logs a warning if it is
   pointed at a non-isolating runtime; a run showing that warning does not count.

9. **Check for leaks afterwards.**

   ```
   ctr -n boks containers ls
   ctr -n boks tasks ls
   ps aux | grep containerd-shim
   grep io.containerd.runtime /proc/mounts
   ```

   All must be empty.

## macOS setup notes

Four things cost time on the first macOS run. None are Boks bugs, but all of them produce
errors that do not name their own cause.

1. **The shim must be codesigned.** libkrun needs the `com.apple.security.hypervisor`
   entitlement. `task build` (buildx bake) produces a shim *without* it; only
   `task build:shim` runs `codesign`. An unsigned shim fails at boot with:

   ```
   failed to create shim task: failure running vm: krun_start_enter failed: -22
   ```

   Check with `codesign -d --entitlements - _output/containerd-shim-nerdbox-v1`.

   `boks doctor` now performs this check itself, as `runtime entitlement`. **That check has
   not yet been run against a real macOS host** — it was added on Linux, where it does not
   apply, so its parsing of `codesign` output is unconfirmed. Verifying it is the first thing
   to do on the next macOS run: it should report `ok` on a signed shim, and `fail` with the
   remedy above on an unsigned one. It reports a warning, never a hard failure, if `codesign`
   itself cannot be run.

2. **`/var/run/containerd` must exist and be writable by you.** containerd derives the
   shim's socket path from a compile-time constant
   (`pkg/shim/util_unix.go`: `const socketRoot = defaults.DefaultStateDir`), so no config
   setting moves it — this is [containerd#12444](https://github.com/containerd/containerd/issues/12444).
   Symptom: `creating sandbox process: mkdir /var/run/containerd: permission denied`. Fix:

   ```
   sudo mkdir -p /var/run/containerd && sudo chown "$(id -u):$(id -g)" /var/run/containerd
   ```

   This is the only step needing root.

3. **Rootless containerd otherwise works**, contrary to nerdbox's README note. `root` and
   `state` can live under `$HOME` provided you also set `[ttrpc] address` alongside
   `[grpc] address` and give both `uid`/`gid`, otherwise startup dies on
   `chown …containerd.sock.ttrpc: operation not permitted`.

4. **containerd's PATH must contain the nerdbox `_output` directory**, which supplies the
   shim *and* the guest kernel and rootfs — the shim locates
   `nerdbox-kernel-<arch>` and `nerdbox-rootfs.erofs` by scanning `PATH`
   (or `LIBKRUN_PATH`), not by looking next to itself.

## Recording results

When this procedure is run again on different hardware, update the "Current status" section
above with the observed values — the boot_ids, the kernel versions, the vCPU count.

### Confirm on the next macOS run

Two changes were made after the verification above, on a Linux machine with no hypervisor.
Both are unconfirmed against a booted VM:

- the `runtime entitlement` check in `boks doctor` (see the codesign note above);
- Ctrl-C now exits `128+signal` — 130 for SIGINT, 143 for SIGTERM — and prints nothing,
  instead of exiting 1 with a raw `context canceled` gRPC error. Verified against
  containerd's `runc` runtime, including that no container, task or shim survives; not yet
  observed tearing down a real VM.
