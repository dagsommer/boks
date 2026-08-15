#!/usr/bin/env bash
#
# Build the Linux packages for one architecture from the tarball build-release.sh produced.
#
#   BOKS_RUNTIME_ASSETS=<dir> scripts/package-linux.sh <goarch> [version] [distdir]
#
# Produces boks_<version>_<arch>.deb and boks-<version>-1.<arch>.rpm, each with a .sha256.
#
# Both are built with the distribution's own tools — dpkg-deb and rpmbuild — rather than
# with a packaging binary fetched from the internet. That is not stylistic: this project
# refuses unpinned downloads everywhere else, and a release pipeline that curls a tool to
# build the thing it is asking people to trust would be the one place it made an exception.
#
# =====================================================================================
# THE INTERFACE WITH CI
# =====================================================================================
#
# This script does not build the runtime. It is handed one, and it refuses to build a
# package without one, because a package that installs cleanly and cannot start a sandbox
# is worse than no package.
#
#   BOKS_RUNTIME_ASSETS   a directory of prebuilt runtime binaries FOR THE TARGET ARCH.
#                         Required. Set it to the literal string `none` to build the
#                         CLI-only package deliberately (see below).
#
# The directory must contain, by these exact names:
#
#   containerd                       the daemon `boks daemon` starts.
#                                      packaging/containerd-linux/build.sh <goarch> <dir>
#   containerd-shim-nerdbox-v1       the runtime shim containerd execs.
#                                      artifact `boks-runtime-linux-<goarch>`
#   libkrun.so                       the VMM the shim dlopens.
#                                      artifact `boks-runtime-linux-<goarch>`
#
# and MAY contain, which this script ships when present and says are absent when not:
#
#   nerdbox-kernel-<x86_64|arm64>            the guest kernel   } artifact
#   nerdbox-rootfs-<x86_64|arm64>.erofs      the guest rootfs   } `nerdbox-guest-<arch>`,
#   nerdbox-rootfs.erofs                     (the unsuffixed spelling is accepted and
#                                             installed under the suffixed name)
#
# Anything else in the directory — a README, a SHA256SUMS, the checksums the runtime
# workflow writes — is ignored, so the artifact can be unpacked as-is.
#
# Every required file is checked with packaging/linux/assert-elf.py before it is staged:
# that it is an ELF64 object of the architecture this package is named for, and, for
# libkrun.so, that it exports all nineteen symbols nerdbox resolves eagerly at dlopen
# (packaging/linux/NERDBOX_SYMBOLS). Both mistakes have been made in this project: an
# arch-swapped artifact reads as "cannot execute binary file", and a same-arch libkrun from
# the Windows pin is missing four of the nineteen and cannot be loaded on any machine. Both
# fail here, at package time, rather than on a user's.
#
# BOKS_RUNTIME_ASSETS=none builds what this script used to build unconditionally: the CLI,
# its licence and three shell completions. That package is honest about itself — its
# description says it carries no runtime and names what has to be installed by hand — and
# it exists so that somebody with dpkg-deb and no runtime artifacts can still exercise this
# script. It is not what a release ships.
#
# =====================================================================================
# WHAT THE PACKAGES CONTAIN, AND WHERE
# =====================================================================================
#
#   /usr/bin/boks                the CLI, and the only thing on anybody's PATH
#   /usr/libexec/boks/*          the runtime: containerd, the shim, libkrun.so, and the
#                                guest images when they are available
#
# /usr/libexec because these are Boks' private executables, not commands a user runs. A
# `containerd` in /usr/bin would collide with the distribution's own containerd package —
# file-level, dpkg would refuse the install — and, worse, would shadow on PATH a containerd
# somebody installed on purpose.
#
# That directory is not a preference: internal/daemon/locate.go searches `<boks exe dir>`
# and then `<boks exe dir>/../libexec/boks`, which for /usr/bin/boks resolves to exactly
# /usr/libexec/boks. `boks daemon` finds containerd there and PREPENDS the same directory
# to the PATH it starts containerd with, so containerd resolves the shim there, and the
# shim — which scans its own PATH for libkrun and the guest images — finds those there too.
# One directory answers all four lookups. See docs/distribution.md, Part 4.
#
# Note that /usr/libexec is used literally rather than through rpm's %{_libexecdir}, which
# is /usr/lib on some distributions. The path is compiled into the search above; a package
# that put the runtime somewhere else would install fine and find nothing.
set -euo pipefail

