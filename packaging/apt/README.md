# The Debian/Ubuntu packages, and the key that signs the repository

`apt` refuses an unsigned repository outright, so a `.deb` archive needs an OpenPGP key
before it needs anything else. This directory holds the public half. What the packages
themselves contain is below it, because the two questions get asked together.

## What is in the package

`scripts/package-linux.sh` builds both the `.deb` and the `.rpm`. Since 2026-08-15 they carry
the runtime, not just the CLI:

| Path | What | Mode |
|---|---|---|
| `/usr/bin/boks` | the CLI, and the only thing on anybody's `PATH` | 0755 |
| `/usr/libexec/boks/containerd` | the daemon `boks daemon` starts | 0755 |
| `/usr/libexec/boks/containerd-shim-nerdbox-v1` | the runtime shim containerd execs | 0755 |
| `/usr/libexec/boks/libkrun.so` | the VMM the shim `dlopen`s | 0644 |
| `/usr/libexec/boks/nerdbox-kernel-<arch>` | the guest kernel, **when available** | 0644 |
| `/usr/libexec/boks/nerdbox-rootfs-<arch>.erofs` | the guest rootfs, **when available** | 0644 |
| `/usr/share/doc/boks/`, completions for bash, zsh and fish | as before | 0644 |

### Why vendored rather than depended on

`Depends: containerd` would produce a machine that looks provisioned and cannot start a
sandbox. Ubuntu 24.04 LTS ships containerd 1.7.x and 26.04 ships 2.2.2; the shim needs 2.3.
Measured on WSL2, 2026-08-15 (`docs/verification.md`): against a 2.2 daemon a sandbox dies at
task start with `unsupported protocol: Yunix`, which is protobuf framing rendered as letters
and names no version. nerdbox is packaged by no distribution at all, and libkrun by none Boks
targets. `packaging/containerd-linux/README.md` has the full argument and the sizes.

### Why `/usr/libexec/boks`

These are Boks' private executables, not commands a user runs. A `containerd` in `/usr/bin`
would collide at the file level with the distribution's own `containerd` package — dpkg would
refuse the install — and would shadow on `PATH` a containerd somebody installed on purpose.

The path is not free choice: `internal/daemon/locate.go` searches `<boks exe dir>` and then
`<boks exe dir>/../libexec/boks`, which for `/usr/bin/boks` is exactly `/usr/libexec/boks`.
`boks daemon` then *prepends* that directory to the `PATH` it gives containerd, so containerd
resolves the shim there and the shim — which scans its own `PATH` for libkrun and the guest
images — finds those there too. One directory answers all four lookups.

Note it is `/usr/libexec` literally, not rpm's `%{_libexecdir}`, which is `/usr/lib` on some
distributions. A package that honoured the macro would install cleanly and find nothing.

### Nothing starts on install

There is no `postinst`, no `prerm`, no `postrm`, no systemd unit and no boot hook, in either
format. Boks' promise is that a host which has not run `boks daemon start` runs no Boks
process at all, and a maintainer script is the usual place that promise gets quietly broken.
Nothing needs one: `libkrun.so` is `dlopen`ed by the full path the shim builds itself, so no
`ldconfig` entry is wanted — registering it with the dynamic linker would be actively wrong —
the state directory is created by the CLI on demand, and there is nothing to clean up on
removal that the package manager does not remove itself.

Verified by unpacking a built `.deb` on 2026-08-15: its control archive holds `control` and
`md5sums` and nothing else, and `rpm -qp --scripts` on the `.rpm` prints nothing.

### `Recommends: erofs-utils`, and why it is not `Depends:`

`mkfs.erofs` is the one runtime piece not vendored, because it is the one that distributions
package properly. It stays a `Recommends` for two reasons. A `Depends` would be satisfied by
Ubuntu 24.04's erofs-utils 1.7.1, which is below the 1.8 floor `internal/doctor/erofs.go`
enforces — so the dependency would be met and the machine still broken, which is the same
failure `Depends: containerd` produces. And Boks degrades correctly without it rather than
breaking: `boks daemon` omits the erofs differ from the diff order when `mkfs.erofs` is
absent and says so in the generated configuration, because *naming* a skipped differ fails
the whole diff service (measured, `internal/daemon/config.go`).

### Two known gaps in the packaging

