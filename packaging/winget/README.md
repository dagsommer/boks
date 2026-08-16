# winget

The manifests that put Boks in `winget install boks`, and the part of getting them there
that a repository cannot do for itself.

> [!IMPORTANT]
> **None of this has been run.** Nobody on this project has a Windows machine to hand while
> writing it, `winget validate` runs only on Windows, and no manifest here has been
> submitted anywhere. What *has* been run is the schema validation described under
> [What has actually been checked](#what-has-actually-been-checked) — on Linux, against
> winget's own published JSON schemas, with a negative control. That is a document check and
> not an install. Treat the first `winget install boks` as the test, and expect it to fail
> the first time.

## What is here

| Path | What it is |
|---|---|
| `manifests/dagsommer.boks.yaml.in` | version manifest template |
| `manifests/dagsommer.boks.installer.yaml.in` | installer manifest template |
| `manifests/dagsommer.boks.locale.en-US.yaml.in` | default-locale manifest template |
| `render.sh` | stamps a version, a SHA-256 and a date into the templates |
| `validate.py` | checks the rendered files against winget's JSON schemas |

The templates are templates rather than files-with-a-placeholder-you-edit because a release
must not depend on anyone remembering three substitutions. `{{VERSION}}`,
`{{INSTALLER_SHA256}}` and `{{RELEASE_DATE}}` are the only things a release changes, they
appear nowhere else, and `render.sh` refuses to emit a file that still contains one.

> That last clause became true on 2026-08-16. The guard matched `{{[A-Z_]*}}` — a character
> class with no digits in it — so it could not match `{{INSTALLER_SHA256}}`, the very
> placeholder it is named for, nor any later `{{GUEST_REV_X86_64}}` or `{{SHA256_ARM64}}`. A
> manifest carrying a literal placeholder rendered and the script exited 0. It now matches
> anything between doubled braces, and `.github/workflows/winget.yml` runs a control that
> puts one back to prove the guard still fires.

Template lines beginning `#|` are notes to ourselves and are **stripped at render**, so what
lands in a pull request is a manifest and not our reasoning about it. The rendered files keep
two comment lines: a provenance line and the `yaml-language-server` schema line, which is the
same shape every manifest merged into winget-pkgs carries.

```sh
packaging/winget/render.sh 0.1.0 dist/boks_0.1.0_windows_amd64.zip
packaging/winget/render.sh 0.1.0 A1B2…64-hex-digits…C3D4        # or the digest directly
```

Output lands in `dist/winget/manifests/d/dagsommer/boks/<version>/`, which is the exact path
`microsoft/winget-pkgs` requires, so the rendered tree can be copied over a winget-pkgs
checkout wholesale rather than reassembled by hand.

## The schema version, and why it is not the newest one

`ManifestVersion: 1.12.0`.

This is deliberately *not* the newest schema Microsoft publishes, and picking the newest is
the obvious mistake. The schemas live in **`microsoft/winget-cli`**, not in winget-pkgs
(`winget-pkgs/schemas/JSON/manifests` does not exist and returns 404 — that repository
carries only a validation schema). As of 2026-08-15 winget-cli has frozen schema directories
up to `v1.28.0` and a `latest/` whose `$id` reads `1.30.0`.

The community repository accepts less than that, and says so:

- `winget-pkgs/.github/PULL_REQUEST_TEMPLATE.md` asks contributors to confirm the manifest
  conforms to the **1.12** schema;
- `winget-pkgs/.github/copilot-instructions.md`: *"Recommended schema version: 1.12.0 (1.10.0
  also accepted)"*;
- `winget-pkgs/Tools/YamlCreate.ps1` sets `$ManifestVersion = '1.12.0'`;
- every manifest merged into winget-pkgs on 2026-08-15 that we sampled carries `1.12.0`;
- `winget-pkgs/doc/manifest/README.md` states outright that the community repository *"will
  often delay support for new schema versions until enough devices have been updated"*.