goarch="${1:?usage: BOKS_RUNTIME_ASSETS=<dir> package-linux.sh <goarch> [version] [distdir]}"

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
version="${2:-$("$root/scripts/image-tag.sh")}"
dist="${3:-$root/dist}"

case "$goarch" in
amd64)
	rpmarch=x86_64
	machine=x86-64
	guestarch=x86_64
	;;
arm64)
	rpmarch=aarch64
	machine=aarch64
	guestarch=arm64
	;;
*)
	echo "unsupported architecture: $goarch" >&2
	exit 1
	;;
esac

tarball="$dist/boks_${version}_linux_${goarch}.tar.gz"
[ -f "$tarball" ] || {
	echo "missing $tarball; run scripts/build-release.sh linux $goarch first" >&2
	exit 1
}

assets="${BOKS_RUNTIME_ASSETS:-}"
if [ -z "$assets" ]; then
	cat >&2 <<'EOF'
BOKS_RUNTIME_ASSETS is not set.

These packages carry their own runtime — containerd, containerd-shim-nerdbox-v1 and
libkrun.so — because no distribution ships a set that works: Boks needs containerd 2.3 or
later and Ubuntu 24.04 LTS has 1.7.x, nerdbox is packaged nowhere at all, and libkrun by no
distribution Boks targets. Building the package without them would produce something that
installs cleanly, passes no check in `boks doctor`, and cannot start a sandbox.

Point BOKS_RUNTIME_ASSETS at a directory holding, for this architecture:

  containerd                    packaging/containerd-linux/build.sh <goarch> <dir>
  containerd-shim-nerdbox-v1  } artifact boks-runtime-linux-<goarch>
  libkrun.so                  } from .github/workflows/linux-runtime.yml

and, when they exist, nerdbox-kernel-<arch> and nerdbox-rootfs-<arch>.erofs from the
guest-image workflow.

To build the CLI-only package on purpose, set BOKS_RUNTIME_ASSETS=none.
EOF
	exit 1
fi

# --- the runtime assets --------------------------------------------------------------
# Resolved and checked before anything is staged, so a missing or wrong-architecture file
# fails with one message naming all of them rather than partway through two package builds.
bundled=0
runtime_files=()
if [ "$assets" != none ]; then
	[ -d "$assets" ] || {
		echo "BOKS_RUNTIME_ASSETS=$assets is not a directory" >&2
		exit 1
	}
	bundled=1

	required=(containerd containerd-shim-nerdbox-v1 libkrun.so)
	missing=()
	for name in "${required[@]}"; do
		[ -f "$assets/$name" ] || missing+=("$name")
	done
	if [ "${#missing[@]}" -gt 0 ]; then
		echo "BOKS_RUNTIME_ASSETS=$assets is missing ${#missing[@]} required file(s):" >&2
		for name in "${missing[@]}"; do echo "  $name" >&2; done
		echo >&2
		echo "A package without these installs cleanly and cannot start a sandbox." >&2
		echo "containerd comes from packaging/containerd-linux/build.sh $goarch <dir>;" >&2
		echo "the shim and libkrun.so from the boks-runtime-linux-$goarch artifact." >&2
		exit 1
	fi

	# The architecture of every binary, read from e_machine rather than trusted from the
	# directory's name. Copying an amd64 file into the arm64 bundle is precisely the
	# mistake that is invisible until a user runs it.
	assert="$root/packaging/linux/assert-elf.py"
	python3 "$assert" "$assets/containerd" --machine "$machine"
	python3 "$assert" "$assets/containerd-shim-nerdbox-v1" --machine "$machine"
	python3 "$assert" "$assets/libkrun.so" --machine "$machine" --type dyn

	# And that libkrun exports what the shim binds. nerdbox resolves all nineteen eagerly
	# at dlopen, so a library missing one fails the load and no VM starts at all — and a
	# libkrun built from the Windows pin is missing four while being the right
	# architecture, which is the case the check above cannot see.
	mapfile -t symbols < <(grep -v '^[[:space:]]*#' "$root/packaging/linux/NERDBOX_SYMBOLS" | grep -v '^[[:space:]]*$')
	python3 "$assert" "$assets/libkrun.so" --require "${symbols[@]}"

	runtime_files=(containerd containerd-shim-nerdbox-v1 libkrun.so)