- **The `.deb` carries no `Depends:` at all**, and it should carry a versioned `libc6`.
  `boks` and `containerd` are built `CGO_ENABLED=0` and link nothing, but the nerdbox shim
  and `libkrun.so` are dynamically linked against glibc. The usual answer, `dpkg-shlibdeps`,
  resolves against libraries installed on the *build* host, which a cross-built package does
  not have. The `.rpm` gets this for free — rpm's ELF dependency generator reads the version
  requirements out of the binaries, and a built package lists `libc.so.6(GLIBC_2.34)` and
  friends. Recorded rather than papered over with a guessed version.
- **`boks doctor` under-reports a correctly installed package.** Three of its checks —
  `vm runtime`, `hypervisor library` and `guest image` — look on the invoking shell's `PATH`
  rather than in the bundle, so they report the vendored shim, libkrun and guest images as
  missing while `runtime skew`, which searches `daemon.RuntimeDirs()`, finds the same shim in
  the same directory. Measured on 2026-08-15 from an unpacked `.deb`. This is a Go-side fix in
  `internal/doctor/` — those checks should consult `daemon.RuntimeDirs()` first, the way
  `internal/doctor/skew.go` already does — and not something packaging can work around.

## The key

The rest of this file is about the repository signing key, which is unchanged: the maintainer's
decision is **unsigned binaries and GPG-signed checksums**, and nothing in the packaging above
touches it.

| | |
| --- | --- |
| Fingerprint | `D5DD07C0F9589C164F7361C20EB93D3C39471E1E` |
| Type | RSA 4096, sign-only |
| Created | 2026-08-13, expires 2029-08-13 |
| Public half | [`boks-archive-keyring.asc`](boks-archive-keyring.asc) — committed here, it is meant to be published |
| Private half | GitHub Actions secret `APT_GPG_PRIVATE_KEY`, with the fingerprint in `APT_GPG_KEY_FPR` |

The private key was generated with `%no-protection` — no passphrase. That is deliberate for
an unattended CI signer, where a passphrase would only move the secret rather than protect
it: anything that can read the passphrase secret can read the key secret beside it.

## What this key is and is not

It signs **repository metadata**, so that apt can tell that the index it fetched is the one
we published. It is not a statement about the contents of a package, and it is not a code
signing certificate — see `docs/install.md` for the macOS entitlement signing and the
Windows SmartScreen story, which are separate problems with separate answers.

## Provenance, which matters more here

Signing proves the archive came from whoever holds this key. It does not prove the binary
came from the source. Releases therefore also carry
[build provenance](https://docs.github.com/actions/security-guides/using-artifact-attestations)
attesting the commit and workflow run that produced each artifact, which is verifiable with
`gh attestation verify` and does not require trusting a key at all.

## Using it

```sh
curl -fsSL https://raw.githubusercontent.com/dagsommer/boks/main/packaging/apt/boks-archive-keyring.asc \
  | sudo tee /usr/share/keyrings/boks-archive-keyring.asc >/dev/null
echo "deb [signed-by=/usr/share/keyrings/boks-archive-keyring.asc] https://dagsommer.github.io/boks/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/boks.list
sudo apt update && sudo apt install boks
```

`signed-by` scopes the key to this one repository. A key dropped in `trusted.gpg.d`
instead would be trusted to sign *any* repository on the system, including ones it has
nothing to do with — a footgun that predates `signed-by` and still gets copied around.

## Rotation

The expiry is real: when it passes, `apt update` starts failing for everyone who added the
key. Rotate well before 2029, and expect to publish both keys for a period so clients that
have not updated keep working.

To rotate, or if the key is ever suspected compromised:

```sh
gpg --batch --gen-key <<'EOF'
%no-protection
Key-Type: RSA
Key-Length: 4096
Key-Usage: sign
Name-Real: Boks Archive Signing Key
Name-Comment: apt/rpm repository signing
Name-Email: dagaleks@gmail.com
Expire-Date: 3y
%commit
EOF
gpg --armor --export "$FPR" > boks-archive-keyring.asc
gpg --armor --export-secret-keys "$FPR" | gh secret set APT_GPG_PRIVATE_KEY -R dagsommer/boks
gh secret set APT_GPG_KEY_FPR -R dagsommer/boks --body "$FPR"
```

Then delete every local copy of the private half. The current key was generated this way in
an ephemeral keyring which was shredded immediately afterwards, so the GitHub secret is the
only copy — losing it means rotating, not recovering, which is the correct trade for a key
that signs an archive nobody has installed yet.