So the rule is: **track what winget-pkgs' own tooling emits, not what winget-cli's `latest/`
contains.** When that changes, three files change together — both `.yaml.in` headers, the
`ManifestVersion:` lines, and `MANIFEST_VERSION` in `validate.py`.

## The installer shape, and the assumption it rests on

`InstallerType: zip` with `NestedInstallerType: portable`.

Boks is a CLI, so `portable` is right. It is a zip rather than a bare portable exe because
the Windows artifact is a **bundle**: Boks orchestrates containerd, the nerdbox shim,
`krun.dll`, `mkfs.erofs.exe` and the guest kernel and root filesystem, and none of those is
packaged anywhere on Windows. A lone `boks.exe` would install a binary that cannot start a
sandbox — the thing `docs/install.md` spends its length warning about.

**`release.yml` now builds exactly that archive.** When this file was first written it did not,
and the manifest named an asset that would have 404'd on the first tag. The `assemble` job
produces `boks_<version>_windows_amd64.zip` as a single top-level directory
`boks_<version>_windows_amd64/` holding `boks.exe`, the runtime beside it, and the guest kernel
and rootfs — and it **asserts that `boks.exe` is at that path** before archiving, so the
contract between this manifest and that workflow fails in our CI rather than in someone else's
repository.

> [!WARNING]
> **The assertion is the only thing holding the two files together, and it is one-directional.**
> It catches `release.yml` laying the archive out differently. Nothing catches someone editing
> `RelativeFilePath` here to a path the workflow does not produce. If the layout has to change,
> the two lines are `InstallerUrl` and `RelativeFilePath` in
> `manifests/dagsommer.boks.installer.yaml.in`, and the check in `release.yml`'s `assemble` job
> has to move with them. Nothing else in this directory depends on the layout.

Why the runtime sitting beside `boks.exe` is the right layout and not a convenience:
`internal/daemon/locate.go` resolves the running executable, follows symlinks, and searches
the directory it lands in — the "tarball, everything side by side" case named in its own
comment. So an extracted bundle needs no configuration, no environment variable and no PATH
entry for Boks to find its own containerd, shim and guest images.

### `ArchiveBinariesDependOnPath` is deliberately absent

Setting it `true` tells winget to put the whole extracted directory on the user's `PATH`.
That would place `containerd.exe` and `ctr.exe` there too, shadowing a containerd the user
installed on purpose — the exact harm `docs/distribution.md` gives as the reason a `.deb`
should put the runtime in `/usr/libexec/boks/` rather than on `PATH`.

Leaving it unset makes winget create one symlink named `boks` in its own links directory,
which is already on `PATH`, and leave everything else invisible. Read from
`winget-cli/src/AppInstallerCLICore/PortableInstaller.cpp`, that path is safe unelevated:
when `CreateSymlink` fails, winget falls back to adding the package directory to `PATH`
rather than failing the install. **The consequence worth naming: on a machine where symlink
creation fails, the fallback puts the bundle directory on `PATH` and the shadowing above
happens anyway.** If that turns out to matter, the fix is small and already supported at the
other end — lay the archive out as `bin/boks.exe` beside `libexec/boks/`, which `locate.go`
already searches as `<exe dir>/../libexec/boks`. Three lines move together: `RelativeFilePath`
here, the layout in `release.yml`'s `assemble` job, and the assertion in it that checks the
path. The flat layout is what ships because it is what this manifest already declared, and
because the shadowing needs a symlink failure first.

### Other choices

- **x64 only.** There is no Windows arm64 build: libkrun's WHP backend here is x86_64 and so
  is the guest kernel that has booted under it.
- **`MinimumOSVersion: 10.0.22000.0`** — Windows 11 RTM. The Windows Hypervisor Platform is
  older than that, but Windows 11 is the only thing this stack has ever run on, so the floor
  stated is the floor observed.
- **No `Copyright`.** The repository's `LICENSE` is the plain Apache-2.0 text with no
  copyright line in it. The field is optional and inventing a value would assert something
  no file in this project asserts.
- **No `Icons`.** winget-pkgs populates that field in its own validation pipeline; it is not
  authorable in a pull request.