fi

# The guest kernel and rootfs, which are optional today for a reason that is not technical:
# the kernel is GPL-2.0 and nerdbox patches it, so publishing the compiled result carries a
# corresponding-source obligation that has not been settled (docs/distribution.md, Part 3).
# Until it is, the packages ship without them, `boks doctor` reports `guest image fail`, and
# the description says so rather than letting a user find out at the first `boks run`.
guest_kernel=""
guest_rootfs=""
if [ "$bundled" = 1 ]; then
	[ -f "$assets/nerdbox-kernel-$guestarch" ] && guest_kernel="nerdbox-kernel-$guestarch"
	for candidate in "nerdbox-rootfs-$guestarch.erofs" "nerdbox-rootfs.erofs"; do
		if [ -f "$assets/$candidate" ]; then
			guest_rootfs="$candidate"
			break
		fi
	done
fi

if [ -z "${SOURCE_DATE_EPOCH:-}" ]; then
	SOURCE_DATE_EPOCH="$(git -C "$root" log -1 --format=%ct 2>/dev/null || echo 0)"
fi
export SOURCE_DATE_EPOCH

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

tar -xzf "$tarball" -C "$work"
extracted="$work/boks_${version}_linux_${goarch}"

# The completion scripts are architecture-independent — they are text the CLI prints — so
# they are generated once with a binary built for whatever host is doing the packaging,
# and shipped in every package. Generating them from the *target* binary would mean being
# able to execute it, which cross-building deliberately does not require.
completions="$work/completions"
mkdir -p "$completions"
gen="$work/boks-host"
CGO_ENABLED=0 go build -trimpath -o "$gen" "$root/cmd/boks"
"$gen" completion bash >"$completions/boks"
"$gen" completion zsh >"$completions/_boks"
"$gen" completion fish >"$completions/boks.fish"

# --- the shared payload -------------------------------------------------------------
# One tree, installed identically by both package formats, so a bug in the layout is a bug
# in one place.
stage_payload() {
	local dest="$1"
	install -Dm0755 "$extracted/boks" "$dest/usr/bin/boks"
	install -Dm0644 "$extracted/LICENSE" "$dest/usr/share/doc/boks/copyright"
	install -Dm0644 "$extracted/README.md" "$dest/usr/share/doc/boks/README.md"
	install -Dm0644 "$completions/boks" "$dest/usr/share/bash-completion/completions/boks"
	install -Dm0644 "$completions/_boks" "$dest/usr/share/zsh/vendor-completions/_boks"
	install -Dm0644 "$completions/boks.fish" "$dest/usr/share/fish/vendor_completions.d/boks.fish"

	# The runtime. containerd and the shim are executed, so 0755; libkrun.so and the guest
	# images are opened, so 0644 — the shim dlopens the library by the full path it built
	# itself, which needs no execute bit and no ldconfig entry. Deliberately NOT registering
	# it with the dynamic linker is the point: nothing but nerdbox should ever load it.
	[ "$bundled" = 1 ] || return 0
	install -Dm0755 "$assets/containerd" "$dest/usr/libexec/boks/containerd"
	install -Dm0755 "$assets/containerd-shim-nerdbox-v1" "$dest/usr/libexec/boks/containerd-shim-nerdbox-v1"
	install -Dm0644 "$assets/libkrun.so" "$dest/usr/libexec/boks/libkrun.so"
	[ -n "$guest_kernel" ] &&
		install -Dm0644 "$assets/$guest_kernel" "$dest/usr/libexec/boks/nerdbox-kernel-$guestarch"
	# Installed under the arch-suffixed name whichever spelling arrived: that is the name
	# the shim stats first, and it is unambiguous in a directory shared between arches.
	[ -n "$guest_rootfs" ] &&
		install -Dm0644 "$assets/$guest_rootfs" "$dest/usr/libexec/boks/nerdbox-rootfs-$guestarch.erofs"
	return 0
}

