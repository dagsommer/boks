# Homebrew

The tap that puts Boks in `brew install boks`, and the part of getting it there that this
repository cannot do for itself — a Homebrew tap is a *separate* GitHub repository with a
fixed name, and `dagsommer/homebrew-boks` does not exist yet.

> [!IMPORTANT]
> **No macOS machine has run these formulae.** This project has no macOS CI. `brew install`
> has never been attempted against either of them and the first attempt is the test.
>
> What *has* been run, on Linux, on 2026-08-16, is Homebrew itself — a real Homebrew 6.0.17,
> which loads, lints, resolves and fetches for a macOS-only formula perfectly well even
> though it can never install one. `brew style` and `brew audit --strict --new` both pass;
> the dependency graph, the trust behaviour and three of the bottles were exercised for real;
> and `boks.rb`'s `install` and `test` bodies were run end to end, on Linux, with the
> platform requirements removed. [What has actually been checked](#what-has-actually-been-checked)
> lists every command and its output. It is still not a macOS install.

## What is here

| Path | What it is |
|---|---|
| `tap/Formula/boks.rb.in` | the `boks` formula, as a template |
| `tap/Formula/nerdbox.rb.in` | the `nerdbox` formula, as a template |
| `tap/README.md` | the tap repository's own README, shipped verbatim |
| `render.sh` | stamps a version and two SHA-256s into the templates and lints the result |

`tap/` is the tap repository, one file per file it needs, so publishing is a copy rather than
a reassembly. The formulae are templates rather than files-with-a-placeholder-you-edit for
the same reason `packaging/winget/` uses them: a release must not depend on anyone
remembering a substitution. `{{VERSION}}`, `{{SHA256}}` and `{{GUEST_SHA256}}` are the only
things a release changes, they appear nowhere else, and `render.sh` refuses to emit a file
that still contains one.

Template lines beginning `#|` are notes to ourselves and are **stripped at render**, so what
lands in the tap is a formula and not our reasoning about it. Because `#|` is a Ruby comment
and every placeholder sits inside a string literal, a `.rb.in` is itself valid Ruby and
`ruby -c` checks the template as well as the rendered file.

```sh
packaging/homebrew/render.sh 0.1.0 dist/v0.1.0.tar.gz dist/SHA256SUMS
packaging/homebrew/render.sh 0.1.0 <64-hex-source-digest> <64-hex-guest-digest>
```

Output lands in `dist/homebrew/`, which is the tap repository's root layout —
`Formula/boks.rb`, `Formula/nerdbox.rb`, `README.md`.

The two digests differ in where they come from, and the difference is not arbitrary:

- **source** — the GitHub-generated tag tarball. It is *not* in the release's `SHA256SUMS`,
  because GitHub generates it rather than this project, so it has to be fetched and hashed.
- **guest** — `boks-guest_<version>_arm64.tar.gz`, which *is* in `SHA256SUMS`. Hand
  `render.sh` the `SHA256SUMS` file and it reads the right line out of it.

## Publishing the tap

Numbered because the order matters: the formula's `url` and its `guest` resource both point
at things that do not exist until the release is published, and both digests are of files
that do not exist until then either.

**The irreversible steps are 1 and 5.** Everything else can be redone or deleted.

### 1. Publish the v0.1.0 release — irreversible

The release is a **draft**, and a draft has no tag: `gh api repos/dagsommer/boks/git/refs/tags`
returns 404 today, which is why `https://github.com/dagsommer/boks/archive/refs/tags/v0.1.0.tar.gz`
404s and why `brew audit` says so. Publishing the draft is what creates the tag.

The draft's target is the **branch** `main`, so publishing it tags whatever `main` happens to
be at that moment — which is not necessarily the commit whose binaries are attached to it.
Pin it explicitly:

```sh
gh release view v0.1.0 -R dagsommer/boks --json targetCommitish,isDraft
gh release edit v0.1.0 -R dagsommer/boks --target <the-commit-the-assets-were-built-from> --draft=false
```

A published tag and a published release are the two things on this page that cannot be taken
back cleanly: people fetch them, and Homebrew pins a checksum to them.

> `release.yml` triggers on `push:` of a `v*` tag. A tag created by publishing a release
> arrives as a `create` event rather than a `push`, so the workflow is not expected to fire a
> second time — but nobody has watched this happen, so watch it.

### 2. Take the two digests

```sh
curl -sL https://github.com/dagsommer/boks/archive/refs/tags/v0.1.0.tar.gz | shasum -a 256
gh release download v0.1.0 -R dagsommer/boks -p SHA256SUMS -D dist/
```

### 3. Render

```sh
packaging/homebrew/render.sh 0.1.0 <source-digest-from-step-2> dist/SHA256SUMS
```

It prints what it wrote, runs `ruby -c`, and — if `brew` is on `PATH` and a scratch tap
exists — `brew style` and `brew audit --strict --new`. After step 1 the audit should be
**clean**; every finding it reports today is one of the three "not reachable (HTTP status
code 404)" lines that the unpublished release causes.

