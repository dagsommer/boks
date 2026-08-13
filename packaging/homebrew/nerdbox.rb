# containerd's nerdbox VM shim, built from source and signed with the entitlement libkrun
# needs. See packaging/homebrew/README.md for how this reaches a tap, and docs/install.md
# for what it does and does not give you.
#
# Why this formula exists at all: nerdbox is packaged nowhere. Not homebrew-core, not the
# AUR, not nixpkgs, not Debian, and — checked 2026-08-13 — not by Repology in any of the
# ~400 repositories it tracks. Its own release workflow has failed on every tag since
# v0.2.0, so all ten of its GitHub releases carry zero assets. There is nothing to
# download, for any platform. Building it here is not a preference.
class Nerdbox < Formula
  desc "containerd shim that runs each container as a libkrun microVM"
  homepage "https://github.com/containerd/nerdbox"
  url "https://github.com/containerd/nerdbox/archive/refs/tags/v0.2.3.tar.gz"
  sha256 "8eb4c638d161701f93b01ec2c84fbc4891a0be98a10d1887473095c6c309cbc1"
  license "Apache-2.0"

  # This formula is pinned to a nerdbox tag on purpose, and the pin is a Boks decision
  # rather than a packaging convenience: v0.2.3 is the release containing cd2c23f, which is
  # the commit docs/verification.md records the VM boundary being verified against. Moving
  # it means the shim under Boks is no longer the shim the evidence was collected with, so
  # bump it deliberately and re-run the procedure in that document.
  livecheck do
    url :stable
    strategy :github_latest
  end

  # Apple silicon only, matching libkrun: upstream supports Hypervisor.framework on arm64
  # and nothing else, and libkrun's own formula carries the same restriction.
  depends_on arch: :arm64
  depends_on :macos

  depends_on "go" => :build
  # libkrun lives in a third-party tap. Because it is a *dependency* rather than something
  # you typed, Homebrew will not trust it implicitly — see the trust note in
  # packaging/homebrew/README.md and docs/install.md.
  depends_on "libkrun/krun/libkrun"

  def install
    # What nerdbox's own `task build:shim` does, minus the parts that need Docker. The
    # no_grpc tag is upstream's; the shim links no C and dlopens libkrun at runtime through
    # purego, so a plain cross-free `go build` is the whole compile.
    system "go", "build", *std_go_args(
      output: bin/"containerd-shim-nerdbox-v1",
      tags:   "no_grpc",
    ), "./cmd/containerd-shim-nerdbox-v1"

    # Kept for post_install, which runs after Homebrew has finished relocating the keg. The
    # build directory is gone by then.
    libexec.install "cmd/containerd-shim-nerdbox-v1/containerd-shim-nerdbox-v1.entitlements"
  end

  # The signature is applied here, not in `install`, and the ordering is the whole reason.
  #
  # libkrun cannot use Hypervisor.framework unless the process carries the
  # `com.apple.security.hypervisor` entitlement. Without it a sandbox does not fail to
  # start — it starts and then dies inside libkrun with `krun_start_enter failed: -22`,
  # which names neither code signing nor the entitlement. `boks doctor` has a check for
  # exactly this (`runtime entitlement`) because the error names nothing.
  #
  # Homebrew re-signs Mach-O files whose load commands it has had to patch, in
  # `fix_dynamic_linkage`. On Intel it re-signs with
  # `--preserve-metadata=entitlements,requirements,flags,runtime`; on Apple silicon it uses
  # ruby-macho's `MachO.codesign!`, which writes a plain ad-hoc signature and carries no
  # entitlements across. A shim signed during `install` — or baked into a bottle — could
  # therefore arrive unsigned-in-the-way-that-matters on precisely the architecture this
  # formula is restricted to. `fix_dynamic_linkage` runs before `post_install`
  # (FormulaInstaller#finish), so signing here is applied last and survives, whether the
  # keg was built from source or poured from a bottle.
  def post_install
    system "codesign", "--sign", "-", "--force",
           "--entitlements", libexec/"containerd-shim-nerdbox-v1.entitlements",
           bin/"containerd-shim-nerdbox-v1"
  end

  # The honest part. This formula installs the shim; it cannot install the guest.
  #
  # nerdbox boots a Linux kernel and an EROFS root filesystem that it builds with
  # `docker buildx bake` — the kernel from a kernel.org tarball, the rootfs with mkfs.erofs
  # over a Go init and a downloaded crun. Homebrew builds have no Docker and no Linux
  # cross-toolchain, so neither can be produced here, and there is nothing published
  # anywhere to download instead.
  #
  # `boks doctor` reports the gap as `guest image`, scanning the same PATH and LIBKRUN_PATH
  # the shim does — but it reports it after the install, to someone who may not run it.
  # Hence the caveat too, in the loudest place available.
  def caveats
    <<~EOS
      This formula installed the shim and signed it with com.apple.security.hypervisor.
      It did NOT install the guest kernel or root filesystem, which nerdbox builds with
      Docker and publishes nowhere. Until those two files exist, a sandbox fails at boot
      with:

        nerdbox-kernel not found in PATH or LIBKRUN_PATH

      `boks doctor` reports this as `guest image`: fail.

      Build them once, on any machine with Docker (they are architecture-specific but not
      host-specific), and install them where the shim looks:

        scripts/build-nerdbox-guest.sh          # in a boks checkout
        cp nerdbox-kernel-arm64 nerdbox-rootfs.erofs #{HOMEBREW_PREFIX}/lib/

      #{HOMEBREW_PREFIX}/lib is on the shim's own search path on Apple silicon, so nothing
      else needs configuring. Any directory on containerd's PATH, or on LIBKRUN_PATH, also
      works.

      containerd resolves this shim through its own PATH, which is the daemon's and not
      your shell's. If containerd cannot find #{opt_bin}, start it with that directory on
      PATH.
    EOS
  end

  test do
    # The shim is a containerd plugin: it speaks ttrpc on a socket handed to it by
    # containerd and has no meaningful standalone invocation. What can be asserted without
    # a hypervisor is that it is present, executable, and — the thing that actually breaks
    # — carries the entitlement.
    assert_predicate bin/"containerd-shim-nerdbox-v1", :executable?

    entitlements = shell_output(
      "codesign -d --entitlements - #{bin}/containerd-shim-nerdbox-v1 2>&1",
    )
    assert_match "com.apple.security.hypervisor", entitlements
  end
end