# --- the description, which has to stay true ----------------------------------------
# Written once and reflowed for each format, because two copies of a paragraph explaining
# what is in the package is how one of them ends up describing a package that no longer
# exists. That is not hypothetical: until this commit both descriptions said the packages
# contained "the boks CLI and nothing else", named containerd 2.2 as the floor when the
# measured floor is 2.3, and called Linux untested end to end after it had been verified.
describe() {
	cat <<EOF
Boks runs coding agents and other untrusted developer tooling inside a microVM
per sandbox, on your own machine. No account, no cloud service, no telemetry.
EOF
	if [ "$bundled" = 1 ]; then
		cat <<EOF

This package carries its own runtime, in /usr/libexec/boks: the containerd Boks
drives, the containerd-shim-nerdbox-v1 runtime shim, and libkrun.so. They are
private files that boks locates and hands to containerd, not commands on your
PATH, so nothing here shadows or conflicts with a containerd you installed
yourself. They are vendored rather than depended on because no distribution
ships a set that works: Boks needs containerd 2.3 or later, Ubuntu 24.04 LTS
has 1.7.x and 26.04 has 2.2.2, and a 2.2 daemon fails at task start with
"unsupported protocol: Yunix" — protobuf framing rendered as letters, naming
neither version. "Depends: containerd" would produce a machine that looks
provisioned and cannot start a sandbox. nerdbox is packaged by no distribution
at all, and libkrun by none that Boks targets.
EOF
		if [ -n "$guest_kernel" ] && [ -n "$guest_rootfs" ]; then
			cat <<EOF

The guest kernel and root filesystem the microVM boots are included.
EOF
		else
			cat <<EOF

The guest kernel and root filesystem the microVM boots are NOT included, so
"boks doctor" will report "guest image fail" after installing and a sandbox
will not start until they are in place. Get them from the guest-image workflow
at https://github.com/dagsommer/boks/actions/workflows/guest-image.yml, or
build them with scripts/build-nerdbox-guest.sh, and put both in
/usr/libexec/boks. "boks doctor" prints the names it looks for.
EOF
		fi
		cat <<EOF

Nothing here is started by installing it. There is no service, no unit and no
boot hook: containerd runs only between "boks daemon start" and "boks daemon
stop", as your own user, with its own root and its own socket, so a containerd
you already run for Docker or Kubernetes is neither used nor disturbed.
EOF
	else
		cat <<EOF

This package contains the boks CLI only — it was built with
BOKS_RUNTIME_ASSETS=none and carries no runtime. Starting a sandbox also needs
containerd 2.3 or later, the containerd-shim-nerdbox-v1 runtime shim, libkrun
1.18 or later, and the nerdbox guest kernel and root filesystem, none of which
are here and none of which any distribution packages. The released packages
carry them in /usr/libexec/boks; this one does not.
EOF
	fi
	cat <<EOF

mkfs.erofs is the one piece deliberately not vendored: erofs-utils is properly
packaged everywhere, so it is a Recommends. It has to be 1.8 or later, and
Ubuntu 24.04 LTS ships 1.7.1 — too old, and not refused up front: an image
pull fails partway through while a layer is unpacked. Run "boks doctor" after
installing. It checks every piece, including that version, and prints what to
do about each gap.
EOF
}