To get the brew half of that on a machine that has never had the tap:

```sh
brew tap-new dagsommer/boks --no-git      # a scratch tap for linting only
```

`render.sh` deliberately refuses to write into a tap directory that is a git checkout, so it
cannot clobber a real `homebrew-boks` clone as a side effect of linting.

### 4. Read the diff

`dist/homebrew/Formula/*.rb` is what strangers will run as root-adjacent Ruby. It is worth
one read even though it was generated.

### 5. Create the tap repository and push — the repository name is claimable once

Homebrew resolves `brew tap dagsommer/boks` to `github.com/dagsommer/homebrew-boks`. The
`homebrew-` prefix is not optional and the tap name is what remains after it.

```sh
gh repo create dagsommer/homebrew-boks --public \
  --description "Homebrew formulae for Boks — run coding agents in isolated microVMs"

git clone https://github.com/dagsommer/homebrew-boks /tmp/homebrew-boks
cp -R dist/homebrew/. /tmp/homebrew-boks/
git -C /tmp/homebrew-boks add Formula README.md
git -C /tmp/homebrew-boks commit -m "boks 0.1.0 and nerdbox 0.2.3"
git -C /tmp/homebrew-boks push
```

Formulae may sit in the repository root, but `Formula/` is the convention and `brew` looks
there first.

### 6. Install it, on a Mac, and find out

```sh
brew tap dagsommer/boks
brew trust dagsommer/boks
brew trust libkrun/krun
brew install boks
boks doctor
```

Expect this to go wrong the first time and write down how. `docs/verification.md` is where
the answer belongs.

### 7. Only then, change the docs

`docs/install.md` and the tap's own README both carry a "this has never been run" notice.
They stop being true at step 6 and not before.

## What `brew install boks` actually does on a clean Mac

Computed from the published `formulae.brew.sh` API and the real `libkrun/krun` tap on
2026-08-16, not estimated. The macOS keg list is the API's **top-level** dependency set —
`variations` carries the Linux overrides, which is the trap: `brew deps --os=tahoe` run on a
Linux host answers from Linux data and reports 51 runtime kegs including `mesa` and the whole
X11 stack. The real answer is much smaller.

### macOS 26 Tahoe and macOS 15 Sequoia — 14 kegs, 2 compiled

| | |
|---|---|
| poured from bottles | `containerd` 2.3.4, `erofs-utils` 1.9.3, `dtc`, `libepoxy`, `libyaml`, `lz4`, `molten-vk`, `xz`, `libkrun` 1.19.4, `libkrunfw` 5.5.0, `virglrenderer` 0.10.4e |
| build-only | `go` 1.26.6 (bottled) |
| compiled from source | `nerdbox`, `boks` — both plain Go builds |
| downloaded, not built | `boks-guest_<v>_arm64.tar.gz`, 13.5 MB |

Nothing in that list is a long build. `libkrun` and its two tap dependencies **have arm64
bottles**, which is the single fact that decides whether this install is pleasant, and it was
checked by fetching them: 2.2 MB, 13 MB and 500 KB respectively, all three checksum-verified.
The two Go compiles and the bottle downloads dominate; on a fast link this should be minutes,
not tens of minutes. **No one has timed it.**

### macOS 14 Sonoma — 31 kegs, and this is the unpleasant surprise

`libkrun`, `libkrunfw` and `virglrenderer` bottle **only** `arm64_tahoe` and `arm64_sequoia`.
Checked directly:

```
$ curl -sI https://github.com/libkrun/homebrew-krun/releases/download/libkrun-1.19.4/libkrun-1.19.4.arm64_sonoma.bottle.tar.gz
HTTP/1.1 404 Not Found
```