- **No `InstallModes`, `UpgradeBehavior` or `InstallationMetadata`.** None is meaningful for
  an archive of portables, and the merged manifests we modelled this on omit all three.

## What has actually been checked

`validate.py` runs on Linux and does two things winget itself would do on Windows, and
stops well short of the rest.

**1. Schema validation.** winget's manifest schemas are ordinary draft-07 JSON Schema, so the
same documents `winget validate` checks against can be checked against here. Run on
2026-08-15 against `v1.12.0` of all three schemas, all three rendered manifests passed.

**2. The conditional rules the schema cannot express.** The installer schema contains no
`if`/`then`/`allOf`, so a generic JSON-schema validator cannot see any of winget-pkgs'
cross-field rules. Four are checked by hand in `cross_file_problems`, each quoting the
documentation it comes from: that a `zip` needs a `NestedInstallerType` and
`NestedInstallerFiles`; that more than one nested file is allowed only for `portable`; that
`PortableCommandAlias` is valid only for `portable`; and that the three files agree on
identifier, version and manifest version.

Both were exercised with a **negative control** — a copy of the rendered manifests mutated to
`InstallerType: tarball`, `NestedInstallerType: exe` and two nested files — and the validator
reported all three faults and exited non-zero. A validator that passes everything proves
nothing, which is why that run happened.

**3. That all three manifests are there, and that they declare the schema version actually
fetched.** Both added 2026-08-16, and both were holes rather than omissions:

- a *missing* manifest was a silent pass. Only an entirely empty directory failed, so a
  render that produced one file out of three reported `ok` — and every cross-file rule above
  is vacuously satisfied over a set of one. Reproduced by deleting
  `dagsommer.boks.installer.yaml` from a good render: exit 0.
- `ManifestVersion` was compared only *across* the three documents. One `sed` pass stamps all
  three from templates carrying the same literal, so that comparison cannot fail — while
  `MANIFEST_VERSION` in `validate.py`, the constant the schema URL is built from, was never
  compared to them at all. Reproduced by rewriting all three to `1.9.0`: the validator
  fetched the 1.12.0 schemas, printed `ok against manifest.installer.1.12.0`, and exited 0.

**Where these run.** `.github/workflows/winget.yml` renders and validates on every change
under `packaging/winget/`, with a negative control for each defect above, and sets
`BOKS_WINGET_REQUIRE_VALIDATION=1` so that render.sh's graceful "jsonschema not available,
skipped schema validation" becomes a failure rather than a green run that checked nothing.
`make winget` does the same render locally. Before that, nothing ran either script: they were
documented here and in `docs/distribution.md` and invoked by no workflow, no Makefile target
and no test, so the first thing to exercise them would have been a release.

**What a pass does not mean.** It does not mean winget will install the package. It says
nothing about whether the installer URL resolves, whether the SHA-256 matches the archive at
that URL, whether `boks.exe` is at that path inside the archive, whether the symlink is
created, whether `boks --version` runs afterwards, or whether winget-pkgs' pipeline accepts
the submission. Those are runtime facts and this is a document check.

## Submitting to microsoft/winget-pkgs

This is a pull request to somebody else's repository, reviewed by people who are not us. No
amount of work in this directory removes the human steps, and pretending otherwise is how a
release plan acquires a step nobody owns.

### What a human has to do, in order

1. **Sign Microsoft's CLA.** Tracked separately from validation; a `Needs-CLA` label blocks
   the merge regardless of whether everything else passed.
2. **Cut the release first.** The manifest names an `InstallerUrl` and an `InstallerSha256`
   for an asset that must already be downloadable — the pipeline re-downloads the archive and
   compares the hash. A draft release is not publicly downloadable and will fail.
3. **Render, and check the digest against the release's `SHA256SUMS`.** `render.sh` computes
   it from a local file if you give it one, which is the transcription step worth removing.
