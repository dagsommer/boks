# Boks agent images

One base image, one thin layer per agent. A user pulls the base once; each agent adds only
its own CLI on top.

```
ghcr.io/dagsommer/boks/base:<tag>          ← the shell agent runs this
ghcr.io/dagsommer/boks/claude:<tag>        ← FROM base + one CLI
ghcr.io/dagsommer/boks/codex:<tag>
...
```

`internal/agent` maps an agent name to one of these. The tag comes from a single constant,
`agent.ImageTag`; the `Makefile` and the release workflow both read that line rather than
keeping copies of it.

## Building

```bash
make images        # base + every agent, for this machine's architecture
make images-test   # the CLI runs, uid is 1000, tini is PID 1, an injected CA lands
```

Multi-arch (amd64 + arm64) is CI's job — `.github/workflows/images.yml` uses a native runner
per architecture. Building the other architecture locally under emulation would take an hour
and prove nothing the native build does not.

## The base

Debian 13 (trixie) slim, because it is the smaller of the two obvious slim images, its glibc
is 2.41, and uid 1000 is unoccupied — `ubuntu:24.04` gives that uid to a distro user who
would have to be deleted first.

It carries `git`, `curl`, `ca-certificates`, `openssh-client`, `build-essential`, `ripgrep`,
`jq`, `unzip`, Node LTS from nodejs.org (checksum-verified against the release's own
`SHASUMS256.txt`) and Debian's Python 3 with `pip` and `venv`. The point of preinstalling is
that a sandbox's first minute is not spent on `npm install`.

Two things about it are load-bearing:

- **`tini` is PID 1.** A sandbox that lives for hours needs something to reap orphans; a real
  Docker Sandboxes guest was observed running `tini` for the same reason.
  `sandbox.keeperCommand` cannot supply one, because it has to work in an arbitrary image —
  so it belongs here.
- **uid/gid 1000, home `/home/agent`.** This matches the kit convention (`setup.startup`
  defaults to user `"1000"`) and what a real sbx guest runs as.

## CA trust

Boks injects credentials by terminating TLS for the hosts a credential rule names, which
means the guest is offered a certificate from a CA generated on the host. `boks ca env`
prints it as `BOKS_CA_CERT_B64`.

`/usr/local/bin/boks-install-ca` decodes that variable into
`/usr/local/share/ca-certificates/boks-local-ca.crt` and runs `update-ca-certificates`. It
does nothing when the variable is unset, so an image that intercepts nothing still starts.
`/usr/local/bin/boks-entrypoint` runs it and then `exec`s the command.

Node and Python ignore the system store, so the images also export:

```
NODE_EXTRA_CA_CERTS=/etc/ssl/certs/ca-certificates.crt
REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt
SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
```

All three name the *system* bundle rather than the injected certificate. That file always
exists, so nothing breaks when no CA is injected, and `update-ca-certificates` appends the
Boks CA to it in place when one is.

A sandbox does **not** use the image's `ENTRYPOINT`: the OCI spec is built with containerd's
`WithProcessArgs`, which replaces the whole argv. `agent.Agent.Init` carries the same
`tini -- boks-entrypoint` prefix so every path through `Argv` keeps it. Two paths still miss
it, and they are listed as gaps in `docs/docker-sandbox-parity.md`: a persistent sandbox runs
an idle keeper as its container process, and `boks exec` runs the command it was given.
`boks exec <sandbox> boks-install-ca` covers both — the write persists in the snapshot.

## Supply chain

The risk that matters here is **build time**, not run time. A compromised agent CLI running
inside a microVM is largely what Boks exists to contain. A compromised *dependency* executing
a `postinstall` during `docker build` runs on the build machine, in CI, beside the token that
publishes to GHCR — that is a compromise of our distribution.

So:

- **Nothing is installed from a package manager.** Every agent CLI is a vendor-published
  artifact, pinned to an exact version, verified against a SHA-256 recorded in its
  Dockerfile. A bump is a reviewable diff. No `@latest`, and no `curl | bash` — piping an
  unpinned script from a URL into a shell is not an improvement on npm, it is the same
  problem with fewer records.
- **Zero packages are installed at build time**, so there are no transitive dependency
  postinstall scripts to disable. `--ignore-scripts` and pnpm's `onlyBuiltDependencies`
  allowlist were both considered and are both moot: with no package manager in the build,
  there is no lifecycle script to permit or deny. (The vendor artifacts do of course bundle
  their own dependencies — that is the vendor's tree, shipped as reviewed bytes, not code we
  execute during a build.)