So on Sonoma all three build from source, and `libkrun` build-depends on `lld` and `rust`,
which drag in eighteen further build-only kegs — `llvm`, `python@3.14`, `libgit2`, `z3`,
`meson`, `ninja` and the rest. `llvm` alone is a very large download. `containerd` and
`erofs-utils` still pour (they bottle `arm64_sonoma`), so the CLI half is unaffected; the
hypervisor half is where the time goes. Expect tens of minutes. **Not measured, and it cannot
be measured from here.**

There is nothing to fix in these formulae about it. It is the libkrun tap's bottle coverage,
and the only lever this project has is to say so in advance, which `tap/README.md` does.

### The trust prompt comes first, not after a build

`brew trust` is real — `Library/Homebrew/cmd/trust.rb` — and non-official taps have required
it since Homebrew **6.0.0**. The check happens when a formula is *loaded*, so it fails in
under a second, before anything is fetched or compiled. Observed:

```
$ brew install dagsommer/boks/boks
==> Trusted formula dagsommer/boks/boks
Error: Refusing to load formula dagsommer/boks/nerdbox from untrusted tap dagsommer/boks.
```

**Two documented shortcuts do not work, and this README used to claim both of them.**

1. `brew install dagsommer/boks/nerdbox dagsommer/boks/boks` — naming both formulae was said
   to make `brew trust` unnecessary. It does not: the command fails on
   `libkrun/krun/libkrun`, which is a dependency and so was never in `ARGV`.
2. `brew trust --formula libkrun/krun/libkrun` — the form `docs/install.md` documented. Not
   enough either:

   ```
   $ brew trust --formula libkrun/krun/libkrun
   $ brew install boks
   Error: Refusing to load formula libkrun/krun/libkrunfw from untrusted tap libkrun/krun.
   ```

   `libkrun` depends on `libkrunfw` and `virglrenderer` from the same tap, and trust is not
   transitive. Item-level trust needs all three names; `brew trust libkrun/krun` is the form
   that keeps working when that tap's dependency graph changes.

The instruction, then, is four commands: `brew tap`, `brew trust dagsommer/boks`,
`brew trust libkrun/krun`, `brew install boks`.

## What has actually been checked

Homebrew 6.0.17 (`6.0.17-85-gbfae037`), cloned and run on aarch64 Linux on 2026-08-16, with
its portable Ruby, its API index and its RuboCop gems. Everything below is a command that was
run, not a file that was read.

| Check | Result |
|---|---|
| `brew style dagsommer/boks` | **3 files inspected, no offenses detected** — after fixing 7 |
| `brew audit --strict --new dagsommer/boks/{boks,nerdbox}` | only the three unreachable-URL findings the unpublished release causes |
| `ruby -c` on both templates and both rendered formulae | pass |
| `render.sh` end to end | renders, strips `#|`, reads the guest digest out of a real `SHA256SUMS`, refuses leftover placeholders |
| trust behaviour | reproduced all three failure modes above against the real tap-trust code |
| `depends_on :macos` | fires: *"boks: This formula requires macOS. nerdbox: This formula requires macOS. Error: Unsatisfied requirements failed this build."* |
| macOS/arm64 dependency closure | computed from `formulae.brew.sh` + the cloned `libkrun/krun` tap: 14 kegs on Tahoe/Sequoia, 31 on Sonoma |
| `containerd` in homebrew-core | 2.3.4, **no** `depends_on :linux`, bottles `arm64_tahoe`/`arm64_sequoia`/`arm64_sonoma`/`sonoma`. Its own macOS caveat recommends "a runtime plugin such as nerdbox" |
| `erofs-utils`, `go` | 1.9.3 and 1.26.6, Apple-silicon bottles for all three macOS versions |
| `libkrun/krun` tap | tapped for real; `libkrun` 1.19.4, `libkrunfw` 5.5.0, `virglrenderer` 0.10.4e |
| the three tap bottles, `arm64_tahoe` | **downloaded and checksum-verified** — 2.2 MB, 13 MB, 500 KB |
| `nerdbox.rb`'s `sha256` | re-downloaded nerdbox v0.2.3 and hashed it: matches `8eb4c638…09cbc1` |
| `boks --version`, `--help`, `completion {bash,zsh,fish,powershell}` | all exit 0 against a `linux/arm64` build with the formula's own ldflags; `--version` prints `boks 0.1.0`, `--help` contains "sandbox". This is what the `test do` block asserts |
| the guest archive | downloaded `boks-guest_0.1.0_arm64.tar.gz` from the draft release: one top-level directory containing `nerdbox-kernel-arm64`, `nerdbox-rootfs.erofs`, `SHA256SUMS`, `SOURCE.txt` |
| `boks.rb`'s `install` and `test` bodies | **installed and tested for real**, see below |