4. **Fork `microsoft/winget-pkgs`**, copy the rendered `manifests/d/dagsommer/boks/<version>/`
   directory into the fork at that same path, and open one pull request.
   - **One package version per pull request.** Two versions in one PR is rejected.
   - **Manifest files only.** No README, tooling or spelling changes mixed in.
   - Multi-file only; `ManifestType: singleton` is deprecated and rejected.
5. **Wait for the validation checks.** They run as a GitHub App posting check runs — the
   Azure DevOps pipeline this used to be is gone, though the `Azure-Pipeline-Passed` label
   name survives it. Ten steps, of which the ones this package can plausibly trip are listed
   below. A moderator can re-trigger them by commenting `@wingetbot run`.
6. **Wait for a human moderator.** Every pull request needs one, not just new packages;
   approval applies `Moderator-Approved` and the merge then happens automatically. Infrequent
   contributors get more scrutiny, and a first-ever package from an unknown publisher is as
   infrequent as it gets. If it sits more than a day, the documented remedy is to ping a
   moderator.
7. **Then, and only then, automate the next one.** `vedantmgoyal9/winget-releaser` (which
   drives Komac) updates an existing package from a release: it needs a **classic** PAT with
   `public_repo` scope — fine-grained tokens are not supported — and a fork of winget-pkgs
   under the same account. **It cannot bootstrap the first submission**, because it derives
   the new manifests from a previous version that has to already be in the repository. So
   step 4 is a human step exactly once.

### What we should expect to go wrong

- **URL reputation, not code signing.** winget-pkgs requires no signature for a zip; only
  MSIX must be signed. But the URL validation step fails a URL that Microsoft Defender
  SmartScreen reports as **low reputation**, which a brand-new unsigned release is a
  candidate for, and the installer scan step runs multiple AV engines over the archive.
  Policy on that is categorical: a package flagged by any security scan cannot be accepted
  *"regardless of the application's legitimacy or intent"*. The mitigations are to host on
  GitHub Releases rather than a new domain — which we do — and, if it trips, to submit the
  URL to Microsoft at `microsoft.com/wdsi/filesubmission/`.
- **Provenance.** The `InstallerUrl` must be the publisher's own release location; download
  aggregators, mirrors and URL shorteners are refused, and redirects are resolved to the
  final URL. A GitHub release asset on `dagsommer/boks` satisfies this. `PackageUrl` is set
  so a moderator can confirm it in one click.
- **A package this unusual invites questions.** Boks installs a hypervisor library and starts
  a daemon. Nothing in winget's policies prohibits that, but it is worth having
  `docs/verification.md` and `docs/security-model.md` ready to link, which is why they are in
  `Documentations`.

Ownership is *not* a problem: winget-pkgs is a community repository and third parties submit
packages they do not own routinely. What it enforces is the provenance of the URL, not the
identity of the submitter.

## What is still missing before any of this can be submitted

Not opinions — things that do not exist:

1. **A tag.** `release.yml` assembles `boks_<version>_windows_amd64.zip` and the four items
   that used to be on this list are all in it — the CLI, `krun.dll`, the Windows containerd
   bundle and the nerdbox shim, and the guest kernel and root filesystem. But no tag has ever
   run that workflow, so there is no `InstallerUrl` that resolves and no digest to compute.
   Step 2 of the sequence above is not a formality: the submission pipeline re-downloads the
   archive and compares its hash, and a draft release is not publicly downloadable.
2. **`mkfs.ext4` for Windows**, which has no build anywhere. containerd wants it for a
   container's writable layer; the archive ships a pre-formatted 64 MiB image instead, which
   is a workaround and is labelled as one in `docs/install.md`.

Item 2 is not this directory's to solve and is tracked in `docs/distribution.md`.

**The guest is in the archive**, which is what makes a `winget install` boot a sandbox with no
further download and is why this package is worth submitting at all. The kernel is GPL-2.0, so
the archive carries a `SOURCE.txt` naming the exact nerdbox revision and the commands that
reproduce it; the reasoning, including what that does and does not settle, is in
`docs/distribution.md` under "What goes in the Windows archive, and the guest".
