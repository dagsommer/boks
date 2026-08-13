# The Boks CLI, and the stack it needs, as far as a formula can honestly take it.
#
# See packaging/homebrew/README.md for how this reaches a tap and what the owner has to do
# that a checked-in file cannot, and docs/install.md for the user-facing instructions.
class Boks < Formula
  desc "Run coding agents in isolated microVMs, locally"
  homepage "https://github.com/dagsommer/boks"
  url "https://github.com/dagsommer/boks/archive/refs/tags/v0.1.0.tar.gz"
  # PLACEHOLDER. This cannot be a real value until the tag exists, and inventing one would
  # produce a formula that fails at the checksum with no explanation of why. Fill it with:
  #
  #   curl -sL https://github.com/dagsommer/boks/archive/refs/tags/v0.1.0.tar.gz | shasum -a 256
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"
  head "https://github.com/dagsommer/boks.git", branch: "main"

  livecheck do
    url :stable
    strategy :github_latest
  end

  # macOS on Apple silicon is the only platform where Boks has been shown to work, and the
  # only one where the stack below it can be installed at all: libkrun's formula is arm64
  # and Hypervisor.framework only. Linux users are better served by the .deb, .rpm or
  # tarball on the release — see docs/install.md — which is why this formula declines to
  # install rather than pretending.
  depends_on arch: :arm64
  depends_on :macos

  depends_on "go" => :build

  # The whole stack, not just the CLI. `boks doctor` checks all of these, and a formula
  # that installed one of five and left the user to discover the other four from failing
  # checks would not be much of an install.
  #
  # containerd 2.2+ (homebrew-core is at 2.3.4) and erofs-utils, for mkfs.erofs, which the
  # snapshotter shells out to when unpacking an image.
  depends_on "containerd"
  depends_on "erofs-utils"
  # The VM shim, from this same tap, because it is packaged nowhere else. Read its caveats:
  # it installs the shim and signs it, and cannot install the guest kernel and rootfs.
  # It pulls in libkrun from the libkrun/krun tap in turn.
  depends_on "dagsommer/boks/nerdbox"

  def install
    ldflags = "-X github.com/dagsommer/boks/internal/cli.Version=#{version}"
    system "go", "build", *std_go_args(ldflags: ldflags), "./cmd/boks"

    generate_completions_from_executable(bin/"boks", "completion")
  end

  # Two things remain after this formula finishes, and neither is something a package can
  # do for you. Both produce errors that do not name their own cause, which is why they are
  # spelled out here rather than left to be discovered.
  def caveats
    <<~EOS
      Run `boks doctor` now. It checks every prerequisite and prints what to do about each
      gap. Two of them are not this formula's to fix:

      1. containerd must be running, and its state directory must be yours.

         containerd derives the shim's socket path from a compile-time constant, so no
         config setting moves it (containerd#12444). Once, as root:

           sudo mkdir -p /var/run/containerd
           sudo chown "$(id -u):$(id -g)" /var/run/containerd

         This is the only step that needs root. Run containerd rootless afterwards; it
         works, and its `root` and `state` can live under $HOME. Start it with
         #{HOMEBREW_PREFIX}/bin on PATH — containerd resolves the shim through the
         daemon's PATH, not your shell's.

      2. The nerdbox guest kernel and root filesystem are not installed.

         `brew info nerdbox` explains why and how to build them, and `boks doctor`
         reports them as `guest image`.

      Full instructions, including what has and has not been verified on this platform:
      https://github.com/dagsommer/boks/blob/main/docs/install.md
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/boks --version")

    # `boks doctor` is deliberately not run here: it is expected to fail on a machine
    # without a running containerd, and a test that asserts a healthy host would fail in
    # CI for reasons that have nothing to do with this formula. Assert instead that the
    # binary is the real CLI and that its completions were generated.
    assert_match "sandbox", shell_output("#{bin}/boks --help")
    assert_path_exists bash_completion/"boks"
  end
end