# --- .deb ---------------------------------------------------------------------------
deb_root="$work/deb"
stage_payload "$deb_root"
mkdir -p "$deb_root/DEBIAN"

# Installed-Size in KiB, per Debian policy 5.6.20, computed rather than guessed — it is what
# apt shows before asking to proceed, and a package that vendors a runtime owes the user
# that number.
installed_size="$(du -ks --exclude=DEBIAN "$deb_root" | cut -f1)"

# There is deliberately no postinst, no prerm and no postrm. Boks' promise is that a host
# which has not run `boks daemon start` runs no Boks process at all, and a maintainer script
# is the standard place that promise gets quietly broken — a systemd unit, an ldconfig
# trigger, a "helpful" first-run. Nothing needs one: libkrun.so is dlopened by full path so
# no ldconfig entry is wanted, the state directory is created by the CLI on demand, and there
# is nothing to clean up on removal that dpkg does not remove itself.
#
# There is also no Depends:, and that IS a gap rather than a decision. boks and containerd are
# built CGO_ENABLED=0 and link nothing; the nerdbox shim and libkrun.so are dynamically linked
# against glibc, so a correct package would carry the versioned `libc6 (>= …)` that
# dpkg-shlibdeps derives. shlibdeps resolves against libraries installed on the *build* host,
# which a cross-built package does not have, so it cannot simply be called here. The rpm gets
# this for free — rpm's ELF dependency generator reads the version requirements out of the
# binary, and `rpm -qp --requires` on the result lists them — and the deb does not. Recorded
# in packaging/apt/README.md rather than papered over with a guessed version.
{
	echo "Package: boks"
	echo "Version: ${version}"
	echo "Architecture: ${goarch}"
	echo "Maintainer: Dag Sommer <dagsommer@users.noreply.github.com>"
	echo "Section: devel"
	echo "Priority: optional"
	echo "Homepage: https://github.com/dagsommer/boks"
	echo "Installed-Size: ${installed_size}"
	echo "Recommends: erofs-utils"
	echo "Description: run coding agents in isolated microVMs, locally"
	# Continuation lines are indented one space; an empty line is " .".
	describe | sed -e 's/^$/./' -e 's/^/ /'
} >"$deb_root/DEBIAN/control"

# md5sums, so that `dpkg -V boks` verifies something. Without it the command succeeds while
# checking nothing, which is the worst answer an integrity check can give. Paths are relative
# to /, sorted, and DEBIAN/ is excluded — dpkg's own format.
(cd "$deb_root" && find . -path ./DEBIAN -prune -o -type f -print |
	sed 's|^\./||' | LC_ALL=C sort | xargs -r md5sum) >"$deb_root/DEBIAN/md5sums"

# --root-owner-group so the package does not depend on the uid that built it, and a fixed
# mtime so two builds of one commit produce identical bytes.
dpkg-deb --root-owner-group --build "$deb_root" \
	"$dist/boks_${version}_${goarch}.deb" >/dev/null

# --- .rpm ---------------------------------------------------------------------------
rpm_root="$work/rpmbuild"
mkdir -p "$rpm_root"/{BUILD,RPMS,SOURCES,SPECS,BUILDROOT}
stage_payload "$work/rpmpayload"

