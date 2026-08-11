# Verifying the VM boundary

A sandbox is only worth the name if the command really ran behind a hypervisor. "It printed
`running in sandbox`" proves nothing. This document defines what counts as evidence, and the
procedure for collecting it.

## Current status

**Not yet verified by this project.** The machine used to develop Boks so far is itself a
guest of Apple's Virtualization Framework with no nested virtualisation: `/dev/kvm` is
absent, `/lib/modules` is empty and no KVM module can be loaded. No hypervisor is available
to it, so no microVM can be booted, so the boundary cannot be demonstrated.

What *has* been verified there, using containerd's `runc` runtime as a stand-in for the
orchestration layer only (`-i-know-this-is-not-isolated`):

| Behaviour | Result |
|---|---|
| containerd connect, image pull, unpack | pass |
| container + task create, start, wait, delete | pass |
| stdout/stderr streaming | pass |
| exit code propagation (0 and 42) | pass |
| workspace visible at its exact host path | pass |
| writes reaching the host | pass |
| read-only workspace rejecting writes | pass |
| parent directories not exposed | pass |
| no host Docker socket in the guest | pass |
| cleanup: no leaked containers, tasks, shims or mounts | pass |
| cleanup after SIGINT | pass |
| clear errors for missing runtime, missing workspace, missing command | pass |

Everything in that table exercises Boks' own logic. **None of it demonstrates isolation** —
`runc` shares the host kernel, which is exactly why `boks run` refuses that runtime unless
you pass an explicit opt-out flag.

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
   boks run . -- sh -c 'uname -r; cat /proc/sys/kernel/random/boot_id; cat /proc/uptime; nproc'
   ```

4. **Compare.** The boot_id must differ, the guest uptime must be far smaller than the
   host's, and `nproc` must equal the `-cpus` value rather than the host's core count.

5. **Check the device topology.**

   ```
   boks run . -- sh -c 'ls /sys/bus/virtio/devices; cat /proc/version'
   ```

6. **Confirm the workspace still behaves.** Exact path, contents, and write-back:

   ```
   boks run . -- sh -c 'pwd && ls && touch boks-probe'
   ```

7. **Confirm containment of the parent.** The parent directory must contain only the
   workspace:

   ```
   boks run /some/dir/project -- ls /some/dir
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

## Recording results

When this procedure is run successfully, replace the "Current status" section above with the
observed values — the two boot_ids, the two kernel versions, the vCPU count — and update the
README's status section. Until then the README must not claim the VM boundary works.