- **Build and push are separate CI jobs.** The job that runs vendor installers holds no
  registry credential; the job that holds `packages: write` runs no third-party code, only
  `skopeo copy`. The workflow says so in a comment, because it will look like it wants to be
  one job.
- **Provenance and an SBOM** are attached to every image (`--provenance=mode=max --sbom=true`).

### What each agent installs

| Agent | Channel | Pinned version | Verification | Package manager |
|---|---|---|---|---|
| `claude` | `downloads.claude.ai` native binary | 2.1.228 | SHA-256 from the vendor's own release manifest | none |
| `codex` | GitHub release, static musl binary | 0.147.0 (`rust-v0.147.0`) | SHA-256 recorded here | none |
| `copilot` | GitHub release tarball | 1.0.79 | SHA-256 recorded here | none |
| `cursor` | `downloads.cursor.com` tarball | 2026.08.04-aaa8809 | SHA-256 recorded here | none |
| `docker-agent` | GitHub release, static Go binary | 1.124.0 | SHA-256 recorded here | none |
| `droid` | `downloads.factory.ai` binary | 0.193.0 | vendor-published `.sha256`, recorded here | none |
| `gemini` | GitHub release, self-contained JS bundle | 0.54.4 | SHA-256 recorded here | none |
| `opencode` | GitHub release tarball | 1.18.16 | SHA-256 recorded here | none |

Where a vendor publishes its own checksum (Claude Code, Droid) that is what is recorded.
Where one does not (Cursor, and the GitHub release assets), the digest attests "the same
bytes we reviewed at this version", not "the bytes the vendor signed" — a real but smaller
guarantee, and still strictly better than running an installer.

On x86-64, Droid and OpenCode use their vendors' "baseline" builds. A Dockerfile cannot know
what CPU a guest will have, and the baseline binaries run on any x86-64.

### Not shipped

**Kiro.** Its CLI is distributed as a ~500 MB archive per architecture — roughly triple the
size of any image here — and the installer resolves its download through a `stable/latest`
manifest with no documented version-pinned URL, so there is no artifact to pin and checksum
the way everything above is. Both would have to change. The name stays registered in
`internal/agent` without an image, so `boks run kiro` says the environment is missing rather
than that the name is wrong.

Three npm packages that *look* like the missing agents are not them, and were checked rather
than assumed: `kiro-cli` is a placeholder published by an unrelated account, `droid-cli` is an
Android tool, and `cagent` on npm is unrelated to Docker's agent (which lives at
`github.com/docker/docker-agent`).

## Sizes

Measured on linux/arm64 after `make images`. "Added" is over the shared base, which is pulled
once regardless of how many agents are used.

| Image | Compressed | On disk | Added over base (compressed) |
|---|---|---|---|
| `base` | 219 MB | 926 MB | — |
| `gemini` | 243 MB | 1.07 GB | 24 MB |
| `docker-agent` | 252 MB | 1.09 GB | 33 MB |
| `opencode` | 278 MB | 1.17 GB | 60 MB |
| `droid` | 282 MB | 1.14 GB | 64 MB |
| `cursor` | 302 MB | 1.27 GB | 84 MB |
| `claude` | 309 MB | 1.33 GB | 91 MB |
| `codex` | 310 MB | 1.24 GB | 92 MB |
| `copilot` | 397 MB | 1.50 GB | 179 MB |

The base is large mostly because of `build-essential` (~445 MB on disk) and Node (~146 MB
after dropping the 65 MB of native-addon headers, which `node-gyp` re-fetches anyway). Both
were kept: an agent that cannot compile a native module is a worse outcome than a large
image that is pulled once.

## What has and has not been verified

Verified with `docker run` on linux/arm64: every image builds, every CLI starts and reports
its version, the process is uid 1000, `tini` is PID 1, and a certificate from `boks ca env`
reaches `/usr/local/share/ca-certificates` and the system bundle.

**Not** verified: nothing here has been run inside a microVM — that needs a hypervisor, which
the machine these were built on does not have. A `docker run` result says the CLI is
installed and starts. It says nothing whatsoever about the VM boundary. The amd64 half of
every image is likewise unbuilt here; it is CI's first job to prove it.