# The %files list is generated from the staged tree rather than written out by hand, because
# the tree's contents now depend on what CI handed over — with and without the guest images
# are two different file lists, and rpmbuild fails on a spec that names a file it cannot find
# just as loudly as on one that ships a file it did not name.
files_list="$rpm_root/SOURCES/files"
{
	echo "%license /usr/share/doc/boks/copyright"
	echo "/usr/bin/boks"
	echo "/usr/share/doc/boks/README.md"
	echo "/usr/share/bash-completion/completions/boks"
	echo "/usr/share/zsh/vendor-completions/_boks"
	echo "/usr/share/fish/vendor_completions.d/boks.fish"
	if [ "$bundled" = 1 ]; then
		echo "%dir /usr/libexec/boks"
		for name in "${runtime_files[@]}"; do echo "/usr/libexec/boks/$name"; done
		[ -n "$guest_kernel" ] && echo "/usr/libexec/boks/nerdbox-kernel-$guestarch"
		[ -n "$guest_rootfs" ] && echo "/usr/libexec/boks/nerdbox-rootfs-$guestarch.erofs"
	fi
} >"$files_list"

{
	cat <<EOF
# Generated by scripts/package-linux.sh. The binaries are built elsewhere — boks by
# build-release.sh, containerd by packaging/containerd-linux/build.sh, the shim and libkrun
# by .github/workflows/linux-runtime.yml — and installed here as-is: there is no %build
# stage, because cross-compiling and building an rpm are two jobs and mixing them would mean
# an rpmbuild per architecture.
%global debug_package %{nil}
%global _build_id_links none
%global __strip /bin/true

# /usr/libexec/boks holds Boks' private runtime, and rpm's automatic dependency generator
# would otherwise advertise libkrun.so to the rest of the system as though any package could
# link against it. Nothing may satisfy a dependency with it: it is dlopened by full path by
# one program. Requires are left switched on — libkrun.so is the only dynamically linked file
# in the package and its glibc requirement is real.
%global __provides_exclude_from ^/usr/libexec/boks/.*\$

Name:           boks
Version:        ${version}
Release:        1
Summary:        Run coding agents in isolated microVMs, locally
License:        Apache-2.0
URL:            https://github.com/dagsommer/boks
Recommends:     erofs-utils

%description
EOF
	describe
	cat <<'EOF'

%install
cp -a %{_sourcedir}/payload/. %{buildroot}/

%files -f %{_sourcedir}/files

%changelog
EOF
} >"$rpm_root/SPECS/boks.spec"

cp -a "$work/rpmpayload" "$rpm_root/SOURCES/payload"
# --target rather than a BuildArch line in the spec: BuildArch is checked against the build
# host's own architecture, so an aarch64 spec cannot be built on an x86_64 runner and vice
# versa, which is precisely what cross-building a Go binary asks for. --target sets the
# package's architecture without that check.
rpmbuild -bb \
	--define "_topdir $rpm_root" \
	--define "_sourcedir $rpm_root/SOURCES" \
	--target "$rpmarch" \
	"$rpm_root/SPECS/boks.spec" >"$work/rpmbuild.log" 2>&1 ||
	{
		cat "$work/rpmbuild.log" >&2
		exit 1
	}
cp "$rpm_root/RPMS/$rpmarch/boks-${version}-1.${rpmarch}.rpm" "$dist/"

(cd "$dist" && sha256sum "boks_${version}_${goarch}.deb" >"boks_${version}_${goarch}.deb.sha256")
(cd "$dist" && sha256sum "boks-${version}-1.${rpmarch}.rpm" >"boks-${version}-1.${rpmarch}.rpm.sha256")

echo "built $dist/boks_${version}_${goarch}.deb"
echo "built $dist/boks-${version}-1.${rpmarch}.rpm"
if [ "$bundled" = 1 ]; then
	echo "  runtime: $(printf '%s ' "${runtime_files[@]}")${guest_kernel:+nerdbox-kernel-$guestarch }${guest_rootfs:+nerdbox-rootfs-$guestarch.erofs}"
	if [ -z "$guest_kernel" ] || [ -z "$guest_rootfs" ]; then
		echo "  NOTE: no guest image — 'boks doctor' will report 'guest image fail'" >&2
	fi
else
	echo "  runtime: none (BOKS_RUNTIME_ASSETS=none) — this package cannot start a sandbox" >&2
fi