### The `install` body was actually run

Not on macOS, but not only read either. A throwaway `boksprobe` formula was built in the
scratch tap carrying `boks.rb`'s `install` body verbatim — the same `std_go_args` build, the
same `generate_completions_from_executable(…, shell_parameter_format: :cobra)`, the same
`resource("guest").stage { lib.install … }` against the real 13.5 MB release archive — with
only the `depends_on :macos` / `arch:` lines removed so a Linux host could reach it.

`brew install --build-from-source` succeeded; `go build` took **11 seconds**; `brew test`
exited 0 on all five assertions. The keg:

```
bin/boks
etc/bash_completion.d/boks
share/zsh/site-functions/_boks
share/fish/vendor_completions.d/boks.fish
share/pwsh/completions/_boks.ps1
lib/nerdbox-kernel-arm64
lib/nerdbox-rootfs.erofs
```

Four completions, not three, and both guest files in `lib/`. That settles the two mechanisms
that were otherwise only read: cobra-format completion generation, and whether
`resource(...).stage` descends into a tarball's single top-level directory so a bare
`lib.install "nerdbox-kernel-arm64"` resolves. It does.

**What none of this establishes:** that the same build succeeds under Homebrew's macOS build
environment (which sandboxes, and which this Linux run explicitly did not — *"Sandbox
unavailable: building without sandboxing"*), that `codesign` in `post_install` produces a shim
libkrun accepts, that `/opt/homebrew/lib` is where the shim actually looks at runtime, or that
any of it boots a VM. Those need a Mac.

## Defects this round found and fixed

- **`generate_completions_from_executable(bin/"boks", "completion")`** generated three shells.
  `boks completion powershell` works, so the formula now passes `shell_parameter_format:
  :cobra` and gets four.
- **Seven `brew style` offenses** across the two formulae: `FormulaAudit/DependencyOrder`
  six times (Homebrew wants build deps, then `arch:`, then named runtime deps alphabetically,
  then `:macos`) and `FormulaAudit/Desc` once — nerdbox's `desc` started with a lowercase
  "containerd".
- **The `sha256 "0000…0000"` placeholder** is gone; the value is stamped by `render.sh`
  instead of edited by hand.
- **The guest gap is closed.** The old README proposed a `resource "guest"` block pointing at
  `nerdbox-guest-arm64-0.2.3.tar.gz`, which is not a file any release produces. The real
  asset is `boks-guest_<version>_arm64.tar.gz` and the formula now fetches it, so
  `brew install boks` no longer leaves `boks doctor` reporting `guest image  fail`.
- **Both `brew trust` shortcuts** documented here and in `docs/install.md` were wrong; see
  above.

## Bottles, if you want them later

A bottle turns `brew install boks` from a Go compile into a download — which, per the keg list
above, is already most of what happens on Tahoe and Sequoia, so the saving is small. It needs
a `brew bottle` run on an Apple silicon machine per macOS version and somewhere to host the
files; a GitHub release on `homebrew-boks` reached through `root_url` is the usual answer,
exactly as `libkrun/krun` does it.

**One thing must not be forgotten if `nerdbox` is ever bottled.** Homebrew re-signs Mach-O
files whose load commands it patches while relocating a keg, and on Apple silicon it does so
with ruby-macho's `MachO.codesign!`, which writes a plain ad-hoc signature carrying no
entitlements. A shim signed at bottle-build time could therefore arrive with its
`com.apple.security.hypervisor` entitlement gone — and the symptom is not a signing error, it
is `krun_start_enter failed: -22` from inside libkrun at boot. The formula avoids this by
signing in `post_install`, which runs after relocation and runs on a poured bottle too. Do not
move it into `install` for tidiness.

## If homebrew-core ever packages nerdbox

It may. `containerd`'s own formula carries the caveat *"You need to install an additional
runtime plugin such as nerdbox (not packaged in Homebrew yet)"*, which reads like an
intention. If a core `nerdbox` appears, this tap's formula becomes an ambiguous name and
`brew` will start asking users which one they meant. At that point delete `nerdbox.rb.in` here
and point `boks.rb.in` at the core formula — provided the core one signs the shim with the
hypervisor entitlement. If it does not, keep this one and rename it
`containerd-shim-nerdbox`.
