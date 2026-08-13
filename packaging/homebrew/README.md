# The Homebrew tap

`boks.rb` and `nerdbox.rb` are the formulae. They live here because the tap repository does
not exist yet and cannot be created from inside this one — a Homebrew tap is a *separate*
GitHub repository with a fixed name. Everything below is what the owner has to do, written
so it can be pasted.

**None of this has been executed.** There is no macOS machine and no Homebrew installation
on the machine these files were written on, so the formulae have been checked for Ruby
syntax and read against Homebrew's current source, and nothing more. `brew install` has
never been run against either of them. Treat the first run as the test.

## What the owner has to create

### 1. The repository

Homebrew resolves `brew tap dagsommer/boks` to `github.com/dagsommer/homebrew-boks`. The
`homebrew-` prefix is not optional and the tap name is what remains after it.

```sh
gh repo create dagsommer/homebrew-boks --public \
  --description "Homebrew formulae for Boks"
git clone https://github.com/dagsommer/homebrew-boks
cd homebrew-boks
mkdir -p Formula
cp /path/to/boks/packaging/homebrew/boks.rb Formula/
cp /path/to/boks/packaging/homebrew/nerdbox.rb Formula/
git add Formula && git commit -m "boks and nerdbox" && git push
```

Formulae may sit in the repository root, but `Formula/` is the convention and `brew` will
look there first.

### 2. The one placeholder

`boks.rb` carries a `sha256` of all zeroes, because the release tarball it names does not
exist until the tag is pushed. It is a placeholder rather than a guess on purpose: a wrong
checksum fails with a mismatch and no explanation, whereas an obviously fake one is
recognisable as unfilled.

After pushing the `v0.1.0` tag and letting `release.yml` finish:

```sh
curl -sL https://github.com/dagsommer/boks/archive/refs/tags/v0.1.0.tar.gz | shasum -a 256
```

and paste it into `Formula/boks.rb`. `nerdbox.rb` needs nothing — its checksum is real,
taken from the published v0.2.3 tarball.

> GitHub's auto-generated tag tarballs are stable in practice but are not contractually
> byte-stable across changes to GitHub's gzip. If that ever bites, switch the formula's
> `url` to the `boks_<version>_darwin_arm64.tar.gz` asset the release workflow uploads,
> whose checksum this project controls and publishes in `SHA256SUMS`.

### 3. Nothing else

No signing key, no bottle, no CI. Bottles are optional and worth adding later — see below —
but a source-built formula works from the first push.

## What a user then types

```sh
brew tap dagsommer/boks
brew trust dagsommer/boks
brew trust --formula libkrun/krun/libkrun
brew install boks
```

**Three commands where `sbx` uses two, and the third is not avoidable.** Since Homebrew
6.0.0, non-official taps require explicit trust: loading a formula runs Ruby from that
repository, so Homebrew asks you to say which non-official code you accept. This is why
Docker's line reads `brew trust docker/tap && brew install docker/tap/sbx`.

There is a shortcut, and it is worth knowing exactly how far it goes. Naming a formula in
full on the command line trusts *that formula only*, so this works with no `brew trust` at
all:

```sh
brew install dagsommer/boks/nerdbox dagsommer/boks/boks
```

Both are named, so both are trusted. It works because Homebrew's check
(`Trust.explicitly_allowed?`) looks for the fully-qualified name **in `ARGV`** — which is
why `brew install dagsommer/boks/boks` on its own is *not* enough: `nerdbox` and
`libkrun/krun/libkrun` are then dependencies rather than things you typed, and a dependency
is never implicitly trusted.

`libkrun/krun/libkrun` always needs explicit trust, because it is always a dependency and
never the thing being installed. The documented instruction in `docs/install.md` therefore
uses `brew trust` for both taps, which is the form that keeps working if the dependency
graph changes.

> The libkrun tap moved. `slp/krun` and `libkrun/krun` are the same repository — GitHub
> redirects the old name — and the formulae are published under `libkrun/homebrew-krun`
> today. `docs/verification.md` records libkrun 1.19.4 and libkrunfw 5.5.0 on the verified
> machine, which are exactly the versions that tap ships, but it does not name the tap
> itself; `libkrun/krun` is the current canonical name rather than a transcription of what
> was installed.

## Bottles, if you want them later

A bottle turns `brew install boks` from a Go compile into a download. It needs a
`brew bottle` run on an Apple silicon machine per macOS version and somewhere to host the
files — a GitHub release on `homebrew-boks` is the usual answer, via `root_url`, exactly as
`libkrun/krun` does it.

**One thing must not be forgotten if `nerdbox` is ever bottled.** Homebrew re-signs Mach-O
files whose load commands it patches while relocating a keg, and on Apple silicon it does
so with ruby-macho's `MachO.codesign!`, which writes a plain ad-hoc signature carrying no
entitlements. A shim signed at bottle-build time could therefore arrive with its
`com.apple.security.hypervisor` entitlement gone — and the symptom is not a signing error,
it is `krun_start_enter failed: -22` from inside libkrun at boot. The formula avoids this
by signing in `post_install`, which runs after relocation and runs on a poured bottle too.
Do not move it into `install` for tidiness.

## The gap this tap does not close

`nerdbox.rb` installs the shim. It does not install nerdbox's **guest kernel** or **root
filesystem**, because those are built with `docker buildx bake` and Homebrew has neither
Docker nor a Linux cross-toolchain, and because there is nothing published anywhere to
download instead — nerdbox's own release workflow has failed on every tag since v0.2.0 and
all ten of its releases carry zero assets.

So after `brew install boks`, `boks doctor` should pass, and a sandbox will still fail to
boot with `nerdbox-kernel not found in PATH or LIBKRUN_PATH` until two files exist.
`scripts/build-nerdbox-guest.sh` builds them; `docs/install.md` says where to put them.
That script **has been run**: on 2026-08-13 it produced a real arm64 kernel Image and a
real EROFS filesystem in about four minutes on an ordinary Linux host, so this is a
publishing decision rather than an engineering problem.

**This is the one thing standing between "installs" and "works", and closing it is the
owner's decision.** Once those two files are published as assets on a Boks release, the
formula change is small and local:

```ruby
  resource "guest" do
    url "https://github.com/dagsommer/boks/releases/download/v0.1.0/nerdbox-guest-arm64-0.2.3.tar.gz"
    sha256 "…"
  end

  # inside install:
  resource("guest").stage { lib.install "nerdbox-kernel-arm64", "nerdbox-rootfs.erofs" }
```

`#{HOMEBREW_PREFIX}/lib` is already on the shim's own search path on Apple silicon, so no
configuration follows from it. What *does* follow is a licensing question worth answering
before publishing rather than after: the kernel is GPL-2.0, so distributing a compiled one
carries a corresponding-source obligation. It is built unmodified from a `cdn.kernel.org`
tarball at a version nerdbox pins, with a config from nerdbox's repository, so satisfying
it is a matter of publishing that recipe alongside — but it is a decision, not a detail.

## If homebrew-core ever packages nerdbox

It may. `containerd`'s own formula carries the caveat *"You need to install an additional
runtime plugin such as nerdbox (not packaged in Homebrew yet)"*, which reads like an
intention. If a core `nerdbox` appears, this tap's formula becomes an ambiguous name and
`brew` will start asking users which one they meant. At that point delete `nerdbox.rb` here
and point `boks.rb` at the core formula — provided the core one signs the shim with the
hypervisor entitlement. If it does not, keep this one and rename it
`containerd-shim-nerdbox`.
